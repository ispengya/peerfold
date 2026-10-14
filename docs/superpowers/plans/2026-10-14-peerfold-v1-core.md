# Peerfold v1 核心 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 N 台机器上的文件在 server 那里聚合成一棵全局索引树，并支持按需读取、写回原机、以及原机变化实时反映到索引——这一整条链路可测、可用，但还没有内核挂载点。

**Architecture:** 星形拓扑。client 出站拨号连 server 并上传自己的目录树；server 持有唯一的全局索引，并把读/写请求路由回归属的 client。另有一条 `viewer` 连接角色，让 `peerfold ls/cat/put` 这类命令（以及下一份计划里的挂载层）直接向 server 查询和操作这棵树。client 之间永不互连。

**Tech Stack:** Go（当前稳定版）；`github.com/fsnotify/fsnotify` 做文件变化监听；标准库 `net`、`encoding/json`、`encoding/binary`、`os`。不引入其他依赖。

**Spec:** `docs/superpowers/specs/2026-10-14-peerfold-design.md`

## Global Constraints

- 模块名 `peerfold`；所有内部包放在 `internal/` 下。
- 平台范围：**Linux + Windows**。macOS 推后。代码不得依赖只有某个平台才有的 API（用 `fsnotify` 而不是 `inotify` 直调）。
- **v1 不做任何认证、授权、传输加密。** 不写 token、不写 TLS、不做权限校验。
- **索引里只装活着的 client。** client 断连 → 立刻 `RemoveClient`；重连 → 重建。
- **保存是同步的**：写操作的返回值代表字节已经落到供货方磁盘。不做观察者侧暂存队列、不做重试队列。
- **整文件回写**，不做分页、不做增量。改一个字节也传整个文件。
- **不做内容缓存**：读路径每次向归属 client 拉。
- **不做忽略规则**：共享目录里有什么就照实进索引，包括临时文件。
- 写操作支持：**新建文件、新建目录、覆盖写（含截断）、重命名、删除（文件与目录）、同一个 client 树内移动**。
- 写操作不支持：跨机器 `mv`（返回 `EXDEV` 语义的错误）、`chmod`/`chown`/硬链接、写 `mmap`。
- 规模目标：≤ 10 台 client，单 client ≤ 100 万文件。列表接口必须能分批、不能要求一次装进内存。
- client 名默认取主机名，由 server 校验唯一；撞名时拒绝该 client 并报错。
- **单个 `--aggregate` 只允许一台**，第二台被拒时**降级成普通供货方继续跑**，只记日志，不退出。
- 索引主键是**大小写敏感的 UTF-8 字符串**，不做归一化。
- 每个任务结束都要 commit（`git init` 在 Task 1 里做）。

## Review Focus

这五类输入 spec 隐含要求、但没有哪个任务的测试天然覆盖；每一条都在它所属任务的步骤里加了对应的测试。

1. **列举之后、读之前文件被删掉**：必须返回"不存在"，不能返回 0 字节的假文件——否则命中 spec 的"树说谎"。
2. **写回时供货方磁盘满或权限不足**：错误必须传到调用方、让这次操作失败；**不能**静默成功——命中"保存悄悄丢数据"。
3. **传输中途供货方掉线**：读和写都必须以错误收场并释放资源，不能挂住。
4. **观察者的挂载点路径落在共享目录内**：必须不进索引——否则套娃。
5. **零字节文件与空目录**：必须能列举、能读（返回 0 字节）、能写空内容，不能当作"不存在"。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `go.mod` | 模块定义、依赖 |
| `cmd/peerfold/main.go` | CLI 入口，子命令分发 |
| `cmd/peerfold/cmd_server.go` | `server` 子命令 |
| `cmd/peerfold/cmd_client.go` | `client` 子命令：解析参数、落盘配置、起 client |
| `cmd/peerfold/cmd_tree.go` | `ls`/`cat`/`put`/`mkdir`/`rm`/`mv` 六个 viewer 子命令 |
| `internal/config/config.go` | 配置结构与读写 |
| `internal/proto/frame.go` | 帧的编码/解码 |
| `internal/proto/conn.go` | 带请求/响应关联与数据流的连接封装 |
| `internal/proto/messages.go` | 消息 kind 常量与 Body 结构 |
| `internal/node/node.go` | 节点模型与路径工具 |
| `internal/client/walk.go` | 扫描共享目录，产出节点批次 |
| `internal/client/shares.go` | 共享名 → 绝对路径的映射与校验 |
| `internal/client/client.go` | 拨号、hello、上传树、服务读/写请求 |
| `internal/client/fsops.go` | 在本地磁盘上执行创建/写/改名/删除 |
| `internal/client/watch.go` | fsnotify 监听 → 增量事件 |
| `internal/index/index.go` | 全局索引：批量落、按 client / 按节点清、查、列 |
| `internal/server/server.go` | 监听、接受连接、按角色分流 |
| `internal/server/registry.go` | 在线 client 注册表、唯一名校验、单聚合门禁 |
| `internal/server/router.go` | 把读/写操作路由到归属 client 的连接 |
| `internal/server/query.go` | 列举与读写聚合树的查询接口（CLI 与将来的挂载层共用） |
| `internal/viewer/viewer.go` | viewer 侧客户端：连 server、调 query 接口 |
| `internal/e2e/e2e_test.go` | 端到端主流程验收 |

---

### Task 1: 模块骨架、配置、CLI 分发

