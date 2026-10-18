# peerfold 交接文档

**日期**：2026-10-16  
**任务**：执行 peerfold v1 内核 + 挂载层开发计划  
**状态**：任务 1–15 已完成并提交（`main` 分支，未推送）。交接时工作区有未提交的挂载层调查代码（见下）。**2026-10-18 四个 Windows/WinFsp 验收边界（1a/1b/1c/1d）全部闭环**——根因分两处：mode 加宽只做了 owner（0700）没做 group/world（0777），以及 owner tombstone 误触发 server 重连。详见第七节。

---

## 一、这是什么项目

peerfold 是一个联邦文件系统：一台机器跑 `server`（聚合索引），若干机器跑 `client`（共享本地目录），一台机器跑 `viewer`/`mount`（把聚合后的树挂载成一个本地目录，像普通文件夹一样用）。

**v1 范围**（明确不做）：无内核挂载以外的深度特性——不做认证、不做加密、不做缓存分层、不做增量同步（整文件写回 + 原子替换）。拓扑是严格星型。

**冻结文档**（不要改）：
- 挂载层设计：`docs/superpowers/specs/2026-10-15-peerfold-mount-design.md`
- 挂载层计划：`docs/superpowers/plans/2026-10-15-peerfold-mount.md`
- 上游 v1 核心：`docs/superpowers/specs/2026-10-14-peerfold-design.md`
- 上游 v1 计划：`docs/superpowers/plans/2026-10-14-peerfold-v1-core.md`

---

## 二、当前进度

### 交接时工作区未提交的改动（重要）

交接时工作区有 6 个文件的未提交改动，围绕**pending content 读回窗口**。这批 pending-content 改动本身不解决 1a/1b（根因在 mode 加宽，见第七节），但其中 `pendingContent` 表被本次 rename 修复复用（用 `sourceFlushed` 判定 owner 是否已知源名）。构建和 `internal/mount` 单测均通过。

改动内容：
- `caller.go`：`Mount` 新增 `pendingContent map[string][]byte`，记录 pending 文件最后一次整体上传的 body
- `handle.go`：`flush` 在路径 pending 时把上传的 body 存入 `pendingContent`
- `ops.go`：`Getattr`/`Read`/`readTarget`/`Create`/`writeThroughPath` 在 index 无条目但有 pending content 时用它应答；`Unlink`/`Rmdir`/`Rename` 同步清理/搬移该表
- `watch.go`：watcher 上报路径时同时清掉 `pendingContent`
- `write_test.go`：4 个新单测钉住该窗口（`TestReadThroughPendingCreateAfterFlush` 等）

### 已完成并提交（`main` 分支，未推送）

| 提交 | 内容 |
|------|------|
| `e4e28fe` | 文档：更正 read-back 的归类——可能是适配层 bug 而非内核边界 |
| `cd3fefa` | 挂载层修复：pending 表跟随 unlink/rename 的名字 |
| `76efd54` | 文档：两个挂载层修复后的交接更新 |
| `60cbb80` | 挂载层修复：give-up 释放日志、onGiveUp 无条件安装、owner-gone e2e 引用 helper 日志 |
| `5efd2ca` | 挂载层修复：pending window 补齐 Open/Getattr/writeThroughPath，每个上报 mode 加宽为 rwX |
| `7fe9c5c` | 任务 15：真实挂载 e2e 测试 + WinFsp 兼容层 |
| `7806936` | 任务 14：`peerfold mount` 命令 + 配置项 |
| `8c04bbe` | 任务 13：平台 runner（Windows/Linux 挂载点语义差异） |
| `69f5e91` | 任务 12：cgofuse 适配层（`internal/mount/host.go`） |
| `7e0b631` | 任务 11：重连监督器 |
| `52e0c3c` | 任务 10：事件推送（`EventBody.Seq`） |
| … | 任务 1–9 同样完成 |

### 任务 15：真实挂载端到端测试

