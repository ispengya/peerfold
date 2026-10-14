# peerfold 交接文档（给下一个会话）

> 写于 2026-10-14 会话中断点。本文件是**执行态**交接，不是产品文档；产品文档见 `docs/superpowers/`。
> 阅读顺序建议：§1 → §2 → §3（立刻要做的）→ §5（环境陷阱，不看必踩）→ §6（裁定，会约束你）。

---

## 1. 产品是什么

**peerfold** —— 「像 NGINX 一样的聚合文件程序」。形态是守护进程 + 一个配置文件 + 无 GUI：
`peerfold server` 起服务端，`peerfold client` 主动拨号连上来，把所有客户端共享的文件聚合成**操作者本机上的一个虚拟目录树**，可列举、可读、可写；写回会落到文件原本所在的机器上。

- **严格星型拓扑**：client 之间永不互连，server 是唯一中继。NAT 友好（client 向外拨号），代价是 server 吃带宽且是单点。
- **聚合 = 虚拟挂载 + 懒加载**：目录列举来自 server 的全局索引（不向 client 往返）；文件内容只在打开时向属主 client 拉取；保存写回属主磁盘。
- **实时**：client 侧 fsnotify → 推 event → server 更新索引 → 通知观察者。
- **只允许一个 client 打开 `aggregate`**；第二个请求聚合的会被**降级为普通供给方**并记日志，不退出。
- 聚合来的文件，本机软件要能像本地文件一样查看和修改；新建文件等常规文件操作 v1 就必须支持。
- v1 平台：**Windows 和 Linux**（macOS 推迟）。
- 驱动只在**观察者**侧需要：Linux FUSE、Windows WinFsp。

设计文档：`docs/superpowers/specs/2026-10-14-peerfold-design.md`（已冻结）
实施计划：`docs/superpowers/plans/2026-10-14-peerfold-v1-core.md`（11 个任务，已冻结）

---

## 2. 当前进度

### 已完成：Task 1（模块骨架、配置读写、CLI 分发）—— 审核通过

提交链（分支 `feat/v1-core`）：

   4ea125c  refactor: Task 1 复审意见——取消 Shares 归一化，使 Save/Load 成为精确互逆   <- HEAD
   ca4c1c8  fix: Task 1 审核意见——删去空断言测试、统一 Shares 归一化、补全分发分支测试
   ed0f017  feat: 模块骨架、配置读写与 CLI 分发
   61c748f  chore: 初始化仓库（设计文档、实施计划、技能脚手架）

- 测试：`cmd/peerfold` 4 个 + `internal/config` 6 个，共 10 个全绿；gofmt 干净、`go vet` 干净。
- 审核记录：Task 1 审核者 **Approved**（0 Critical / 0 Important / 5 Minor，已处理）→ fix round 1 复审：全部处理 → fix round 2 复审：**全部处理，无新增 Critical/Important**。
- 交付物：`go.mod`/`go.sum`、`.gitattributes`、`internal/config/{config.go,config_test.go}`、`cmd/peerfold/{main.go,main_test.go,cmd_server.go,cmd_client.go,cmd_tree.go}`（后三个是打印 TODO 到 stderr 并返回 0 的桩）。

### 进行中：Task 2（线上协议——帧与连接）—— **代码已写，未提交，且最后一步未验证**

`internal/proto/` 目前是 **untracked**（`git status --short` 显示 `?? internal/proto/`），5 个文件：

- `frame.go`：`FrameKind`（`FrameControl=1`、`FrameData=2`）、`MaxPayload = 1<<20`、`WriteFrame`、`ReadFrame`。帧 = 1 字节 kind + 4 字节大端长度 + payload。`io.ReadFull` 读满头部；帧边界处的干净结束返回 `io.EOF`，中途截断返回 `io.ErrUnexpectedEOF`；长度超限的错误信息含 `too large`。
- `conn.go`：`Conn` + `NewConn` + `Send`/`Recv`/`SendData`/`RecvData`/`Close`。**读锁 `rmu` 与写锁 `wmu` 分开，且 `Send` 与 `SendData` 共用 `wmu`**（否则并发发送会交错）。数据帧 payload = 8 字节大端 stream ID + 内容；**零长度内容 = 流结束**（0 字节文件就是一个空帧）。
- `messages.go`：`Envelope{ID,Kind,Body,Err,EOF}`、13 个 kind 常量（含 `KindListOK`）、`RoleClient`/`RoleViewer`、以及全部 body 结构体（`HelloBody`、`HelloOKBody`、`NodeJSON`、`TreeDoneBody`、`ListBody`、`ListOKBody`、`ReadBody`、`WriteBody`、`MkdirBody`、`CreateBody`、`RenameBody`、`RemoveBody`、`EventBody`）。
- `frame_test.go`（6 个测试）、`conn_test.go`（3 个测试）。

