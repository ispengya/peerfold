# peerfold 项目文档

**最后更新**：2026-10-18
**当前状态**：v1 内核 + 挂载层完成，Windows 可用（多 client 实测通过），Linux 待验证。

---

## 一、项目简介

peerfold 是一个联邦文件系统：一台机器跑 `server`（聚合索引），若干机器跑 `client`（共享本地目录），一台机器跑 `mount`（把聚合后的树挂载成一个本地目录，像普通文件夹一样用）。

**v1 范围**（明确不做）：认证、加密、缓存分层、增量同步（整文件写回 + 原子替换）。拓扑是严格星型。

**冻结文档**（不要改）：
- 挂载层设计：`docs/superpowers/specs/2026-10-15-peerfold-mount-design.md`
- 挂载层计划：`docs/superpowers/plans/2026-10-15-peerfold-mount.md`
- 上游 v1 核心：`docs/superpowers/specs/2026-10-14-peerfold-design.md`
- 上游 v1 计划：`docs/superpowers/plans/2026-10-14-peerfold-v1-core.md`

---

## 二、已完成功能

### 核心架构（任务 1–15）

| 层 | 内容 | 位置 |
|---|---|---|
| 索引 | 全局聚合索引，按 client/share/relative-path 索引 | `internal/index/` |
| 服务端 | 聚合 server，client 上线/下线广播、请求转发 | `internal/server/` |
| 客户端 | 加入 server、共享本地目录、fsnotify 监听变化并推送 | `internal/client/` |
| Viewer | 只读客户端（mount 的数据面） | `internal/viewer/` |
| 挂载层 | 可移植操作核 + cgofuse 适配层 | `internal/mount/` |
| 配置 | per-component 配置（见下） | `internal/config/` |
| 日志 | per-component 诊断日志（见下） | `internal/logging/` |
| CLI | `peerfold server / client / mount / ls / cat / put / mkdir / rm / mv` | `cmd/peerfold/` |

### 挂载层（任务 11–15 + 后续修复）

- **cgofuse 适配层**（`host.go`）：可移植操作核 → FUSE 回调，双向翻译
- **强制刷盘表**（D10）：Fsync / Flush / Release / dirty Truncate / rename 前 / unlink 前 / unmount / SIGINT
- **重连监督器**（D11）：退避重连（1s→30s），2 分钟放弃后自卸载，离线返回 EIO
- **owner tombstone**：client 下线 → server 广播 → mount 处理
- **多 owner 支持**：一个 owner 下线只撤其子树，最后一个下线才卸载
- **pending 窗口**：刚创建的文件/目录在 watcher 上报前可被 Windows 重新查询到
- **真实挂载 e2e**（`e2e_windows_test.go`）：10 个子测试，覆盖全部关键路径

### 运维设施

**配置布局**（`~/` 下，每个组件一个文件夹）：

```
~/peerfold-server/
  config.json          ← server 配置（listen 地址）
~/peerfold-client-<name>/
  config.json          ← client 配置（server 地址、共享目录）
  log.txt              ← client 诊断日志
```

**日志**（始终开启，无需 flag）：

```
~/peerfold-server/log.txt      ← server 日志
~/peerfold-client-<name>/log.txt ← client 日志
```

格式：`时间戳 [级别] 内容`，级别有 `[info]` / `[debug]` / `[error]`。

**日志记录的操作**：

| 组件 | 记录内容 |
|---|---|
| server | client 连接/断开、list、read、write、mkdir、remove、rename、事件接收、广播 |
| client | read、write、mkdir、remove、rename、watcher 推送 |

---

## 三、验证状态

### 真实挂载 e2e（Windows / WinFsp）

`go test ./internal/mount -run '^TestE2ERealMount$' -count=1 -v` — **10/10 全过**：

| 子测试 | 状态 | 覆盖 |
|---|---|---|
| list | ✓ | 列目录 |
| read | ✓ | 读文件 |
| write-inside-the-share | ✓ | 挂载内写文件 |
| write-reaches-the-owners-disk | ✓ | 写回到 owner 磁盘 |
| read-back-through-the-mount | ✓ | 刚写的文件立刻读回 |
| fsync-then-verify-the-owner | ✓ | fsync 持久化 |
| mkdir-rename-unlink | ✓ | 创建目录/子文件、重命名、删除 |
| editor-save-pattern | ✓ | 编辑器保存（写临时文件 + rename 覆盖） |
| directory-listing-matches-the-owner | ✓ | 挂载列表 == owner 磁盘 |
| unmount-when-the-owner-goes-away | ✓ | owner 下线 → 挂载点消失 |