任务 15 已经完成到当前可验证边界：

- 新增真实 Windows 挂载测试：`internal/mount/e2e_windows_test.go`
- 新增并修复挂载路径边界：
  - `viewerCaller` 在 `internal/mount/host.go` 中通过 `toIndexPath` 去掉 FUSE 路径前导斜杠
  - `pendingCreate` / `pendingStat` 处理 WinFsp 在 `Create`/`Open` 后立即发出的属性查询
  - pending directory 与普通文件句柄区分，避免把文件误判为目录并返回 `EISDIR`
  - `Mkdir` 成功后通知内核失效新路径
  - `Flush` 对 WinFsp 目录哨兵句柄做安全处理
- 新增 owner 下线通知链路：
  - `internal/server/server.go` 在 client 断开时广播 owner tombstone
  - `internal/mount/watch.go` 收到 owner tombstone 后进入 disconnected 状态
  - `internal/mount/runner.go` 轮询间隔从 200ms 调整为 100ms
- 修复断连丢弃的重复上报：`discardDirtyOnDisconnect` 现在每个路径只上报一次
- 补充并修复相关单元测试和真实挂载回归测试

### 任务 15 之后的两个挂载层修复（`5efd2ca`、`60cbb80`）

**pending window 补齐：** `Opendir` 之前有 pending 路径，`Open`/`Getattr`/`writeThroughPath` 没有——一个刚由本挂载创建的文件在 owning client 的 watcher 上报之前对这三条回调不可见。`lookup` 现在对每条路径都查 pending 表，`Create` 把新名字记进去，`writeThroughPath` 对没有 owner 内容的名字跳过 owner 内容加载。新增五个单元测试钉住这个窗口：`TestLookupSeesAPendingDirectory`、`TestReaddirSeesAPendingDirectory`、`TestOpenSeesAPendingFile`、`TestGetattrSeesAPendingFile`、`TestWriteThroughPathSeesAPendingFile`。

**每个上报 mode 加宽为 rwX：** index 只记录类型位，所以每个节点之前上报为 mode 0；owner 记为 0644 的文件在挂载里打不开，owner 记为 0555 的目录拒绝每个子文件创建。适配层（`intToStat`）和操作核三个上报面（`Getattr`、`Readdir`、`pendingStat`）现在都给每个上报 mode 加上 owner 读/写/执行位。owner 自己的只读选择保留在 owner 的磁盘上；挂载是联邦面，通过挂载打开文件的是调用方，不是 owner。

**give-up 释放无条件安装：** `Runner` 之前只在有 redialler 时安装 `onGiveUp`，没有 redialler 的挂载在 owner 消失后只剩一行日志、挂载点还在。现在 `onGiveUp` 无条件安装，两个分支都记录内核对释放的应答。

### 任务 15 最近一次真实挂载 e2e 结果（2026-10-18 更新）

最近一次运行（`go test ./internal/mount -run '^TestE2ERealMount$' -count=1 -v`）：

- `TestE2ERealMount/list`：通过
- `TestE2ERealMount/read`：通过
- `TestE2ERealMount/write-inside-the-share`：通过
- `TestE2ERealMount/write-reaches-the-owners-disk`：通过
- `TestE2ERealMount/read-back-through-the-mount`：**通过**（1a 已闭环）
- `TestE2ERealMount/fsync-then-verify-the-owner`：通过
- `TestE2ERealMount/mkdir-rename-unlink`：**通过**（1b 已闭环）
- `TestE2ERealMount/editor-save-pattern`：**通过**（1c 已闭环）
- `TestE2ERealMount/directory-listing-matches-the-owner`：通过
- `TestE2ERealMount/unmount-when-the-owner-goes-away`：**通过**（1d 已闭环）

因此准确状态是：**核心 Windows 主路径与四个 Windows 特定验收边界全部通过（10/10，耗时 3.4s）。**

---

## 三、最新验证证据