**验证状态（重要，别当成已完成）**：

1. RED 证据齐全：先 `no non-test Go files in internal/proto`，后 `undefined: proto.NewConn` 等。
2. **曾**跑到 9/9 PASS（不带 `-race`；本机无 C 编译器，`-race` 不可用，见 §5）。
3. 之后我**加强了 `TestConnConcurrentSend`**（8 个并发发送者 × 32KB 各不相同载荷，逐字节比对收到的 body），目的是在缺少竞态检测器时让「漏掉写锁」这个缺陷可被发现。**这个加强版还没有跑过一次。**
4. 我原本要做的下一步是：**拿掉写锁做变异测试**，确认加强后的测试真的会失败。**这条命令的提权被取消了，所以没有执行**，`conn.go` 的写锁完好（已确认有 2 处 `c.wmu.Lock()`），工作区没有残留改动。

---

## 3. 立刻要做的（第一步，按顺序）

   1) 跑一次全量测试，确认加强后的 conn_test.go 能编译且通过：
      go test ./... -v
      （必须带 danger-full-access 提权，见 §5）

   2) 补做被取消的变异测试（这是判断「并发测试到底有没有鉴别力」的唯一办法）：
      - 备份或记住 conn.go
      - 删掉 Send 与 SendData 里的 c.wmu.Lock() / defer c.wmu.Unlock() 共 4 行
      - go test ./internal/proto/ -run TestConnConcurrentSend -count=5 -v
      - 期望：FAIL。若仍 PASS，说明这个测试没有鉴别力，需要再想办法（装 C 编译器跑 -race，或改成确定性更强的断言）
      - 用 git checkout -- internal/proto/conn.go 还原，再确认全绿

   3) gofmt + vet，然后提交（Step 7）：
      git add internal/proto
      git commit -m "feat: 线上协议帧与连接封装"

   4) 生成审核包并派发 Task 2 的**只读审核子代理**（模板见 §4），审核通过后往 ledger 追加
      「Task 2: complete (commits <base7>..<head7>, review clean)」，
      然后按同样的循环做 Task 3。

补一句：Task 2 的 stdout 行为是「帧」级别，没有 CLI 可跑；要手工验证就写个临时 Go 测试，别去动 `cmd/`。

---

## 4. 执行流程（SDD）与账本的存放位置

用的是 `subagent-driven-development` 技能，技能文件在 `.agents/skills/subagent-driven-development/`
（`SKILL.md`、`implementer-prompt.md`、`task-reviewer-prompt.md`、`re-review-prompt.md`）。

**关键：`subagent` 子代理在本套 harness 下无法写文件**（给委派子代理的提权会被自动拒绝）。
所以本次采用**混合模式**并有明确裁定：**控制器自己实现代码，每个任务后派发一个只读审核子代理做质量门，全部任务做完再来一次整分支终审。**
不要试图派「实现者子代理」——它写不了文件。审核者只需要读，所以可用。

账本与临时产物都在 **`.superpowers/sdd/2026-10-14-peerfold-v1-core/`**（该目录被 `.gitignore` 忽略）：

- `progress.md` —— **ledger，最重要的文件**。含全部裁定、每任务完成行、环境勘误。上下文被压缩后就靠它。
- `constraints.md` —— 从计划里抽出的全局约束 + Review Focus（5 个失败模式），每个任务都隐含适用。
- `plan-path` —— 指向计划文件的路径。
- `task-1-brief.md` / `task-2-brief.md` —— 每个任务的简报（从计划里切出来的那一段）。
- `task-1-report.md` —— Task 1 实现报告，含两轮修复记录、变异测试证据、以及我自己那次真实失败的自述。
- `task-1-package.txt` / `task-1-fix-package.txt` / `task-1-fix2-package.txt` —— 各轮审核用的 diff 包。
- `sdd-package.ps1` —— 生成 diff 包的脚本，**被执行策略禁止运行**（见 §5），实际都是内联同一段逻辑做的。