### 单元测试

`go test ./... -count=1` — **11/11 包全部通过**（含 server / client / index / mount / viewer / config / e2e）。

### 手工验证（2026-10-18，多 client 场景）

- server + alice + bob 两个 client + mount，同时在线
- 挂载点下可见 `alice/docs/` 和 `bob/docs/` 两棵树
- 通过挂载写入/读回/重命名/删除，挂载和 owner 磁盘全程双向一致
- client 日志完整记录每次操作链路

### 未验证

- **Linux 真实 FUSE 挂载**：本机无 `/dev/fuse`、C 工具链、libfuse。核心逻辑跨平台，但真实挂载未跑过。

---

## 四、已解决的关键问题

### WinFsp 权限拒绝（1a/1b/1c）

**现象**：刚创建的文件读不回来（`Access is denied`），新目录里建第一个子文件被拒，editor 保存覆盖失败。写路径正常。

**根因**：挂载层不设 `Uid`/`Gid`，WinFsp 把 uid=0 映射到 `S-1-5-0`（Nobody，永不匹配调用方 token），唯一能命中调用方的是 world ACE（`S-1-1-0` Everyone）。但 mode 加宽只做了 owner（0700），world 位为 0 → `AccessCheck()` 失败 → 驱动层拒绝，请求到不了 FUSE 回调。

**修复**：mode 加宽从 0700 改成 0777（`fuse.S_IRWXU|S_IRWXG|S_IRWXO`）。"写成功读失败"的表面矛盾：`FILE_CREATE` 查父目录，`FILE_OPEN` 查文件自身 DACL——文件刚建时 mode=0700。

### Rename 不到 owner（1b 尾部）

**现象**：`os.WriteFile` 后立刻 `os.Rename`，owner 磁盘停留在旧名。

**根因**：`Rename` 对 pending 源一律跳过 wire rename，但 pending 只代表"索引没跟上"，`flush` 已把 body 传给 owner。

**修复**：`sourceOnOwner := !sourcePending || sourceFlushed`——owner 知道源名就发 wire rename。

### 挂载点不消失（1d）

**现象**：owner 下线后，tombstone 到达、supervisor 启动，但挂载点在预算内不消失。

**根因**：tombstone 被当成"连接断开"（`setDisconnected(true)`），reconnect 监督器 dial **server**（server 还活着）→ 重连成功 → 挂载点继续挂着。

**修复**：tombstone 不再设 disconnected，改为 `anyOwnerRemains()` 判断是否还有别的 owner——有则继续服务，没有则 `releaseMountPoint()`。

### 多 owner 语义

**现象**：任一 owner 下线会把整个挂载卸掉。

**修复**：tombstone 先判断 `anyOwnerRemains()`（列根目录、过滤 events 表中已知的 tombstone），有存活 owner 就继续服务。单测钉住两个场景。

---

## 五、技术决策（重要，不要推翻）

### 架构

- **D2**：可移植操作核 + 单一外壳，双平台都用 cgofuse
- **D3**：两级缓冲上限（单文件 64 MiB / 全局 256 MiB），128 KiB 块
- **D5**：`EventBody.Seq` + watcher 存活检测，失效走有界表
- **D10**：强制刷盘表（Fsync、Flush、Release、dirty Truncate、rename/unlink 前刷、unmount/SIGINT 时 flushAll）
- **D11**：退避监督器（1s → 30s），重连后 re-Watch + 全量失效，离线时返回 **EIO（绝不 ENOENT）**，2 分钟后自卸载，脏句柄丢弃并记日志
- **D12**：符号链接在读路径拒绝（`ELOOP`），但可删除/移动
- **D13**：Windows 在范围内

### 关键接口（已冻结，不要改签名）

- `viewer.Read(path string, off int64, n int) ([]byte, error)`
  - `n < 0` 表示读到末尾；`n == 0` 返回空