2026-10-18 重新执行。**离线构建要点：默认 `GOMODCACHE`（`C:\Users\86151\go\pkg\mod`）为空且本机无外网，构建会失败；需把 `GOMODCACHE` 指到 `C:\Users\86151\AppData\Local\Temp\peerfold-gomodcache` 并设 `GOPROXY=off`（`GOFLAGS=-mod=mod`）。**

- `go build ./...`：exit 0
- `go test ./... -count=1`：11/11 包全部通过
- `go vet ./internal/mount`：通过
- 真实挂载 e2e：10/10 全过，耗时 3.4s

---

## 四、技术决策（重要，不要推翻）

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
  - `n < 0` 表示读到末尾
  - `n == 0` 返回空
- `Caller` 接口 7 个方法：`List, Read, Put, Mkdir, Remove, Rename, Watch()`
- `node.Node = {Path, Kind, Size, Mode, Mtime}`，没有 `Seq` 字段
- 核心 `Mount.Open/Opendir/Create` 返回 `(uint64, int)`（句柄在前）
- cgofuse 返回 `(int, uint64)`（状态在前），适配层负责转换

### 挂载点

- Linux：`~/peerfold`
- Windows：`%USERPROFILE%\peerfold`
- 绝不预创建挂载目录：Linux 要求存在且为空，Windows 要求不存在
- WinFsp 会自动创建挂载目录，进程退出时自动删除

### 错误码

- `ENOSYS = 40`
- `ENOATTR = ENODATA = 120`
- 错误映射只在 `errnoFor` 一处做

---

## 五、关键文件位置

| 文件 | 作用 |
|------|------|
| `internal/mount/` | 挂载层操作核 |
| `internal/mount/host.go` | cgofuse 适配层 |
| `internal/mount/ops.go` | 挂载操作实现（Read/Write/Create/Mkdir...） |
| `internal/mount/path.go` | FUSE 路径清理和 lookup |
| `internal/mount/watch.go` | 事件应用、owner tombstone、断连状态 |
| `internal/mount/reconnect.go` | 重连、给定放弃、脏句柄丢弃 |
| `internal/mount/runner.go` | `Mount()` 阻塞循环封装 |
| `internal/mount/e2e_windows_test.go` | 任务 15 真实挂载测试 |
| `internal/server/server.go` | client 下线时广播 owner tombstone |
| `internal/server/broadcast_test.go` | owner tombstone 回归测试 |
| `cmd/peerfold/cmd_mount.go` | `peerfold mount` 命令 |
| `internal/config/config.go` | mount 配置项 |
| `.superpowers/sdd/2026-10-15-peerfold-mount/progress.md` | 挂载层计划审计账本 |
| `.superpowers/toolchain/winfsp-sdk/` | WinFsp SDK |
| `.artifacts/` | 本地测试、e2e 和诊断输出目录（Git 忽略） |
| `.artifacts/tools/Procmon64.exe` | Sysinternals Process Monitor（2026-10-16 下载，用于内核级 trace） |
| `.artifacts/tools/trace1.pml` / `trace1.csv` | e2e 运行期间的完整内核 trace（4.4 GB PML / 1.7 GB CSV） |
| `.artifacts/tools/written-denied.csv` 等 | 按路径过滤后的关键事件（ACCESS DENIED / REPARSE） |
| `C:\Users\86151\AppData\Local\Temp\winfsp-clone` | WinFsp 源码浅克隆（master），用于驱动层/用户态库分析 |

---

## 六、测试与产物管理约定

项目保持 Go 标准的 **package internal tests** 结构：

```text
internal/mount/
  ops.go
  ops_test.go
  host.go
  host_test.go
  real_mount_windows_test.go
```

测试文件继续与被测包放在同一个目录中，因为大量测试需要访问包内未导出函数；不要为了“像其他项目”而移动到顶层 `tests/`。

本地输出统一放到 `.artifacts/`：