生成简报 / diff 包的做法（内联，别调用 .ps1）：

    # 简报：按 '### Task N' 切段
    $lines = Get-Content -LiteralPath $plan -Encoding utf8
    # 找到 '^###\s+Task\s+N[^0-9]' 为起点，下一个 '^###\s+Task\s+\d' 或 '^##\s' 为终点
    # 写文件时必须用 Write-Lf（LF、无 BOM）

    # diff 包
    $parts = @('## commits','') + (git log --oneline "$base..$head") +
             @('','## diff --stat','') + (git diff --stat "$base..$head") +
             @('','## diff -U10','') + (git diff -U10 "$base..$head")
    Write-Lf $out ($parts -join "`n")

派发审核者时的必填项：简报路径、`constraints.md`、实现报告路径、BASE_SHA、HEAD_SHA、diff 包路径，以及该任务的**裁定**（否则审核者会拿计划的原文去判，把已裁定的事项当缺陷）。审核者是只读的，且被明确要求**不许再派子代理**、**不许重跑测试套件**、**不许相信实现报告**。

---

## 5. 环境陷阱（不看必踩，逐条都是踩过的）

### 5.1 `pwsh` 其实是 Windows PowerShell 5.1

    PSVersion=5.1.19041.4291

后果：`Set-Content -Encoding utf8` 会写 **UTF-8 BOM**。BOM 会让 `gofmt -l` 把**每个** Go 文件都判为未格式化。

**所有源码/文本写入都必须用这个 helper（LF 结尾、无 BOM）：**

    function Write-Lf([string]$p,[string]$text) {
      $text = $text.Replace("`r`n","`n").TrimEnd("`n") + "`n"
      $enc = New-Object System.Text.UTF8Encoding $false
      [System.IO.File]::WriteAllText($p, $text, $enc)
    }

### 5.2 行尾

仓库级已设 `core.autocrlf=false`，并落地了 `.gitattributes`（`* text=auto eol=lf`）。
`core.autocrlf` **不是版本化状态**，新克隆靠的就是 `.gitattributes`——这是它存在的唯一理由，别删。

### 5.3 Go 工具链

- 机器原本没有 Go；已用 `winget install GoLang.Go` 装得 **go1.27.0**，位于 `C:\Program Files\Go`。
- 长驻的 DSH 进程环境变量是旧的，`go` 在 PATH 上不可解析，所以有个转发 shim：**`D:\Scripts\go.cmd`**，内容设定
  `GOCACHE`/`GOMODCACHE` 到 `%TEMP%\peerfold-*`、`GOPROXY=https://goproxy.cn,direct`。
- **`gofmt` 不在 PATH**：用绝对路径 `C:\Program Files\Go\bin\gofmt.exe`。
- `proxy.golang.org` 在本机不可达，必须走 `goproxy.cn` 镜像。

### 5.4 提权（沙箱）

- **所有 `go` 命令都要 `sandbox_permissions: "danger-full-access"`。** 原因：Go 的构建/模块缓存放在 `%TEMP%`，而 Windows ACL 沙箱在 `workspace-write` 下不允许写 `%TEMP%`，会报 `Access is denied`。
- 纯仓库内文件写入 `workspace-write` 就够。但注意**标准策略可能被判成 read-only**，那时任何写入都要提权；提权请求可能被拒绝（本会话就发生过一次），被拒绝时不要反复重试同一件事，先问人。
- 子代理**拿不到**提权 → 所以才只能做只读审核。

### 5.5 没有竞态检测器

`go test -race` 需要 cgo，而本机**没有任何 C 编译器**（`gcc`/`clang`/`cc` 全无），`CGO_ENABLED=1` 会报
`cgo: C compiler "gcc" not found`。