**Files:**
- Create: `go.mod`, `cmd/peerfold/main.go`, `cmd/peerfold/cmd_server.go`, `cmd/peerfold/cmd_client.go`, `cmd/peerfold/cmd_tree.go`, `internal/config/config.go`
- Test: `internal/config/config_test.go`, `cmd/peerfold/main_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `config.Config` 结构体，字段：`Listen string`、`Server string`、`ClientName string`、`Shares map[string]string`、`Aggregate bool`
  - `config.Load(path string) (config.Config, error)`
  - `config.Save(path string, c config.Config) error`
  - `config.DefaultPath() (string, error)`
  - `config.Default() config.Config`
  - `main.run(args []string, stdout, stderr io.Writer) int`

- [ ] **Step 1: `git init` 并建模块**

```bash
git init
go mod init peerfold
go get github.com/fsnotify/fsnotify@latest
```

- [ ] **Step 2: 写失败测试 `internal/config/config_test.go`**

断言：
- `Default()` 的 `Listen == ":7000"`
- `Save` 后 `Load` 回来的 `Config` 与写进去的相等（`reflect.DeepEqual`）
- `Load` 一个不存在的路径返回的错误满足 `errors.Is(err, os.ErrNotExist)`
- `Shares` 为 `map[string]string`，`{"Documents": "/home/me/Documents"}` 能原样往返

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/config/ -run TestConfig -v`
Expected: FAIL，`undefined: config.Default`

- [ ] **Step 4: 实现 `internal/config/config.go`**

`Default()` 返回上面字段的默认值；`Load`/`Save` 用 `encoding/json`；`DefaultPath()` 返回 `os.UserConfigDir()` 下的 `peerfold/config.json`。

- [ ] **Step 5: 写失败测试 `cmd/peerfold/main_test.go`**

断言：
- `run([]string{"ls", "/nonexistent"}, &out, &errOut)` 返回非零
- `run([]string{"bogus"}, ...)` 返回非零，且 stderr 含 `unknown command`
- `run(nil, ...)` 返回非零，且 stdout 含 `server`、`client`

- [ ] **Step 6: 实现 `cmd/peerfold/main.go` 及子命令文件**

`main.go` 只做分发：`server`/`client` 走 `cmd_server.go`/`cmd_client.go`，`ls`/`cat`/`put`/`mkdir`/`rm`/`mv` 走 `cmd_tree.go`。本轮每个子命令只做到"接受参数、打印 TODO、返回 0"。

- [ ] **Step 7: 运行测试并确认通过**

Run: `go test ./... -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum cmd internal
git commit -m "feat: 模块骨架、配置读写与 CLI 分发"
```

---

### Task 2: 线上协议——帧与连接

**Files:**
- Create: `internal/proto/frame.go`, `internal/proto/conn.go`, `internal/proto/messages.go`
- Test: `internal/proto/frame_test.go`, `internal/proto/conn_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `proto.FrameKind`（`FrameControl`、`FrameData`）
  - `proto.WriteFrame(w io.Writer, kind FrameKind, payload []byte) error`
  - `proto.ReadFrame(r io.Reader) (FrameKind, []byte, error)`
  - `proto.MaxPayload = 1 << 20`
  - `proto.Envelope`：`ID uint64`、`Kind string`、`Body json.RawMessage`、`Err string`、`EOF bool`
  - **kind 常量一律是 `string`**：`proto.KindHello = "hello"`、`KindHelloOK = "hello_ok"`、`KindTreeBatch = "tree_batch"`、`KindTreeDone = "tree_done"`、`KindEvent = "event"`、`KindList = "list"`、`KindRead = "read"`、`KindWrite = "write"`、`KindMkdir = "mkdir"`、`KindCreate = "create"`、`KindRename = "rename"`、`KindRemove = "remove"`
  - `proto.RoleClient = "client"`、`proto.RoleViewer = "viewer"`
  - `proto.HelloBody{Name string; Role string; Shares []string; Aggregate bool}`
  - `proto.HelloOKBody{Name string; Aggregate bool}`
  - `proto.NodeJSON{Path string; Kind uint8; Size int64; Mode uint32; Mtime int64}`——字段类型与 `node.Node` 一一对应，供 `encoding/json` 直接往返
  - `proto.TreeDoneBody{Shares []string}`
  - `proto.ListBody{Client string; Path string}`
  - `proto.ListOKBody{Nodes []NodeJSON}`
  - `proto.ReadBody{Client string; Path string; Off int64; N int64}`
  - `proto.WriteBody{Client string; Path string; Truncate bool}`
  - `proto.MkdirBody{Client string; Path string}`
  - `proto.CreateBody{Client string; Path string}`
  - `proto.RenameBody{Client string; OldPath string; NewPath string}`
  - `proto.RemoveBody{Client string; Path string}`
  - `proto.EventBody{Nodes []NodeJSON}`——`Kind == 255` 表示删除
  - `proto.NewConn(rw io.ReadWriteCloser) *Conn`
  - `(*Conn) Send(env Envelope) error`
  - `(*Conn) Recv() (Envelope, error)`
  - `(*Conn) SendData(id uint64, chunk []byte) error`
  - `(*Conn) RecvData(id uint64) ([]byte, bool, error)`——第三个返回值表示流结束
  - `(*Conn) Close() error`

- [ ] **Step 1: 写失败测试 `internal/proto/frame_test.go`**

断言：
- 往 `bytes.Buffer` 写一个 `FrameControl` 帧再读回来，kind 和 payload 相等
- 读一个只有 2 字节的残帧返回 `io.ErrUnexpectedEOF`
- 帧头声明的长度 > `MaxPayload` 时返回错误，且错误信息含 `too large`

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/proto/ -run TestFrame -v`
Expected: FAIL，`undefined: proto.WriteFrame`