```text
.artifacts/
  test/
    unit/
    e2e/
  logs/
```

`.artifacts/` 已加入 `.gitignore`，以下根目录临时文件模式也已忽略：

```text
zz_*.out
zz_*.err
zz_*.log
zz_*.txt
zz_*.test
zz_*.exe
zz_*.ps1
```

真实挂载测试输出建议使用：

```powershell
$outDir = Join-Path '.artifacts' 'test\e2e'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
$out = Join-Path $outDir 'real-mount.out'
$err = Join-Path $outDir 'real-mount.err'

$env:PEERFOLD_E2E_MOUNT = '1'
go test ./internal/mount -run '^TestE2ERealMount$' -count=1 -v 1> $out 2> $err
```

测试产生的临时文件继续使用 `t.TempDir()`，不要写入仓库根目录。根目录下原有 210 个 `zz_*` 临时文件已清理。

---

## 七、遗留问题（按优先级）

### 交接后最新进展（2026-10-16 深度调查）

#### 已完成的调查动作（按顺序）

1. **复现**：重新跑真实挂载 e2e，四个 skip 依旧复现（`read-back`/`mkdir-rename-unlink`/`editor-save`/`unmount`）。证明交接工作区那批 pending content 改动（第二节）**不能解决 1a/1b**。
2. **挂载层 trace 分析**：`read-back-through-the-mount` 全程无一条 `Open`/`Read` 回调；`Create notify=true`、`Getattr pending=1`、`Write/Flush status=0` 全部正常——**操作核 pending 表与简单负缓存问题均已排除**。
3. **ProcMon 内核级抓取**（工具已下载至 `.artifacts/tools/Procmon64.exe`，trace 在 `.artifacts/tools/trace1.pml` / `trace1.csv`，4.4 GB，导出 CSV 后已按 `written.txt`/`sub` 过滤到 `.artifacts/tools/*.csv`）：
   - `CreateFile \Device\Volume{...}\written.txt` 返回 **ACCESS DENIED**，`Desired Access: Generic Read`，**挂载层 `Open` 回调 0 次触发**——拒绝发生在 WinFsp 驱动层，请求根本没下发到用户态。
   - `sub\a.txt`（1b）同样 `ACCESS DENIED` 且无 `Open` 回调，**1a/1b 是同一根因**。
   - 写路径 `Create/Write/Flush` 全部 `SUCCESS`，`Getattr` 后续返回 `status=0`——挂载层本身能工作。
4. **WinFsp 源码分析**（clone 至 `C:\Users\86151\AppData\Local\Temp\winfsp-clone`，浅克隆 master）：
   - `src/sys/create.c`：驱动层 Create 流程，转发用户态前有多处 `STATUS_ACCESS_DENIED` 返回点（EA/paging file/root-dir overwrite 等，均不匹配本场景）。
   - `src/dll/fsop.c`：`FspFileSystemCreateCheck`/`OpenCheck` → `FspAccessCheckEx`（`security.c:55`）——**请求转发给 FUSE 回调之前**先做 Windows DACL 访问检查。
   - `src/dll/security.c`：`FspAccessCheckEx` → `FspGetSecurityByName` 拿安全描述符 → Windows `AccessCheck()` → 失败返回 `STATUS_ACCESS_DENIED`。
   - `src/dll/fuse/fuse_intf.c`：`fsp_fuse_intf_GetSecurityByName` → `fsp_fuse_intf_GetSecurityEx` → `FspPosixMergePermissionsToSecurityDescriptor(Uid, Gid, Mode, ...)`。
   - `src/shared/ku/posix.c`：`FspPosixMapPermissionsToSecurityDescriptor` 把 POSIX mode 转成 DACL（owner/group/world 三 ACE + deny ACE），`FspPosixMapUidToSid` 做 UID→SID 映射。

#### 已确认的根因与修复（2026-10-18）

**根因不是 `Uid`/`Gid`，而是 mode 加宽只做了 owner（0700）。**