- `Caller` 接口 7 个方法：`List, Read, Put, Mkdir, Remove, Rename, Watch()`
- `node.Node = {Path, Kind, Size, Mode, Mtime}`，没有 `Seq` 字段
- 核心 `Mount.Open/Opendir/Create` 返回 `(uint64, int)`（句柄在前）
- cgofuse 返回 `(int, uint64)`（状态在前），适配层负责转换

### 挂载点

- Linux：`~/peerfold`；Windows：`%USERPROFILE%\peerfold`
- 绝不预创建：Linux 要求存在且为空，Windows 要求不存在
- WinFsp 自动创建，进程退出自动删除

### 错误码

- `ENOSYS = 40`；`ENOATTR = ENODATA = 120`
- 错误映射只在 `errnoFor` 一处做

---

## 六、后续规划

### 近期（确定要做）

1. **push 到远端**——目前所有提交都在本地 `main`，未推送。
2. **Linux 真实 FUSE 验证**——找一台有 `/dev/fuse` 的 Linux 机器，跑 `go test ./internal/mount`（或真实挂载 e2e），确认 cgofuse/Linux 路径正确。验收措辞保持"Windows 已验证 / Linux 待外部验证"。
3. **mount 进程日志落文件**——目前 mount 内部日志（重连、give-up、丢弃脏句柄）只到 stderr，不落文件。补 `~/peerfold-mount/log.txt`。

### 中期（值得考虑）

4. **增量同步**——目前整文件写回 + 原子替换，大文件频繁编辑低效。可考虑分块差异同步。
5. **认证/加密**——v1 明确不做，v2 如果要暴露到公网需要补。
6. **Linux 挂载点语义**——runner_linux.go 已写好（要求目录存在且为空），但从未真实跑过，可能有偏差。
7. **性能调优**——attr_timeout / entry_timeout 当前 1s，可根据实际使用调。

### 长期（待定）

8. 非星型拓扑（P2P / 多 server）
9. 冲突解决策略（当前是 last-writer-wins via 整文件覆盖）

---

## 七、关键文件位置

| 文件 | 作用 |
|---|---|
| `internal/mount/` | 挂载层操作核 |
| `internal/mount/host.go` | cgofuse 适配层 |
| `internal/mount/ops.go` | 挂载操作实现（Read/Write/Create/Mkdir...） |
| `internal/mount/path.go` | FUSE 路径清理和 lookup |
| `internal/mount/watch.go` | 事件应用、owner tombstone、断连状态 |
| `internal/mount/reconnect.go` | 重连、放弃释放、脏句柄丢弃 |
| `internal/mount/runner.go` | `Mount()` 阻塞循环封装 |
| `internal/mount/e2e_windows_test.go` | 真实挂载 e2e 测试 |
| `internal/server/server.go` | 聚合 server，请求转发、owner tombstone 广播 |
| `internal/client/client.go` | client 主体（上传树、读写请求处理） |
| `internal/client/watch.go` | fsnotify 监听 + 推送 |
| `internal/logging/logging.go` | 诊断日志 |
| `internal/config/config.go` | per-component 配置 |
| `cmd/peerfold/cmd_*.go` | CLI 各子命令 |
| `.artifacts/` | 本地测试输出（Git 忽略） |

---

## 八、开发约定

1. 外部反馈要怀疑，但要认真核对——不盲从，也不盲拒
2. 规则要记录，绝不僵住——遇到问题写 `Ruling: <做了什么> — <为什么> — <如果错了代价是什么>`
3. 不能失败的测试是负债——测试必须真的能失败
4. 绝不信任终端回显的非 ASCII——中文/特殊字符在 CP936 控制台可能乱码
5. Windows 环境：
   - `go test -run 'A|B'` 在 cmd.exe 中不可用，分开跑或跑整个包
   - `go fmt` 使用 `Join-Path (& go env GOROOT) 'bin\gofmt.exe'`
   - PowerShell 5.1 不转义 BOM-less UTF-8 脚本里的反引号
6. 直接在 `main` 上做（用户明确同意）
7. 缺工具就装（用户明确要求，Windows 必须支持）
8. 测试文件与被测包同目录（package internal tests），不要移到顶层 `tests/`

**离线构建要点**：本机无外网且默认 `GOMODCACHE` 为空，构建需：

```bash
GOMODCACHE=/c/Users/86151/AppData/Local/Temp/peerfold-gomodcache GOPROXY=off GOFLAGS=-mod=mod go build ./...
```