- [ ] **Step 3: 实现 `internal/proto/frame.go`**

帧格式：1 字节 kind + 4 字节大端 payload 长度 + payload。`ReadFrame` 用 `io.ReadFull` 读满头部。

- [ ] **Step 4: 写失败测试 `internal/proto/conn_test.go`**

用 `net.Pipe()` 成对连接，断言：
- A `Send` 一条 `KindHello`，B `Recv` 收到同 `ID`、同 `Kind`
- 连续 `SendData(id, chunk)` 三轮后 `SendData(id, nil)`，B 侧 `RecvData` 依次拿到三段，第三段之后一次 `RecvData` 的结束标志为真
- 两个 goroutine 同时对同一 `*Conn` 调 `Send`，两边都返回 nil，且 B 侧收到两条不交错的 envelope

- [ ] **Step 5: 实现 `internal/proto/conn.go` 与 `messages.go`**

`Conn` 用两把 `sync.Mutex` 分别保护读和写；`Send` 把 `Envelope` 用 `encoding/json` 序列化后走 `FrameControl`；数据块走 `FrameData`，其 payload 前 8 字节是大端 `ID`。**`Send` 与 `SendData` 共用同一把写锁**，否则上面第三条测试会交错。

- [ ] **Step 6: 运行测试并确认通过**

Run: `go test ./internal/proto/ -race -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/proto
git commit -m "feat: 线上协议帧与连接封装"
```

---

### Task 3: 节点模型与共享目录扫描

**Files:**
- Create: `internal/node/node.go`, `internal/client/walk.go`, `internal/client/shares.go`
- Test: `internal/node/node_test.go`, `internal/client/walk_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `node.Kind uint8`，取值 `KindFile = 0`、`KindDir = 1`（删除标记 `KindDeleted = 255` 在 Task 10 加入）
  - `node.Node{Path string; Kind node.Kind; Size int64; Mode uint32; Mtime int64}`——`Path` 是 `<share>/<rel>` 形式的斜杠分隔路径，`Mtime` 是 Unix 纳秒
  - `node.SplitFirst(p string) (head, rest string)`
  - `node.IsUnder(parent, child string) bool`——两个参数都是绝对路径，判断 `child` 是否在 `parent` 里或等于 `parent`
  - `client.Shares`（内部持 `map[string]string` 与 `[]string` exclude）
  - `client.NewShares(pairs []string, exclude []string) (client.Shares, error)`——`pairs` 形如 `NAME=/abs/path`
  - `client.Shares.Root(name string) (string, bool)`
  - `client.Shares.Names() []string`
  - `client.Shares.Excluded(abs string) bool`
  - `client.Scan(sh client.Shares, batch int, emit func([]node.Node) error) error`

- [ ] **Step 1: 写失败测试 `internal/node/node_test.go`**

断言：
- `SplitFirst("Documents/a/b.txt")` → `("Documents", "a/b.txt")`
- `SplitFirst("Documents")` → `("Documents", "")`
- `IsUnder("/mnt/peerfold", "/mnt/peerfold")` → true
- `IsUnder("/mnt/peerfold", "/mnt/peerfold/win-laptop/x")` → true
- `IsUnder("/mnt/peerfold", "/mnt/peerfoldX")` → **false**（前缀相同但不是子路径，这条最容易写错）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/node/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/node/node.go`**

`IsUnder` 用 `filepath.Rel` 判断，不要用字符串前缀比较。

- [ ] **Step 4: 写失败测试 `internal/client/walk_test.go`**

先 `t.TempDir()` 造树：`Documents/a.txt`（内容 `hello`）、`Documents/sub/b.txt`、`Documents/empty/`（空目录）、`Documents/zero.bin`（0 字节）；另造 `mnt/peerfold/loop.txt`。断言：
- `NewShares([]string{"Documents=<tmp>/Documents"}, nil)` 成功
- `Scan` 的 `batch=2` 时 `emit` 被调用多次，合起来正好含 `Documents`、`Documents/a.txt`、`Documents/sub`、`Documents/sub/b.txt`、`Documents/empty`、`Documents/zero.bin`（**空目录和 0 字节文件都必须在**，命中 Review Focus 第 5 条）
- `Documents/zero.bin` 的 `Size == 0`、`Kind == node.KindFile`
- `NewShares([]string{"Documents=<tmp>/Documents", "Loop=<tmp>/mnt/peerfold"}, []string{"<tmp>/mnt/peerfold"})` 时，`loop.txt` 不出现（**命中 Review Focus 第 4 条：观察者挂载点不进索引**）
- `NewShares([]string{"Bad=/does/not/exist"}, nil)` 返回错误
- `NewShares([]string{"A=/tmp", "A=/var"}, nil)` 返回错误（共享名重复）

- [ ] **Step 5: 实现 `internal/client/shares.go` 与 `internal/client/walk.go`**

`NewShares` 校验每个路径是存在的目录、共享名非空且不重复，并把 `exclude` 转成绝对路径存下。`Scan` 用 `filepath.WalkDir`，遇到 `Excluded` 为真的子树调 `filepath.SkipDir`，每个共享名产出自己的头部目录节点。

- [ ] **Step 6: 运行测试并确认通过**

Run: `go test ./internal/node/ ./internal/client/ -race -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/node internal/client
git commit -m "feat: 节点模型、共享目录校验与分批扫描"
```