挂载层不设 `Uid`/`Gid`，WinFsp 用户态库把 uid=0/gid=0 映射到 `S-1-5-0`（**Nobody**，注意不是 World；World/Everyone 是 `S-1-1-0`）。`S-1-5-0` 永远不在调用方 token 里，所以 DACL 里唯一能命中调用方的是 **world ACE（`S-1-1-0` Everyone）**，它携带 world 权限位。而 `intToStat`（`host.go`）和操作核（`ops.go` 的 `Getattr`/`Readdir`/`pendingStat`）之前把 mode 加到 `0700`（只 owner rwx），world 位为 0 → `FspAccessCheckEx` → `AccessCheck()` 失败 → 驱动层 `STATUS_ACCESS_DENIED`，请求到不了 FUSE `Open`/`Create` 回调。

"写路径成功、读路径失败"的表面矛盾由此而来：`FILE_CREATE`（`fsop.c:421` `FspFileSystemOpCreate_FileCreate`）检查**父目录**的 `FILE_ADD_FILE`（父目录是已有索引项、world 位继承 owner 原始 mode），而 `FILE_OPEN`（`fsop.c:487` `FspFileSystemOpCreate_FileOpen`）检查**文件自身** DACL（刚创建的文件 mode=0700、world=0）。

**修复：加宽从 0700 改成 0777**（`fuse.S_IRWXU|S_IRWXG|S_IRWXO`），调用方以 world 身份拿到 rwx。改动点：`host.go:intToStat` + `ops.go` 五个 `0o700`→`0o777`。新增单测 `TestIntToStatWidensToWorldReadWriteExecute`、`TestGetattrCarriesTheOwnersPermissionBits` 断言 777。

**修复顺带暴露并解决了一个 rename bug：** `Rename` 之前对 pending 源一律跳过 wire rename（`if !sourcePending && !targetIsPending`），但 pending 只代表"索引没跟上"，不代表 owner 不知道——`flush` 已把 body 传给 owner（`pendingContent` 有记录）。于是 `os.WriteFile` 后立刻 `os.Rename`，owner 磁盘仍停留在旧名（`mkdir-rename-unlink` 里的 `sub/a.txt`→`sub/b.txt`）。现在改成 `sourceOnOwner := !sourcePending || sourceFlushed`，owner 知道源名就发 wire rename；本地 pending 表搬移同时清理目标旧条目。新增单测 `TestRenameOfAPendingFlushedFileReachesTheOwner`。

**测试框架还有一个句柄泄漏：** `startMountHelper` 打开 `mount-helper.log` 后父进程从不 `Close`，Windows 上该句柄挡住 `t.TempDir()` 清理（"used by another process"）。已在 `cmd.Start()` 后 `f.Close()`。

**1d（unmount）的根因是另一处：** owner tombstone 到达后，`watch.go` 把它当成了"连接断开"（`setDisconnected(true)`），于是 reconnect 监督器去 dial **server**——server 还活着，重连立刻成功、`adopt` 装上新连接，挂载点继续挂着（只是空树），永远不会 give up。修复：tombstone 不再设 `disconnected`，而是先 `anyOwnerRemains()`（列根、过滤掉 events 表里已知的 tombstone）判断是否还有别的 owner 存活——有则继续服务、继续 watch；没有则 `releaseMountPoint()`（`onGiveUp` → `host.Unmount()`）。`gaveUp` 把"释放挂载点"抽成 `releaseMountPoint()` 复用。新单测 `TestOwnerWithdrawalReleasesTheMount`（最后一个 owner 掉线才释放）+ `TestOwnerWithdrawalKeepsServingOtherOwners`（一个 owner 掉线、另一个继续服务）。