- 现状：**替代方案是把并发测试写强**（大载荷 + 多发送者 + 逐字节比对），而不是放弃验证。
- 若你（或用户）愿意装一个 C 工具链，装上之后就能跑：
  `$env:CGO_ENABLED='1'; go test -race ./...`
- 这是一个**待用户决定**的事项，见 §7。

### 5.6 其他

- **`bash` 在本沙箱不可用**：`D:\Git\Git\bin\bash.exe` 会 `fatal error - couldn't create signal pipe, Win32 error 5`。技能里所有 `bash scripts/*` 的指引都要改成 PowerShell 等价实现。
- **`.ps1` 被执行策略禁止运行**（`UnauthorizedAccess`，未签名）。要么内联逻辑，要么用 `-ExecutionPolicy Bypass -File`。
- **`Get-Content` 必须显式 `-Encoding utf8`**，否则按系统 ANSI 代码页读，中文全变乱码（踩过：约束提取器因此产出过一个 645 行的垃圾文件）。
- 捕获 Go 的测试输出要这样，否则 PowerShell 会把 stderr 折叠成 `NativeCommandError` 吞掉内容：
  `$o = & go test ./... -v 2>&1; $o | ForEach-Object { $_.ToString() }`
- 每次 `pwsh` 调用都会有一堆 `ConstrainedLanguage` 报错（harness 自己的 UTF-8 前导代码失败），属背景噪音，命令本身仍返回正确结果。
- **提交前必须检查 `go test` 的退出码**。踩过：我在没检查退出码的情况下 `git add` + `commit`，把一个失败的测试提交了进去（后来用 `--amend` 覆盖）。现在一律用退出码门禁。

---

## 6. 已做过的裁定（会约束后续任务，别当缺陷改掉）

### 6.1 预检阶段（11 条）

1. `node.KindDeleted = 255` 定义在 **Task 3**，被 Task 4 的 `Apply` 使用；Task 10 才开始发出它。
2. Task 2 的常量表**新增** `proto.KindListOK = "list_ok"`。
3. **流结束 = 零长度内容的数据帧**：`SendData(id, nil)`；0 字节文件就是一个空帧。
4. `server.New(listen)` **同步**做 `net.Listen`；监听失败由 `Serve()` 返回；`Addr()` 在 `New` 之后立刻有效。测试写法：`s := server.New("127.0.0.1:0"); go s.Serve(); s.Addr()`。
5. Task 1 的测试断言 `ls` 返回 **0**（桩）；「PATH 不存在 → 非零」属于 Task 7。
6. `proto.ReadBody.N` 是 `int64`，而 `FS.ReadAt` 取 `n int` → 显式转换 + 边界检查。
7. `client.Options.Batch` 默认 **500**；一个 `KindTreeBatch` 信封编码后必须 < `proto.MaxPayload/2`，否则要拆分。
8. `Router.Send` 的 `ctx` **只覆盖首帧等待**；`Query.Read` 用 30s；离线靠注册表查表**立即**判定，不靠超时。
9. Review Focus 2 的测试按平台分叉：Linux `chmod 0500` 目录；Windows 对已存在文件设只读属性；两者都断言返回错误且目标未被修改。
10. 保留 `FS.Create` 与 `KindCreate`（Milestone B 需要），并在 Task 9 的 CLI 里加 `peerfold touch PATH` 给它一个真实消费者。
11. `Put` 走 `WriteAll`（临时文件 + 原子 rename），**绝不** Create 后流式写，避免留下半截目标文件。

### 6.2 执行阶段