---

### Task 4: 全局索引

**Files:**
- Create: `internal/index/index.go`
- Test: `internal/index/index_test.go`

**Interfaces:**
- Consumes: `node.Node`, `node.Kind`
- Produces:
  - `index.Index`（零值可用；内部 `sync.RWMutex`）
  - `(*Index) Apply(client string, nodes []node.Node)`——按 `<client>` + `node.Path` 落库，同键覆盖；**`Kind == node.KindDeleted` 的节点是删除指令，转掉 `Remove`**
  - `(*Index) Remove(client, path string)`——删掉该路径**及其整棵子树**
  - `(*Index) RemoveClient(client string)`
  - `(*Index) Lookup(client, path string) (node.Node, bool)`
  - `(*Index) Children(client, dir string) ([]node.Node, error)`——只列直接子节点，按 `Path` 升序
  - `(*Index) Clients() []string`——升序
  - `(*Index) Len() int`
  - `index.ErrNotDir`、`index.ErrNotFound`

- [ ] **Step 1: 写失败测试 `internal/index/index_test.go`**

断言：
- `Apply("win", ...)` 三个节点后 `Len() == 3`
- `Children("win", "Documents")` 只返回 `Documents` 的直接子节点，不返回孙子
- 对文件路径调 `Children` 返回 `ErrNotDir`；对不存在的路径返回 `ErrNotFound`
- `Children("win", "")` 返回顶层（即 `Documents`）
- `RemoveClient("win")` 后 `Len() == 0`，`Lookup("win","Documents")` 为 false
- **同键重复 `Apply` 覆盖旧值**：先落 `Size=1`，再落同路径 `Size=2`，`Lookup` 拿到 2
- **`Remove("win","Documents/sub")` 连子孙一起消失**：`Documents/sub/b.txt` 也查不到，但 `Documents/a.txt` 还在
- **`Apply` 一个 `Kind == node.KindDeleted` 的节点等于 `Remove`**
- 存 `Documents` 和 `documents` 两个节点，两者都能查到（**大小写敏感**）
- 1000 个并发 `Apply` + 1000 个并发 `Lookup` 无 panic（`-race` 下跑）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/index/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/index/index.go`**

内部 `map[string]map[string]node.Node`（client → path → node）。`Children` 用字符串前缀筛直接子节点，**不要缓存列表**。`Apply` 对空 `Path` 或含 `..` 的路径直接丢弃（防越界）。`Remove` 用前缀匹配删子树。

- [ ] **Step 4: 运行测试并确认通过**

Run: `go test ./internal/index/ -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/index
git commit -m "feat: 全局索引"
```

---

### Task 5: Server——监听、注册表、单聚合门禁

**Files:**
- Create: `internal/server/server.go`, `internal/server/registry.go`
- Test: `internal/server/registry_test.go`, `internal/server/server_test.go`

**Interfaces:**
- Consumes: `proto.*`, `index.Index`
- Produces:
  - `server.Registry`（零值可用，内部加锁）
  - `(*Registry) Register(name string, aggregate bool) (effectiveAggregate bool, err error)`
  - `(*Registry) Unregister(name string)`
  - `(*Registry) Live() []string`——升序
  - `(*Registry) AggregateOwner() (string, bool)`
  - `server.ErrDuplicateName`、`server.ErrNoSuchClient`
  - `server.New(listen string) *Server`
  - `(*Server) Addr() net.Addr`——监听成功后才有值；用 `:0` 时给出真实端口
  - `(*Server) Serve() error` / `(*Server) Close() error`
  - `(*Server) Index() *index.Index`
  - `(*Server) Registry() *Registry`
  - `(*Server) Query() *Query`——在 Task 7 加入

- [ ] **Step 1: 写失败测试 `internal/server/registry_test.go`**

断言：
- `Register("a", true)` → `(true, nil)`，`AggregateOwner() == ("a", true)`
- `Register("b", true)` → `(false, nil)`——**第二台降级，不报错**，`AggregateOwner()` 仍是 `a`
- `Register("a", false)` 再来一次 → 返回 `ErrDuplicateName`
- `Unregister("a")` 后 `AggregateOwner()` 为 false，此时 `Register("b", true)` → `(true, nil)`
- `Unregister("nobody")` 不 panic
- 50 个 goroutine 并发 `Register` 各自唯一的名字，最后 `Live()` 长度为 50（`-race`）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/server/ -run TestRegistry -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/server/registry.go`**

`Register` 就是上面三条断言的直译：重名 → 报错；`aggregate=true` 且已有 owner → `(false, nil)`（降级）；否则成为 owner。

- [ ] **Step 4: 写失败测试 `internal/server/server_test.go`**

断言：
- `server.New("127.0.0.1:0")` + `go s.Serve()` 后 `s.Addr()` 非 nil
- `net.Dial` 连上、`proto.NewConn` 发 `KindHello{Name:"a", Role:proto.RoleClient, Aggregate:true}`，收到 `KindHelloOK` 且 `Err == ""`、Body 里 `Aggregate == true`
- 再连一条同名同 aggregate 的 hello，收到 `KindHelloOK` 但 Body 里 `Aggregate == false`（**降级在协议上可见**）
- 一条 `Role: proto.RoleViewer` 的 hello **不进** `Registry.Live()`
- 断开第二条连接后 `s.Registry().Live()` 只剩第一条
- `s.Close()` 后再 `Dial` 失败

- [ ] **Step 5: 实现 `internal/server/server.go`**