**结果（2026-10-18 全量真实挂载 e2e）：10/10 全过，耗时 3.4s（原来 183s）。** 1a（read-back）、1b（mkdir-rename-unlink）、1c（editor-save）、1d（unmount）全部闭环。1c 之所以一起修好：目标文件/父目录报 0777 后，`MOVEFILE_REPLACE_EXISTING` 的 delete-access 检查在驱动层通过，`Rename` 回调能到达。

（历史记录：Fix #1——`Create`/`Mkdir` 成功后对父目录补 `Notify(NOTIFY_CREATE)`——已试过并回退，四个 skip 依旧复现，证明不是父目录缓存失效。）

### 1. 四个 Windows/WinFsp 验收边界

（2026-10-18 更新：1a/1b/1c/1d 已全部闭环。根因是 mode 加宽只做 owner（1a/1b/1c）和 tombstone 误触发重连（1d），都不是"纯内核边界"——见第七节"已确认的根因与修复"。）

**关键观察（当时）：** `write-inside-the-share` 和 `fsync-then-verify-the-owner` 通过了——`os.Create` → `Write` → `Close` 在挂载里能工作。但 `os.ReadFile` 紧接着读同一个文件却失败（`read-back-through-the-mount`）。写路径能工作说明内核是接受这个挂载的，所以"纯内核边界"这个归类是错的——这个判断后来被证实：写走 `FILE_CREATE`（查父目录），读走 `FILE_OPEN`（查文件自身 DACL），两条路径的检查目标不同。

#### 1a. 读回刚写入的文件被拒（`read-back-through-the-mount`）

**现象：** `os.WriteFile` 成功，owner 磁盘上文件存在，但 `os.ReadFile` 通过挂载读回返回 `Access is denied`。

**2026-10-16 最新 trace 证据（`read-back-through-the-mount` 子测试全程无一条 `Open`/`Read` 回调记录）：**

```
Getattr enter /shared/shared/written.txt
Getattr status=-2 (ENOENT)
Create enter /shared/shared/written.txt
Create status=0 handle=62
Create notify=true                          ← host.Notify(NOTIFY_CREATE) 已调用
Getattr pending=1 /shared/shared/written.txt
Write off=0 len=25 fh=62 → n=25
Flush fh=62 → status=0                      ← 内容成功上传到 owner
...（后续 30 秒大量 Getattr status=0 与目录 Opendir，但全程无 Open/Read 回调）...
```

**已排除的原因：**
- 操作核 pending 表 bug —— 工作区未提交改动已覆盖 `Getattr`/`Read`/`Open` 对 pending 路径的应答，问题依旧复现
- 简单负缓存未失效 —— `Create notify=true` 已确认通知内核失效，且后续 `Getattr` 已能正确返回 `status=0`（不再是 ENOENT）
- 父目录缓存失效问题（Fix #1，已回退）—— `Create`/`Mkdir` 后补 `Notify(NOTIFY_CREATE)` on parent，e2e 依旧复现，已回退
- 挂载层 `Open` 回调本身有 bug —— ProcMon 证明请求根本没下发到用户态，`Open` 回调 0 次触发

**已解决（2026-10-18）：** 根因是 mode 加宽只做 owner（0700），world 位为 0；uid/gid=0 映射到 `S-1-5-0`（Nobody），调用方以 world（`S-1-1-0`）身份命中 world ACE、拿到 0 权限 → `STATUS_ACCESS_DENIED`。修复：加宽改成 0777。子测试已 PASS。

#### 1b. 刚创建目录的子文件创建被拒（`mkdir-rename-unlink`）

**现象：** `os.Mkdir` 成功，owner 磁盘上目录存在，但 `os.WriteFile` 在该目录下创建第一个子文件返回 `Access is denied`。

**与 1a 是同一个根因（已确认）：** ProcMon 显示 `sub\a.txt` 同样 `ACCESS DENIED` 且挂载层 `Open` 回调 0 次触发，与 1a 完全一致的模式——"刚通过挂载创建的东西，紧接着在驱动层被拒"。