- **R1 `.gitattributes` 保留**（虽不在计划 Step 8 的 add 列表内）。理由：它是 LF 策略唯一的版本化载体。判错的代价：多一个仓库级配置文件。
- **R2 `config.Load` 不合并默认值、不归一化 `Shares`；`Save`/`Load` 是精确互逆。** 因此 **Task 5/6 的 `server`/`client` 子命令必须用 `config.Default()` 兜底空的 `Listen`/`Server`**。判错的代价：Task 5 处要补一层默认值。这条同时把「服务端监听空地址」的坑挡在了 Task 5。
- **R3 不用 `-race`**（无 C 编译器），改为把并发测试写强。判错的代价：存在数据竞态而未被发现，直到装上 C 工具链后跑一次 `-race`。
- **R4 `config.Save` 创建父目录**（因为 `DefaultPath()` 落在可能不存在的 `peerfold/` 目录下）。
- **R5 子命令桩的 TODO 通知写 stderr，不写 stdout**：`ls`/`cat` 落地后 stdout 是纯数据通道，现在就别污染它。
- **R6 混合执行模式**（控制器实现 + 只读审核子代理），因为委派子代理写不了文件。
- **R7 提交前用 `go test` 退出码做门禁**（踩过失败提交）。
- **R8 测试必须被证明可失败**：新增或修改测试后，用变异测试确认它在缺陷存在时真的会失败，再宣称它「钉住了」某个性质。踩过：为满足审核意见而新加的 `TestConfigNilSharesRoundTrip` 第一版**永不可能失败**，被复审者当场识破。判错的代价：一堆看起来在保护你、实际什么也不检查的测试。

---

## 7. 待用户确认 / 待决定的事项

1. **`viewer` 角色是对规格的增补** —— 设计文档只写了 server 和 client 两种角色，而计划的 CLI 需要 `viewer`（只观察、不供给文件）。**当前按「已接受」处理**，但如果用户反对，Task 5/6 要回头改。
2. **`git init` 是控制器做的**（分支 `feat/v1-core`，初始提交 `61c748f`）。用户未明确要求过。
3. **要不要装 C 工具链以启用 `-race`？** 影响：整个协议层与后续所有并发代码的竞态检测能力。
4. **计划的批准是隐含的**：用户在计划写完后说「先不考虑这么多」，执行是据此推进的，不是显式批准。若用户想改计划，现在改成本最低。
5. **第二份计划尚未写**：挂载侧（Linux FUSE + Windows WinFsp、按需水合、内核缓存失效）。它只增加一个视图层，**不改协议、索引、读写路径**。
6. **Task 1 的 5 条 Minor 审核意见**已全部处理（3 条改代码、2 条裁定保留），无需用户介入，仅备案。

---

## 8. 技术事实速查

### 协议

- 传输：TCP；帧 = 1 字节 kind + 4 字节大端 payload 长度 + payload。
- `FrameControl` 承载 JSON `Envelope{ID,Kind,Body,Err,EOF}`；`FrameData` 承载 8 字节大端 ID + 内容字节。
- `proto.MaxPayload = 1 << 20`（既是上限也是分块大小）。
- 零长度数据帧 = 流结束。
- 角色：`proto.RoleClient = "client"`、`proto.RoleViewer = "viewer"`（viewer 不供文件、不进 client 注册表）。

### 配置

- `config.Config{Listen, Server, ClientName, Shares, Aggregate}`，JSON 标签 `listen`/`server`/`client_name`/`shares`/`aggregate`。
- `Default()` → `Listen: ":7000"`、`Shares` 是非 nil 空 map。
- `Load()` 忠实读：不合默认值、不归一化；缺文件时错误满足 `errors.Is(err, os.ErrNotExist)`；省略的键保持零值（`Shares` 为 nil）。
- `Save()` 忠实写 + 建父目录 + `MarshalIndent` + 结尾换行。
- 路径：`os.UserConfigDir()/peerfold/config.json`。

### CLI

`peerfold server --listen :7000`
`peerfold client --server HOST:PORT --share NAME=/abs/path [--share ...] [--name NAME] [--aggregate] [--config PATH]`
`peerfold ls [PATH]` / `cat PATH` / `put PATH`（stdin）/ `mkdir PATH` / `rm PATH` / `mv OLD NEW` / `touch PATH`（Task 9 加）

### 索引与写语义（后续任务要遵守）

- 无数据库：全局索引是内存里的 `map[client]map[path]node.Node`；server 重启靠 client 重连重建。
- 挂载点自身**绝不进入索引**（否则无限套娃）。
- 写回是**整文件写回**（临时文件 → `f.Sync()` → `f.Close()` → `os.Rename`），**同步**，无观察者侧暂存与重试队列。
- 编辑器的保存是「临时文件 + rename」，所以 `rename` 必须翻译成整文件写回 + 原子替换；`create`+`write`+`rename` 是 v1 不可省的。
- v1 无内容缓存、无忽略规则、last-write-wins、索引键按 UTF-8 字节串区分大小写。
- **`stat` 的 size 出错会损坏文件**，所以 `open` 要在属主 client 处重新校验 size/mtime。