`Serve` 在 `net.Listen` 上循环 `Accept`，每条连接一个 goroutine，读第一条 envelope 必须是 `KindHello`，`RoleClient` 走 `Registry.Register`，`RoleViewer` 只登记连接；回 `KindHelloOK`。本任务只处理 hello，其他 kind 回 `Err: "not implemented"`。连接断开时对 client 调 `Unregister` 并 `Index().RemoveClient(name)`。

- [ ] **Step 6: 运行测试并确认通过**

Run: `go test ./internal/server/ -race -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/server
git commit -m "feat: server 监听、注册表与单聚合门禁"
```

---

### Task 6: Client 拨号、上传目录树、`peerfold client` 子命令

**Files:**
- Create: `internal/client/client.go`
- Modify: `internal/server/server.go`（接 `KindTreeBatch`）, `cmd/peerfold/cmd_client.go`
- Test: `internal/client/client_test.go`

**Interfaces:**
- Consumes: `client.Shares`、`client.Scan`、`proto.*`, `server.Server`, `config.Config`
- Produces:
  - `client.Options{Server string; Name string; Role string; Shares client.Shares; Aggregate bool; Batch int; OnBatch func(sent int)}`
  - `client.Dial(o client.Options) (*Client, error)`——连上、发 hello、拿到 `HelloOK`
  - `(*Client) UploadTree() error`——分批把 `Scan` 结果发成 `KindTreeBatch`，最后一条 `KindTreeDone`（带 `EOF: true`）；**每批发完调一次 `OnBatch`**
  - `(*Client) Name() string`
  - `(*Client) Aggregate() bool`——server 说的生效值，可能被降级
  - `(*Client) Close() error`
  - `cmd_client.go`：`--server`、`--share NAME=PATH`（可重复）、`--name`（默认 `os.Hostname()`）、`--aggregate`、`--config`；启动时把参数合并进配置并 `config.Save` 落盘，然后 `Dial` + `UploadTree` 并阻塞

- [ ] **Step 1: 写失败测试 `internal/client/client_test.go`**

起真 server（`127.0.0.1:0`），造 `t.TempDir()` 树，断言：
- `Dial` 后 `c.Name()` 等于传入的名字
- `UploadTree()` 返回 nil 后，`s.Index().Children(name, "")` 含共享名节点
- 索引里节点的 `Size`/`Mtime` 与磁盘上的 `os.Stat` 一致
- 传 `Aggregate: true` 的第二个 client，`c.Aggregate()` 为 `false`（降级可见）
- **`Aggregate: true` 的那台自己的目录树同样进了索引**（"观察者也共享自己"）
- `Batch: 1` 也能正确上传（边界：每批一个）
- **`OnBatch` 被调用多次，且每次回调时 `s.Index().Len()` 严格递增**（"索引重建期间树是逐步长出来的"，不是最后一次性出现）
- `c.Close()` 后 server 端 `Registry().Live()` 不含该名字，且 `Index().Lookup` 查不到它的节点

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/client/ -run TestClientUpload -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/client/client.go` 与 server 侧批量落索引**

`Dial` 建 TCP、包 `proto.Conn`、发 `KindHello`、收 `KindHelloOK`。`UploadTree` 调 `Scan` 并用 `[]proto.NodeJSON` 分批发。**server 侧一收到 `KindTreeBatch` 就立刻 `Index().Apply`**——不要攒到最后一起落，否则上面那条 `OnBatch` 断言过不去。`Close` 只关底层连接——**server 侧的清理靠"连接断开"这个事实触发**，不要发再见消息。

- [ ] **Step 4: 接上 `peerfold client`**

`cmd_client.go` 解析参数 → `config.Load` 现有配置（不存在就用 `Default()`）→ 合并 `--server`/`--share`/`--name`/`--aggregate` → `config.Save` → 组装 `client.Options` → `Dial` → `UploadTree` → 阻塞到收到信号。`--name` 缺省时取 `os.Hostname()`。

- [ ] **Step 5: 加一个 CLI 层测试 `cmd/peerfold/cmd_client_test.go`**

断言：
- `run([]string{"client", "--server", "127.0.0.1:1", "--share", "Documents=" + tmp, "--config", cfgPath, "--name", "probe"}, ...)` 因连不上而返回非零
- **但配置已经落盘**：`config.Load(cfgPath)` 拿到的 `Server == "127.0.0.1:1"`、`ClientName == "probe"`、`Shares["Documents"] == tmp`（连不上也要先落盘——这是"一条命令加入"的手感）

- [ ] **Step 6: 运行测试并确认通过**

Run: `go test ./internal/client/ ./internal/server/ ./cmd/... -race -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal cmd
git commit -m "feat: client 拨号与目录树上传，peerfold client 子命令"
```

---

### Task 7: 列举查询与 `peerfold ls`

**Files:**
- Create: `internal/server/query.go`, `internal/viewer/viewer.go`
- Modify: `internal/server/server.go`（加 `Query()`）, `cmd/peerfold/cmd_tree.go`
- Test: `internal/server/query_test.go`, `internal/e2e/e2e_test.go`

**Interfaces:**
- Consumes: `server.Server`、`index.Index`、`proto.*`
- Produces:
  - `(*Server) Query() *Query`
  - `(*Query) List(client, path string) ([]node.Node, error)`——`client == ""` 且 `path == ""` 时返回所有在线 client 名合成的目录节点
  - `(*Query) Clients() []string`
  - `viewer.Dial(serverAddr string) (*viewer.Viewer, error)`
  - `(*viewer.Viewer) List(path string) ([]node.Node, error)`——`path` 形如 `""`、`"win"`、`"win/Documents"`
  - `(*viewer.Viewer) Close() error`

- [ ] **Step 1: 写失败测试 `internal/server/query_test.go`**

断言：
- `List("win", "")` 返回 `win` 的顶层节点（共享名）
- `List("", "")` 返回所有在线 client 的目录节点
- `List("nobody", "")` 返回 `server.ErrNoSuchClient`
- `List("win", "Documents")` 返回 `Documents` 的直接子节点

- [ ] **Step 2: 写失败测试 `internal/e2e/e2e_test.go` 的第一段**

起 server + 两个 client（`win`、`linux`），各 `UploadTree()`，然后：
- `viewer.Dial` + `List("")` 得到 `["linux","win"]`（**升序**）
- `List("win")` 得到 win 的共享名
- 逐层 `List` 走到一个已知文件，`node.Kind == node.KindFile`
- server 日志里不出现 panic

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/... -run "TestQuery|TestE2EList" -v`
Expected: FAIL