**已解决（2026-10-18）：** 1a 的 0777 修复让第一个子文件创建（`FILE_ADD_FILE` 走父目录 world ACE）通过。但该子测试继续跑到 `os.Rename` 时又暴露一个独立 bug——pending 源即使已 flush 也跳过 wire rename，owner 磁盘停在旧名——已一并修掉（`sourceOnOwner := !sourcePending || sourceFlushed`）。子测试已 PASS。

#### 1c. 编辑器保存模式不工作（`editor-save-pattern`）

**现象：** `os.Rename` 覆盖已存在文件返回 `Access is denied`，trace 显示只到 `Flush`，`Rename` 回调根本没到。

**已解决（2026-10-18）：** 和 1a/1b 同一根因。`MOVEFILE_REPLACE_EXISTING` 在驱动层要查目标的 delete-access（文件自身 DELETE 或父目录 `FILE_DELETE_CHILD`），之前目标文件/父目录 world 位为 0 → 检查不过、`Rename` 回调到不了。0777 加宽后父目录 world 位含写（非 sticky 目录会加 `FILE_DELETE_CHILD`），检查通过，`Rename` 回调到达。子测试已 PASS。

#### 1d. owner 下线后挂载点未消失（`unmount-when-the-owner-goes-away`）

**现象：** owner tombstone 到达、supervisor 启动、`giving up` 记录在案、`onGiveUp` 已无条件安装，但挂载点在测试预算（180s，give-up 阈值 120s）内没有消失。

**已解决（2026-10-18）：** 根因是 tombstone 被当成"连接断开"（`setDisconnected(true)`），于是 reconnect 监督器去 dial **server**——server 还活着，重连立刻成功、`adopt` 装上新连接，挂载点继续挂着空树，永远不会 give up。修复：tombstone 不再设 `disconnected`，而是先 `anyOwnerRemains()` 判断是否还有别的 owner——有则继续服务、继续 watch；没有（最后一个）则 `releaseMountPoint()`。单 owner 场景子测试已 PASS（0.10s，原来 skip 要等满 180s）；多 owner 场景由单测钉住（一个掉线不卸载、最后一个掉线才卸载）。

### 2. Linux 外部验证

本机没有 `/dev/fuse`、没有 C 工具链、没有 libfuse 头文件，Linux FUSE 集成测试仍未在本机执行。验收措辞必须保持：

**Windows 已验证（核心主路径） / Linux 待外部验证**

不能写成"两平台都验证完成"。

---

## 八、工作方式（继续执行时要遵守）

1. 外部反馈要怀疑，但要认真核对——不盲从，也不盲拒
2. 规则要记录，绝不僵住——遇到问题写 `Ruling: <做了什么> — <为什么> — <如果错了代价是什么>`，然后继续
3. 不能失败的测试是负债——测试必须真的能失败
4. 绝不信任终端回显的非 ASCII——中文/特殊字符在 CP936 控制台可能乱码，用文件读取确认
5. Windows 环境陷阱：
   - `go test -run 'A|B'` 在 cmd.exe 中不可用，建议分开跑或跑整个包
   - `go fmt` 使用 `Join-Path (& go env GOROOT) 'bin\gofmt.exe'`
   - PowerShell 5.1 不转义 BOM-less UTF-8 脚本里的反引号
6. 直接在 `main` 上做（用户明确同意，不建 worktree）
7. 缺工具就装（用户明确要求，Windows 必须支持）
8. 自主连续执行，不要每步都问用户

---

## 九、下一步（给接手的人）

1. **四个 Windows/WinFsp 验收边界（1a/1b/1c/1d）已全部闭环**。根因两处：mode 加宽只做 owner（0777 修复 1a/1b/1c）+ tombstone 误触发重连（`releaseMountPoint` 修复 1d）。改动尚未提交，工作区还有交接时的 pending-content 调查代码与本次修复混在一起，提交前需区分。
2. 最后在 Linux 环境补做真实 FUSE 验证。