### 五个 Review Focus（每个任务都要防）

1. 列举与读取之间文件被删除 → 必须报错，不许返回 0 字节的假文件。
2. 写回遇磁盘满/权限拒绝 → 必须向上传播。
3. client 传输中途掉线 → 必须报错，**永不挂起**。
4. 观察者的挂载点绝不能进索引。
5. 0 字节文件与空目录必须能正常列举/读/写。

规格的反目标：树说谎、保存悄悄丢数据、误删真文件、套娃、退化成同步盘。
规模上限：≤10 个 client、每 client ≤100 万文件；列举必须分批。

---

## 9. 剩余任务（计划里的 Task 3–11）

- **Task 3** 节点模型与共享目录扫描（定义 `node.KindDeleted = 255`、`NodeKindDeleted`）
- **Task 4** 全局索引（`Index`、`Apply` 需处理 `KindDeleted`、`Remove(client,path)`）
- **Task 5** Server：监听、注册表、单聚合门禁（+ R2：空 `Listen` 要用 `Default()` 兜底）
- **Task 6** Client：拨号、上传目录树、`peerfold client` 子命令
- **Task 7** 列举查询、`KindListOK` 与 `peerfold ls`（这里才断言「PATH 不存在 → 非零」）
- **Task 8** 读路径与 `peerfold cat`（离线立即判定）
- **Task 9** 写路径与 `put`/`mkdir`/`rm`/`mv`/`touch`（含平台分叉测试）
- **Task 10** 变更传播：fsnotify → 索引 → 观察者
- **Task 11** 端到端主流程验收
- **Final** 整分支终审（用 `requesting-code-review` 的 code-reviewer 模板）+ `finishing-a-development-branch`

流程要求：一个任务做完 → 生成 diff 包 → 派发只读审核者 → 处理 findings（每轮修复后都要再一次**限定范围**的复审）→ 往 ledger 追加完成行 → 下一个任务。**审核有未处理的 Critical/Important 时不许进下一个任务。**

收尾时（全部做完且终审干净后）：把 ledger 里**每一条** `Ruling:` 汇总进最终消息的「Rulings I made」，每条附「判错的代价」——那是这些决策唯一能到达用户的地方。然后才删工作区。最后用 `finishing-a-development-branch`。

---

## 10. 已知偏差（如实记录，别美化）

1. **实现者子代理不可用** → 混合模式，控制器自己写代码。质量门靠只读审核者保住。
2. **技能的 Model Selection 无法遵守**：harness 的 `subagent` 工具**没有 `model` 参数**，所有派发都用会话默认模型。
3. **Task 1 的实现报告迟交**：第一次写入所在的命令在 `gofmt` 处中断（脚本因 `gofmt` 不在 PATH 而中止），重做提交时漏补，被审核者记为证据缺口。后来补写，并在报告里自述原因。
4. **一次失败提交**：为落实审核意见而新加的测试第一版必然失败，且我在未检查退出码的情况下提交了它。已 `--amend` 覆盖并加门禁。
5. **没有 `-race`**（见 §5.5、§7.3）。
6. **中文输出在只读模式下会乱码**：read-only 的 PowerShell 处于 ConstrainedLanguage，harness 的 UTF-8 前导代码失败，导致中文在终端显示为乱码——**文件内容是好的**，判断文件正确性要用 `read` 工具而不是终端回显。

---

## 11. 给下一个会话的忠告

- **先读 `.superpowers/sdd/2026-10-14-peerfold-v1-core/progress.md`**，它比这份文档更细，且含全部裁定的原始上下文。
- **不要重新发明流程**：简报切段、diff 包、只读审核、ledger，都在 §4 写好了。
- **不要试图派实现者子代理**，它写不了文件。
- **不要相信终端里的中文回显**，用 `read` 工具看文件。
- **每写一个测试就问自己：它在缺陷存在时会失败吗？** 不会失败的测试是负债。
- **提交前看退出码。**
- 提权被拒绝时不要反复重试；先问用户。