- [ ] **Step 4: 实现 `internal/server/query.go` 与 `internal/viewer/viewer.go`**

viewer 是一条普通 TCP 连接，`Dial` 后发 `KindHello{Name:"", Role: proto.RoleViewer}`。随后 `List` 发 `KindList`、收 `KindListOK`，Body 里是 `[]proto.NodeJSON`。viewer 侧把路径按第一个 `/` 切成 `client` + `path` 再发出去——**切分只做一次，剩下整条交给 server**。

- [ ] **Step 5: 接上 `peerfold ls [PATH]`**

`cmd_tree.go`：读配置拿 server 地址，`viewer.Dial`，按 `PATH`（默认 `""`）调 `List`，逐行 `fmt.Fprintf(stdout, "%s\t%s\n", kindLetter, name)`，`d` 表示目录、`f` 表示文件。PATH 不存在时 stderr 打印错误并返回非零。

- [ ] **Step 6: 运行测试并确认通过**

Run: `go test ./... -race -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal cmd
git commit -m "feat: 列举查询、viewer 连接与 peerfold ls"
```

---

### Task 8: 读路径（按范围拉取）与 `peerfold cat`

**Files:**
- Create: `internal/client/fsops.go`, `internal/server/router.go`
- Modify: `internal/client/client.go`, `internal/server/query.go`, `internal/viewer/viewer.go`, `cmd/peerfold/cmd_tree.go`
- Test: `internal/client/fsops_test.go`, `internal/e2e/e2e_test.go`

**Interfaces:**
- Consumes: `server.Registry`、`proto.*`, `client.Shares`
- Produces:
  - `server.ErrClientOffline`、`server.ErrNotFound`
  - `server.Router`：`(*Router) Send(ctx context.Context, client string, env proto.Envelope) (proto.Envelope, error)`——发给指定在线 client 并等响应，按 `ID` 关联；client 不在线返回 `ErrClientOffline`
  - `(*Server) Router() *Router`
  - `client.FS`（持 `client.Shares`）
  - `client.NewFS(sh client.Shares) *FS`
  - `(*FS) ReadAt(share, rel string, off int64, n int) ([]byte, os.FileInfo, error)`
  - `(*FS) LocalPath(share, rel string) (string, error)`——做逃逸校验并返回绝对路径
  - `(*Query) Read(client, path string, off, n int64) ([]byte, error)`——`n == 0` 表示读到结尾
  - `(*viewer.Viewer) Read(path string, off, n int64) ([]byte, error)`
  - `(*viewer.Viewer) Cat(path string) ([]byte, error)`

- [ ] **Step 1: 写失败测试 `internal/client/fsops_test.go`**

造 `t.TempDir()`，写一个 300 字节的已知内容文件。断言：
- `ReadAt(share, rel, 0, 100)` 返回前 100 字节
- `ReadAt(share, rel, 250, 100)` 返回最后 50 字节（**越界要截断到文件尾，不能报错**）
- `ReadAt(share, rel, 400, 10)` 返回 0 字节 + `io.EOF`
- 0 字节文件 `ReadAt(.., 0, 10)` 返回 0 字节 + `io.EOF`，`FileInfo.Size() == 0`（**Review Focus 第 5 条**）
- 读不存在的文件返回满足 `errors.Is(err, os.ErrNotExist)` 的错误
- `LocalPath(share, "../../etc/passwd")` 返回错误（**不返回路径**）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/client/ -run TestFSRead -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/client/fsops.go` 的读部分**

`ReadAt` 用 `os.Open` + `ReadAt`。**共享名到根路径的映射必须在拼接前校验 `rel` 逃逸**：用 `node.IsUnder(root, filepath.Join(root, rel))`。

- [ ] **Step 4: 写失败测试（续）——server 路由与端到端读**

在 `internal/e2e/e2e_test.go` 加断言：
- `v.Cat("win/Documents/a.txt")` 等于磁盘上的原文
- `v.Read("win/Documents/a.txt", 0, 5)` 等于前 5 字节
- 读 `win/Documents/nope.txt` 返回错误
- 读 `nobody/x` 返回 `ErrClientOffline` 或 `ErrNotFound` 之一（**不能 panic、不能挂住**）
- **Review Focus 第 1 条**：先 `Cat` 成功一次，然后在供货方磁盘上删掉该文件，再 `Cat` 必须失败（不是返回 0 字节）
- **Review Focus 第 3 条**：起一个 client，读之前把它的连接关掉，`Cat` 必须在 2 秒内返回 `ErrClientOffline`

- [ ] **Step 5: 实现 `internal/server/router.go`，并把读接进 client**

`Router` 为每个在线 client 保存 `*proto.Conn` 和 `map[uint64]chan proto.Envelope`；`Send` 分配自增 `ID`、登记 channel、发送、`select` 等 channel 或 `ctx.Done()`，返回时注销 channel。client 侧起一个读循环 goroutine：收到 `KindRead` 就调 `FS.ReadAt`，把结果用 `SendData` 分块发回（空文件也要发一次结束标志），出错则填 `Err`。

- [ ] **Step 6: 接上 `peerfold cat PATH`**

`cmd_tree.go`：`v.Cat(path)` 把字节写进 stdout。错误走 stderr + 非零退出码。

- [ ] **Step 7: 运行测试并确认通过**

Run: `go test ./... -race -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal cmd
git commit -m "feat: 按范围读路径、server 路由与 peerfold cat"
```

---

### Task 9: 写路径与 `put`/`mkdir`/`rm`/`mv`

**Files:**
- Modify: `internal/client/fsops.go`, `internal/client/client.go`, `internal/server/router.go`, `internal/server/query.go`, `internal/viewer/viewer.go`, `cmd/peerfold/cmd_tree.go`
- Test: `internal/client/fsops_test.go`, `internal/e2e/e2e_test.go`

**Interfaces:**
- Consumes: `server.Router`、`client.FS`
- Produces:
  - `server.ErrCrossClient`——跨 client `mv` 用
  - `(*FS) Create(share, rel string) error`——已存在返回 `os.ErrExist`
  - `(*FS) Mkdir(share, rel string) error`——已存在返回 `os.ErrExist`
  - `(*FS) WriteAll(share, rel string, r io.Reader) (int64, error)`——**整文件覆盖写**
  - `(*FS) Rename(share, oldRel, newRel string) error`
  - `(*FS) Remove(share, rel string) error`——只删空目录，非空报错
  - `(*Query) Create/Mkdir/Rename/Remove(client, ...) error`、`(*Query) Write(client, path string, r io.Reader) error`
  - `(*viewer.Viewer) Put(path string, r io.Reader) error`、`Mkdir(path)`、`Remove(path)`、`Rename(old, new string) error`

- [ ] **Step 1: 写失败测试 `internal/client/fsops_test.go`（写部分）**

断言：
- `WriteAll` 到新路径：文件出现在磁盘、内容一致、返回值等于字节数
- `WriteAll` 覆盖更长的旧文件：旧文件被完全替换（**没有残留尾巴**）
- `Create` 已存在返回 `os.ErrExist`；新建后 `Size() == 0`（**Review Focus 第 5 条**）
- `Mkdir` 建出的目录在磁盘上存在；重复 `Mkdir` 返回 `os.ErrExist`
- `Rename` 同目录改名后旧路径消失、新路径内容不变
- `Remove` 空目录成功；`Remove` 非空目录报错（**不能递归删**）
- **Review Focus 第 2 条**：把共享根 `chmod 0500`，`WriteAll` 到一个新路径必须返回错误，且磁盘上确实没有产出文件；测试结尾把权限改回来
- `LocalPath` 的逃逸校验在 `Create`/`Mkdir`/`Rename`/`Remove` 上同样生效

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/client/ -run "TestFSWrite|TestFSNames" -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/client/fsops.go` 的写部分**

`WriteAll` 必须是：在目标同目录建 `.<name>.peerfold.tmp` → 写完 → `f.Sync()` → `f.Close()` → `os.Rename`。**顺序不能换**；`Sync` 之后再 `Rename` 是"返回成功即已落盘"这条约束的实现。每个写操作都先做和读一样的逃逸校验。

- [ ] **Step 4: 写失败测试（续）——端到端写**

在 `internal/e2e/e2e_test.go` 加断言：
- `v.Put("win/Documents/new.txt")` 后**供货方磁盘上真的有这个文件**且内容一致（这是"保存同步回原机"的核心断言）
- `v.Put` 覆盖已有文件后原机内容变了
- `v.Mkdir("win/Documents/sub2")` 后原机目录存在
- `v.Rename("win/Documents/a.txt", "win/Documents/renamed.txt")` 后原机旧名没了、新名有
- `v.Remove("win/Documents/zero.bin")` 后原机文件没了
- `v.Rename("win/Documents/a.txt", "linux/Documents/a.txt")` 返回 `ErrCrossClient`
- 写一个不存在的父目录下的文件（`win/Documents/nope/x.txt`）返回错误，不是 panic
- **Review Focus 第 3 条**：`Put` 时用一个在第 3 次 `Read` 返回错误的 `io.Reader`，再在此时关掉 client 的连接，`Put` 必须返回 `ErrClientOffline`，且**原机上不能留下目标文件**（临时文件可以残留）

- [ ] **Step 5: 实现 server 侧路由 + client 侧处理 + CLI 四个子命令**

`KindWrite` 先发 `KindWrite`（带 `Truncate`）再连续发 `FrameData`。`Rename` 在 server 侧先比较两端 client 名，不同就返回 `ErrCrossClient`，**不要**降级成复制+删除。`Put` 的内容来自 stdin（`cmd_tree.go` 里 `v.Put(path, os.Stdin)`）。

- [ ] **Step 6: 运行测试并确认通过**

Run: `go test ./... -race -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal cmd
git commit -m "feat: 写回原机——整文件覆盖、新建、建目录、改名、删除"
```

---

### Task 10: 变更传播——fsnotify → 索引 → 观察者

**Files:**
- Create: `internal/client/watch.go`
- Modify: `internal/node/node.go`（加 `KindDeleted`）, `internal/client/client.go`, `internal/server/server.go`, `internal/viewer/viewer.go`
- Test: `internal/client/watch_test.go`, `internal/e2e/e2e_test.go`

**Interfaces:**
- Consumes: `client.Shares`、`node.Node`、`proto.*`
- Produces:
  - `node.KindDeleted node.Kind = 255`
  - `client.Watch(sh client.Shares, emit func(events []node.Node) error) (io.Closer, error)`
  - `(*Client) Watch() (io.Closer, error)`
  - `(*viewer.Viewer) Watch() (<-chan []node.Node, error)`——server 主动推来的增量

- [ ] **Step 1: 写失败测试 `internal/client/watch_test.go`**

断言：
- 起 `Watch`，在共享目录里新建 `Documents/x.txt`，2 秒内收到 `Path == "Documents/x.txt"`、`Kind == node.KindFile` 的事件
- 删掉它，2 秒内收到 `Kind == node.KindDeleted`
- 改它内容，2 秒内收到 `KindFile` 且 `Size` 变了
- 新建一个子目录再往里建文件，两条事件都收得到（**不递归 `Add` 新目录就会漏掉这条**）
- Closer 关闭后再改文件不再有事件产出，且不 panic

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/client/ -run TestWatch -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/client/watch.go`**

用 `fsnotify.NewWatcher()`，对每个共享根递归 `Add` 所有子目录；收到建目录事件时对新目录 `Add`。事件里的绝对路径转回 `<share>/<rel>`；`Excluded` 为真的路径丢弃。

- [ ] **Step 4: 写失败测试（续）——端到端实时**

在 `internal/e2e/e2e_test.go` 加断言：
- 观察者 `v.Watch()` 后，在 `win` 的共享目录里新建文件，2 秒内 `v.List("win/Documents")` 能看到它（**这是"实时查看文件列表"的核心断言**）
- 删掉后 2 秒内 `v.List` 看不到它
- 观察者 `v.List("")` 在 client 断开后 2 秒内不再返回该 client（**索引只装活着的 client**）
- **Review Focus 第 4 条**：把观察者自己的挂载点目录放进它的 `--share`，那个目录下**新建**的文件不进索引

- [ ] **Step 5: 实现 server 侧汇总与广播**

server 收到 `KindEvent` 后调 `Index().Apply`（删除事件带 `KindDeleted`，由索引内部转成 `Remove`），然后向所有 `RoleViewer` 的连接广播同一个增量。广播失败（对端已断）只记日志、不影响其他 viewer。

- [ ] **Step 6: 运行测试并确认通过**

Run: `go test ./... -race -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal cmd
git commit -m "feat: 文件变化实时传播到索引与观察者"
```

---

### Task 11: 端到端主流程验收

**Files:**
- Modify: `internal/e2e/e2e_test.go`
- Create: `docs/peerfold-cli.md`

**Interfaces:**
- Consumes: 前面全部
- Produces: 无新接口

- [ ] **Step 1: 写端到端验收测试 `TestMainFlowEndToEnd`**

一个测试里按 spec 第九节的六步走完，全部用真 server + 真 client + 真临时目录（不开内核挂载）：
1. 起 server
2. 起 `win`、`linux` 两个 client 并上线
3. 观察者 `List("")` 同时看到 `linux` 和 `win`
4. `Cat("win/<share>/a.txt")` 拿到内容
5. `Put` 改掉它，然后**直接读供货方磁盘上的文件**确认内容变了
6. 在 `linux` 的磁盘上直接新建一个文件，2 秒内观察者 `List` 能看到

- [ ] **Step 2: 运行确认通过**

Run: `go test ./internal/e2e/ -run TestMainFlowEndToEnd -v`
Expected: PASS

- [ ] **Step 3: 写 `docs/peerfold-cli.md`**

只写实际存在的东西：`server`、`client`、`ls`、`cat`、`put`、`mkdir`、`rm`、`mv` 各自的一行用法和一个例子，加一段"v1 已知限制"（跨机 mv 不支持、无缓存、无忽略规则、后写覆盖、无认证、还没有内核挂载点）。

- [ ] **Step 4: 全量跑一遍带竞态检测**

Run: `go build ./... && go test ./... -race -count=1`
Expected: 全部 PASS，无 `DATA RACE`

- [ ] **Step 5: Commit**

```bash
git add internal/e2e docs/peerfold-cli.md
git commit -m "test: v1 核心主流程端到端验收"
```

---

## 完成之后

这份计划跑完，"主流程"在**没有内核挂载点**的前提下是通的、可测的：`peerfold cat` 能读远端文件，`peerfold put` 能写回原机，原机变化 2 秒内反映到索引。

剩下的挂载那一半（Linux FUSE + Windows WinFsp、按需水合、内核缓存主动失效）是**第二份计划**的内容，它在这份之上加一层视图，不改协议、不改索引、不改读写路径。

### 一处对 spec 的补充，需要你确认

spec 第二、三节里只有 `server` 和 `client` 两个角色。这份计划为了让 `peerfold ls/cat/put` 有地方说话，在协议里加了一个 `viewer` 连接角色——它不供货、不注册进 client 注册表，只用来查询和操作聚合树。下一份计划里的挂载层也会复用它。

spec 里"全局只允许一台开 aggregate"这条不受影响：门禁管的仍然是 `client` 角色的 `--aggregate` 标志。