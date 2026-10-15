# peerfold 挂载层 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让观察者上的普通程序通过一个真内核挂载点读写远端文件 —— 打开文件夹、双击、编辑、保存，peerfold 变成可以天天用的东西。

**Architecture:** 分成**可移植操作核**（`internal/mount/ops.go`，不导入任何 FUSE 库，只依赖一个 7 方法的 `Caller` 接口）和一个**平台壳**（`cgofuseHost`，把操作核适配成 cgofuse 的 `FileSystemInterface`）。两平台共用同一个 cgofuse API：Windows 走它的纯 Go 实现（`CGO_ENABLED=0` 可用），Linux 走 cgo。操作核是全部逻辑所在，也是唯一能在本机被完整测试的部分。

**Tech Stack:** Go 1.27.0、`github.com/winfsp/cgofuse v1.6.0`（FUSE 绑定）、WinFsp 2.1.25156（Windows 运行时，已装）、`github.com/fsnotify/fsnotify`（已有）、`internal/viewer`（已有，CLI 已证明可用）。

**Spec:** `docs/superpowers/specs/2026-10-15-peerfold-mount-design.md`（v4，已批准）

## Global Constraints

- **挂载点落点**：Linux `~/peerfold`；Windows `%USERPROFILE%\peerfold`。两平台都是**目录**，都不占盘符。
- **绝对不要预先创建挂载目录。** Linux 要目录**已存在**，Windows 要目录**不存在**（WinFsp 自己创建、进程退出时自动删除）。这个分支属于**平台壳**，操作核里不能有"先 Mkdir 挂载点"这个动作。
- **`Mount()` 会阻塞**。持有挂载的进程不能对挂载点做文件 IO。端到端测试里挂载进程要"粘住"，读写来自另一个进程。
- **读缓冲两级上限**：单一 open `64 MiB`（`--buffer`，块 `128 KiB`），所有 open 合计 `256 MiB`（`--buffer-total`）。
- **`viewer.Read` 的 `n` 用负数表示"读到结尾"**；`n == 0` 返回空切片而**不是错误**。挂载层必须传 `-1` 或正数。
- **元数据超时**：`attr_timeout`/`entry_timeout` = `1s`；`negative_timeout` = `0`。
- **`mount` 的一次只允许一个请求在飞**（`viewer.stream()` 是容量 1 的令牌），所以**同一个 `viewer` 连接的调用必须串行**。需要并发就开第二条连接。
- **错误码映射只允许出现在 `errnoFor` 一个地方**。挂载层其他任何位置**不允许**出现字符串比较或直接返回裸 errno。
- **`FileSystemInterface` 有 33 个方法，`FileSystemBase` 是空结构体（没有默认实现）**，所以 33 个全都要实现；不支持的返回 `-ENOSYS`。
- **符号链接必须拒绝**：`Getattr`/`Open`/`Create` 遇到 `Mode&ModeSymlink != 0` 返回 `-ELOOP`。**不是**当普通文件，**不是**跟随目标。
- **强制回写表**（每一条都必须在返回成功**之前**完成回写）：`Fsync`、涉及脏 open 的 `Rename`/`Unlink`/`Truncate`、`Release`/`Flush`、卸载与 SIGINT。
- **写入是整文件回写**（复用 `viewer.Put`）。写缓冲超限必须如实报 `-ENOSPC`，**不能悄悄截断用户的文件**。
- **`CGO_ENABLED=0` 且本机无 C 工具链**，所以 `-race` 不可用；**Linux 侧只能交叉编译，本机跑不了 Linux FUSE 集成测试**，计划里不得声称跑过。
- **`proto.EventBody.Seq` 与错误文本 token 前缀**是本层唯一要动 `proto`/`client`/`server` 的两处；不改帧结构、不改 `Kind` 已有取值、不加 `NodeJSON` 字段。
- **已有 9 个包共 276 个测试全绿、`go build ./...` 与 `go vet ./...` exit 0、`gofmt -l .` 为空** —— 每个任务结束时这四条都要保持。

## Review Focus

规格是一份愿景文档，它没说遇到下列输入会怎样，而它的沉默不等于这些输入可以把程序弄坏。这五条是最可能咬到使用者的：

1. **共享里存在符号链接，而程序对同一个路径做读-改-写。** 期望：拒绝（`ELOOP`）。索引现在报 9 字节、读却返回目标内容，写回会覆盖目标文件 —— 这是**静默丢失真实数据**，而且代码仓库和家目录里到处是符号链接。
2. **程序调用 `fsync` 之后立刻断电或杀掉供货方。** 期望：内容已落盘。整文件回写在 `Release` 才发生的实现会让 `fsync` 返回成功但什么都没写 —— 这是"保存悄悄丢数据"。
3. **观察者正开着文件时连接断了。** 期望：`EIO`，并且日志说明哪个路径的修改被丢弃。返回 `ENOENT` 会让程序以为文件被删、可能触发破坏性反应；静默重试回写会覆盖别人的修改。
4. **`server.broadcast` 因为一次推送超时把 watcher 永久删掉，但连接仍可用于 `List`/`Read`。** 期望：挂载层**察觉自己变聋**并恢复。现在的实现会永远服务陈旧的树，且没有任何信号 —— 用户看到的是一个撒谎的目录。
5. **内核在没有 `close` 的情况下要求落盘（`rename` 一个脏 open 的文件）。** 期望：先冲刷再改名，内容跟着走到新名字。编辑器写临时文件再改名就是这个模式，漏掉它等于"改完保存，内容还在内存里"。

---

## File Structure

**新建：**

| 文件 | 职责 |
|---|---|
| `internal/mount/caller.go` | `Caller` 接口（7 个方法）+ `Entry` + `Event`，操作核唯一的外部依赖 |
| `internal/mount/path.go` | 路径规范化、逃逸检查、按路径取单个节点 |
| `internal/mount/errno.go` | `errnoFor` —— 唯一允许做错误映射的地方 |
| `internal/mount/budget.go` | 读块缓存 + 全局内存预算（两级上限） |
| `internal/mount/handle.go` | open 句柄表：读状态、写缓冲、脏标记 |
| `internal/mount/ops.go` | 操作核：`Getattr`/`Readdir`/`Open`/`Read`/`Write`/`Create`/`Truncate`/`Flush`/`Release`/`Fsync`/`Mkdir`/`Unlink`/`Rmdir`/`Rename`/`Statfs` |
| `internal/mount/watch.go` | 事件泵、丢更新探测、"交出去的表"、作废 |
| `internal/mount/reconnect.go` | supervisor：退避重连、重连后重新订阅与全部作废、超阈值自卸载 |
| `internal/mount/host.go` | `cgofuseHost`：把操作核适配成 cgofuse 的 33 方法 `FileSystemInterface` |
| `internal/mount/runner.go` | 平台无关的挂载生命周期：挂载点检查、`Mount()` 阻塞、信号处理、卸载 |
| `internal/mount/runner_windows.go` | Windows 入口：**不预先创建挂载点**，走 WinFsp 自动创建 |
| `internal/mount/runner_linux.go` | Linux 入口：**必须预先创建**挂载点目录，且要检查它为空 |
| `cmd/peerfold/cmd_mount.go` | `peerfold mount` 子命令 |
| `internal/mount/e2e_windows_test.go` | 真实挂载的端到端测试（本机能跑） |

**修改：**

| 文件 | 改什么 |
|---|---|
| `internal/client/shares.go:167,207` | `Mode: uint32(info.Mode())`（原来 `.Perm()` 丢了类型位） |
| `internal/client/shares.go:210-215` | 符号链接索引进 `KindSymlink` 而不是 `KindFile` |
| `internal/client/watch.go:295-297` | 符号链接事件不要再静默丢弃，要反映到索引 |
| `internal/client/watch.go:320,331` | `Mode: uint32(info.Mode())` |
| `internal/client/walk_test.go:225-226` | 断言改成完整 mode（含类型位） |
| `internal/node/node.go` | 加 `KindSymlink`（**新取值**，不改已有 `KindFile=0`/`KindDir=1`/`KindDeleted=255`） |
| `internal/viewer/viewer.go` | 加定点错误与 token 前缀；`Watch` 侧暴露丢包计数/序号 |
| `internal/server/server.go:568-585` | `broadcast` 失败**不再永久删除 watcher**，改为发"你落后了" |
| `internal/server/server.go:270-277` | client 注册/注销时广播（用已有 `KindDeleted`） |
| `internal/proto/proto.go` | `EventBody` 加 `Seq uint64` |
| `cmd/peerfold/main.go` | 分发里加 `mount` |
| `internal/config/config.go` | 加 `mount_point`、`buffer_limit`、`buffer_total` |

**为什么这样分：** 操作核（`ops.go` 及其四个伙伴）不导入 cgofuse，所以它能在**任何平台**被完整测试 —— 这正是本机唯一能验证的部分。`host.go` 只做"把 cgofuse 的调用翻译成核的调用"，逻辑密度低但方法多（33 个）。平台差异只在两个 runner 文件里，而它们一共只有几十行。

---

## Task 1: 修符号链接的不一致（客户端侧，D12 的前半）

这是**唯一一个修既有数据损坏路径的任务**，所以它排在挂载层之前：挂载层要依赖 `Mode` 里有类型位才能拒绝符号链接。

**Files:**
- Modify: `internal/node/node.go`（加 `KindSymlink`）
- Modify: `internal/client/shares.go:167,207,210-215`
- Modify: `internal/client/watch.go:295-297,320,331`
- Test: `internal/client/symlink_test.go`（新建）

**Interfaces:**
- Consumes: 无（第一个任务）
- Produces: `node.KindSymlink`（取值 `2`）；索引里符号链接的节点满足 `n.Kind == node.KindSymlink` 且 `n.Mode&(1<<27) != 0`（`1<<27` = `os.ModeSymlink`）

- [ ] **Step 1: Write the failing test**

在 `internal/client/symlink_test.go` 新建。测试用一个临时目录建真实符号链接，跑扫描，断言符号链接**不是** `KindFile`、且 `Mode` 带类型位。

```go
package client

import (
	"os"
	"path/filepath"
	"testing"

	"peerfold/internal/node"
)

// A symlink must not be indexed as a regular file. Before this fix the scan
// used info.Mode().Perm(), which threw away the type bits, and decided the kind
// with d.IsDir(); a symlink therefore landed as KindFile with Size set to the
// length of the link target string. The mount layer then reported 9 bytes and
// read 5000, and a read-modify-write overwrote the real target.
func TestScanIndexesSymlinkAsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("real content here"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	sh, err := NewShares([]string{"d=" + dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()

	var got *node.Node
	for _, n := range sh.Nodes() {
		if n.Path == "d/link.txt" {
			c := n
			got = &c
		}
	}
	if got == nil {
		t.Fatal("d/link.txt not in index")
	}
	if got.Kind != node.KindSymlink {
		t.Errorf("link.txt Kind = %d, want KindSymlink (%d)", got.Kind, node.KindSymlink)
	}
	if got.Mode&(1<<27) == 0 {
		t.Errorf("link.txt Mode = %o, want the ModeSymlink bit (1<<27) set", got.Mode)
	}
}

// The regular file next to it must be unaffected.
func TestScanStillIndexesRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, err := NewShares([]string{"d=" + dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()

	for _, n := range sh.Nodes() {
		if n.Path == "d/a.txt" {
			if n.Kind != node.KindFile {
				t.Errorf("a.txt Kind = %d, want KindFile", n.Kind)
			}
			if n.Size != 3 {
				t.Errorf("a.txt Size = %d, want 3", n.Size)
			}
			if n.Mode&(1<<27) != 0 {
				t.Errorf("a.txt Mode = %o, must not have the symlink bit", n.Mode)
			}
			return
		}
	}
	t.Fatal("d/a.txt not in index")
}
```

> **实现者注意**：`NewShares` 的确切构造方式与"扫描结果怎么读"要看 `internal/client/shares.go` 与 `walk_test.go` 的现有用法；上文的 `sh.Nodes()` 是**占位**，如果现有 API 不是这个名字，用现有的那个，并把测试改成它的形态。**不要**为了让测试编译而给 `Shares` 加新方法。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run 'TestScanIndexesSymlinkAsSymlink|TestScanStillIndexesRegularFile' -v`
Expected: FAIL —— `link.txt Kind = 0, want KindSymlink (2)`（`KindSymlink` 还不存在时是编译错误，那也算 FAIL，先加常量再加断言）

- [ ] **Step 3: Add `KindSymlink` to `internal/node/node.go`**

在 `KindDir Kind = 1` 之后加，**不要改已有取值**：

```go
// KindSymlink is a symbolic link. The index used to record links as KindFile
// with the size of the link target string, which made a read of the path
// disagree with the stat; the mount layer refuses them with ELOOP.
KindSymlink Kind = 2
```

- [ ] **Step 4: Fix the scan in `internal/client/shares.go`**

三处改动：

1. 第 167 行：`Mode: uint32(rootInfo.Mode().Perm())` → `Mode: uint32(rootInfo.Mode())`
2. 第 207 行：`Mode: uint32(info.Mode().Perm())` → `Mode: uint32(info.Mode())`
3. 第 210-215 行的 kind 判断，加符号链接分支**在 `d.IsDir()` 之前**：

```go
switch {
case info.Mode()&os.ModeSymlink != 0:
	n.Kind = node.KindSymlink
case d.IsDir():
	n.Kind = node.KindDir
default:
	n.Kind = node.KindFile
	n.Size = info.Size()
}
```

注意 `Size` 对符号链接**保持 0**：现在填的是链接目标字符串长度，那个数字会让挂载层以为文件有内容。`d.Info()` 返回的是链接自身的 `FileInfo`（`os` 的文档明确写了"Info reports the information about the link itself, not the link's target"），所以 `info.Mode()` 里**有** `os.ModeSymlink` —— 这一步不需要改成 `os.Lstat`。

- [ ] **Step 5: Fix the watch path in `internal/client/watch.go`**

第 295-297 行现在**直接 `return`**，也就是符号链接的变更永远不进索引；但扫描又把它索引进去了，于是索引里的这条记录**从此不再更新**。改成发一个 `KindSymlink` 节点：

```go
	if info.Mode()&os.ModeSymlink != 0 {
		w.send([]node.Node{{
			Path:  rel,
			Kind:  node.KindSymlink,
			Mode:  uint32(info.Mode()),
			Mtime: info.ModTime().UnixNano(),
		}})
		return
	}
```

同时第 320 行与第 331 行的 `uint32(info.Mode().Perm())` → `uint32(info.Mode())`。

- [ ] **Step 6: Fix the existing assertion in `internal/client/walk_test.go`**

第 225-226 行现在断言 `Mode == uint32(info.Mode().Perm())`，这个断言**编码了那个 bug**。改成完整 mode：

```go
	if a.Mode != uint32(info.Mode()) {
		t.Errorf("Documents/a.txt Mode = %o, want %o", a.Mode, uint32(info.Mode()))
	}
```

- [ ] **Step 7: Run the full client package and the whole tree**

Run: `go test ./internal/client/ ./internal/node/ -v 2>&1 | tail -30`
Expected: PASS，含新加的两个测试

Run: `go test ./... 2>&1 | tail -15`
Expected: 九个包全 `ok`（`cmd/peerfold`、`internal/client`、`internal/config`、`internal/e2e`、`internal/index`、`internal/node`、`internal/proto`、`internal/server`、`internal/viewer`）

- [ ] **Step 8: Commit**

```bash
git add internal/node/node.go internal/client/shares.go internal/client/watch.go internal/client/walk_test.go internal/client/symlink_test.go
git commit -m "fix(client): index symlinks as KindSymlink, keep mode type bits

A symlink was indexed as KindFile with Size set to the length of the link
target string, because shares.go used info.Mode().Perm() (which drops the
type bits) and decided the kind with d.IsDir(). The owner's ReadAt calls
os.Open, which follows the link, so stat said 9 bytes and read returned the
target's contents. A read-modify-write over a mounted symlink would put those
9 bytes on top of the real file.

watch.go made it worse: it discarded symlink events outright, so a link that
the scan had indexed could never be updated again for the life of the
connection.

Mode now carries the full FileMode (same uint32 field, no wire change) and
links land in the new KindSymlink = 2. Existing Kind values are untouched."
```

---

## Task 2: `Caller` 接口与操作核骨架

**Files:**
- Create: `internal/mount/caller.go`
- Create: `internal/mount/path.go`
- Test: `internal/mount/path_test.go`

**Interfaces:**
- Consumes: `node.Node`（`Path`/`Kind`/`Size`/`Mode`/`Mtime`）、`node.KindSymlink`（Task 1）
- Produces:
  - `type Caller interface { List(path string) ([]node.Node, error); Read(path string, off int64, n int) ([]byte, error); Put(path string, r io.Reader) error; Mkdir(path string) error; Remove(path string) error; Rename(oldPath, newPath string) error; Watch() <-chan []node.Node }`
  - `func SplitParent(p string) (parent, base string, err error)` —— `"/a/b.txt"` → `"/a"`, `"b.txt"`；`"/"` → 错误 `errRoot`
  - `func Clean(p string) (string, error)` —— 拒绝逃逸（`..` 走出根）与非法输入
  - `func (m *Mount) lookup(path string) (node.Node, int)` —— 返回节点与 errno
  - `func (m *Mount) lookupParent(path string) (node.Node, string, int)` —— 父目录节点、基名、errno

> **核直接用 `node.Node`，不另造一个 `Entry`。** `node.Node` 是五个字段的普通值类型（`Path`/`Kind`/`Size`/`Mode`/`Mtime`），v1 的 276 个测试已经把它钉住了，而挂载层本来就要读 `Kind` 才能判断目录、文件和符号链接。再造一个平行类型只会多一层映射。`Caller` 因此是核唯一的外部依赖，且它依赖的是**已有并且已被测过**的类型。

- [ ] **Step 1: Write the failing test**

`internal/mount/path_test.go`：

```go
package mount

import (
	"testing"

	"peerfold/internal/node"
)

func TestCleanRejectsEscape(t *testing.T) {
	for _, in := range []string{"/../etc/passwd", "/a/../../b", "..", "/.."} {
		if got, err := Clean(in); err == nil {
			t.Errorf("Clean(%q) = %q, want an error (it escapes the root)", in, got)
		}
	}
}

func TestCleanAccepts(t *testing.T) {
	cases := map[string]string{
		"/":            "/",
		"":             "/",
		"/a":           "/a",
		"/a/b.txt":     "/a/b.txt",
		"/a//b":        "/a/b",
		"/a/./b":       "/a/b",
		"/a/b/":        "/a/b",
		"/中文/文件.txt": "/中文/文件.txt",
	}
	for in, want := range cases {
		got, err := Clean(in)
		if err != nil {
			t.Errorf("Clean(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitParent(t *testing.T) {
	parent, base, err := SplitParent("/a/b.txt")
	if err != nil || parent != "/a" || base != "b.txt" {
		t.Errorf("SplitParent(/a/b.txt) = %q, %q, %v", parent, base, err)
	}
	if _, _, err := SplitParent("/"); err == nil {
		t.Error("SplitParent(/) must fail: the root has no parent")
	}
}

func TestLookupRejectsSymlink(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/a")
	m.fake.addSymlink("/a/link")
	m.fake.addFile("/a/real.txt", 3)

	if _, code := m.lookup("/a/link"); code != -ELOOP {
		t.Errorf("lookup(symlink) = %d, want -ELOOP (%d)", code, -ELOOP)
	}
	if _, code := m.lookup("/a/real.txt"); code != 0 {
		t.Errorf("lookup(regular) = %d, want 0", code)
	}
}

func TestLookupMissingAndNotDir(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a/real.txt", 3)

	if _, code := m.lookup("/a/nope"); code != -ENOENT {
		t.Errorf("lookup(missing) = %d, want -ENOENT", code)
	}
	if _, code := m.lookup("/a/real.txt/x"); code != -ENOTDIR {
		t.Errorf("lookup(under file) = %d, want -ENOTDIR", code)
	}
}

func TestLookupRoot(t *testing.T) {
	m := newTestMount(t)
	n, code := m.lookup("/")
	if code != 0 || n.Kind != node.KindDir {
		t.Errorf("lookup(/) = %+v, %d; want a directory and 0", n, code)
	}
}
```

> **实现者注意**：`newTestMount` 与 `m.fake` 是**测试辅助**，在 Task 2 的 Step 3 里给出。`ELOOP`/`ENOENT`/`ENOTDIR` 这些常量来自 `internal/mount/errno.go`（Task 3）；如果 Task 3 还没做，这一步先在本文件顶部定义它们，**Task 3 会把它们移到 `errno.go` 并从那里引用**。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -v`
Expected: FAIL —— `undefined: Clean`、`undefined: SplitParent`、`undefined: newTestMount`

- [ ] **Step 3: Write `internal/mount/caller.go`**

```go
// Package mount presents a peerfold federation as an ordinary directory tree
// through a real kernel mount point.
//
// The package is split in two. This file and ops.go, path.go, errno.go,
// budget.go, handle.go, watch.go and reconnect.go are the portable operation
// core: they do not import a FUSE library, so they build and are tested on any
// platform. host.go is the adapter that presents the core to cgofuse, and the
// runner_*.go files carry the only genuinely platform-specific behaviour.
package mount

import (
	"io"

	"peerfold/internal/node"
)

// Caller is everything the operation core needs from a peerfold client. It is
// satisfied by *viewer.Viewer through viewerCaller (host.go); the tests use a
// fake instead, which is the point: the core has no dependency on the wire.
//
// Every method is called from the core's single goroutine, so an
// implementation does not need to be safe for concurrent use. That matches
// viewer.stream(), which allows exactly one request in flight.
type Caller interface {
	List(path string) ([]node.Node, error)
	Read(path string, off int64, n int) ([]byte, error)
	Put(path string, r io.Reader) error
	Mkdir(path string) error
	Remove(path string) error
	Rename(oldPath, newPath string) error
	// Watch returns a channel that is closed when the underlying connection
	// goes away. The core treats closure as "reconnect or give up", never as
	// "the tree is now empty".
	Watch() <-chan []node.Node
}

// errRoot is returned for requests about the mount root, which has no parent
// and cannot be renamed, removed or created.
var errRoot = errStr("mount: the root has no parent")

type errStr string

func (e errStr) Error() string { return string(e) }
```

- [ ] **Step 4: Write the test helper `newTestMount` and the fake caller in `internal/mount/fake_test.go`**

```go
package mount

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"peerfold/internal/node"
)

type fakeCaller struct {
	mu    sync.Mutex
	nodes map[string]node.Node
	data  map[string][]byte
	calls []string
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{nodes: map[string]node.Node{}, data: map[string][]byte{}}
}

func (f *fakeCaller) addDir(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[p] = node.Node{Path: p, Kind: node.KindDir, Mode: 0o755}
}

func (f *fakeCaller) addFile(p string, size int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[p] = node.Node{Path: p, Kind: node.KindFile, Size: int64(size), Mode: 0o644}
}

func (f *fakeCaller) addSymlink(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 1<<27 is os.ModeSymlink; the client records the full FileMode in Mode.
	f.nodes[p] = node.Node{Path: p, Kind: node.KindSymlink, Mode: 1 << 27}
}

func (f *fakeCaller) list(path string) []node.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []node.Node
	for _, n := range f.nodes {
		parent, _, err := SplitParent(n.Path)
		if err != nil {
			continue
		}
		if parent == path {
			out = append(out, n)
		}
	}
	return out
}

func (f *fakeCaller) List(path string) ([]node.Node, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "List "+path)
	f.mu.Unlock()
	if n, ok := f.nodes[path]; ok && n.Kind != node.KindDir {
		return nil, errNotDir
	}
	return f.list(path), nil
}

func (f *fakeCaller) Read(path string, off int64, n int) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "Read "+path)
	f.mu.Unlock()
	body, ok := f.data[path]
	if !ok {
		return nil, errNotFound
	}
	if off >= int64(len(body)) {
		return nil, nil
	}
	rest := body[off:]
	if n >= 0 && n < len(rest) {
		rest = rest[:n]
	}
	return append([]byte(nil), rest...), nil
}

func (f *fakeCaller) Put(path string, r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Put "+path)
	f.data[path] = body
	f.nodes[path] = node.Node{Path: path, Kind: node.KindFile, Size: int64(len(body)), Mode: 0o644}
	return nil
}

func (f *fakeCaller) Mkdir(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Mkdir "+path)
	f.nodes[path] = node.Node{Path: path, Kind: node.KindDir, Mode: 0o755}
	return nil
}

func (f *fakeCaller) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Remove "+path)
	delete(f.nodes, path)
	delete(f.data, path)
	return nil
}

func (f *fakeCaller) Rename(oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Rename "+oldPath+" "+newPath)
	if n, ok := f.nodes[oldPath]; ok {
		delete(f.nodes, oldPath)
		n.Path = newPath
		f.nodes[newPath] = n
	}
	if b, ok := f.data[oldPath]; ok {
		delete(f.data, oldPath)
		f.data[newPath] = b
	}
	return nil
}

func (f *fakeCaller) Watch() <-chan []node.Node { return make(chan []node.Node) }

func (f *fakeCaller) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newTestMount(t *testing.T) *Mount {
	t.Helper()
	f := newFakeCaller()
	f.addDir("/")
	return &Mount{caller: f, opts: DefaultOptions(), fake: f}
}
```

> **实现者注意**：`Mock` 上的 `fake` 字段是**测试专用**，为了不把 `_test.go` 的类型塞进生产结构体，请把它放在一个 `//go:build test` 之外的同包文件里但**标注清楚**；如果 Go 不允许（`_test.go` 里的类型不能被生产代码引用，但生产结构体可以有这个字段只要类型在 `_test.go` 里——**不行**），那就把 `fake` 字段改成 `any` 或者让测试自己持有一份 map。**这一处请实现者按 Go 的实际规则处理**，不要为了通过编译把测试钩子做进 `Caller` 接口。

- [ ] **Step 5: Write `internal/mount/path.go`**

```go
package mount

import (
	"path"
	"strings"

	"peerfold/internal/node"
)

// Clean normalises a FUSE path and refuses anything that would escape the
// mount root. FUSE hands us absolute slash-separated paths; the caller of this
// function is the last place that can notice a traversal attempt, because
// every layer below treats the path as opaque.
func Clean(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", errStr("mount: path must be absolute: " + p)
	}
	cleaned := path.Clean(p)
	// path.Clean("/../etc") == "/etc", which already refuses to climb past the
	// root, but it does so silently. Reject the attempt instead of quietly
	// serving a different path: a caller that asked for ".." is either buggy
	// or hostile, and neither should be answered with a plausible-looking file.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errStr("mount: path escapes the root: " + p)
		}
	}
	return cleaned, nil
}

// SplitParent returns the parent directory and the base name. The root has no
// parent and yields errRoot.
func SplitParent(p string) (parent, base string, err error) {
	if p == "/" || p == "" {
		return "", "", errRoot
	}
	parent = path.Dir(p)
	base = path.Base(p)
	if parent == "" {
		parent = "/"
	}
	return parent, base, nil
}

// lookup resolves a path to the index entry for it. It returns (node, 0) on
// success and (zero, -errno) on failure, so callers can return the code
// straight to cgofuse.
func (m *Mount) lookup(p string) (node.Node, int) {
	cleaned, err := Clean(p)
	if err != nil {
		return node.Node{}, -EINVAL
	}
	if cleaned == "/" {
		return node.Node{Path: "/", Kind: node.KindDir, Mode: uint32(m.opts.RootMode)}, 0
	}
	parent, base, err := SplitParent(cleaned)
	if err != nil {
		return node.Node{}, -EINVAL
	}
	children, err := m.caller.List(parent)
	if err != nil {
		return node.Node{}, errnoFor(err)
	}
	for _, c := range children {
		if path.Base(c.Path) == base {
			if c.Kind == node.KindSymlink {
				// A symlink's stat and its read disagree (the index records
				// the link, the owner opens and follows it). Refusing is the
				// only answer that cannot corrupt the target.
				return node.Node{}, -ELOOP
			}
			return c, 0
		}
	}
	// The parent exists but the name is not in it. If the parent was a file,
	// say so: that is a different error and programs branch on it.
	if _, code := m.lookup(parent); code == -ENOTDIR {
		return node.Node{}, -ENOTDIR
	}
	return node.Node{}, -ENOENT
}

// lookupParent returns the parent directory node and the base name.
func (m *Mount) lookupParent(p string) (node.Node, string, int) {
	cleaned, err := Clean(p)
	if err != nil {
		return node.Node{}, "", -EINVAL
	}
	parent, base, err := SplitParent(cleaned)
	if err != nil {
		return node.Node{}, "", -EINVAL
	}
	n, code := m.lookup(parent)
	if code != 0 {
		return node.Node{}, "", code
	}
	if n.Kind != node.KindDir {
		return node.Node{}, "", -ENOTDIR
	}
	return n, base, 0
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/mount/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/mount/
git commit -m "feat(mount): Caller interface, path handling, symlink refusal

The operation core talks to a seven-method Caller interface rather than to
*viewer.Viewer directly, which is what makes it testable without a wire and
without a FUSE library. path.go holds the only code that can notice a
traversal attempt, so it rejects a '..' segment outright instead of letting
path.Clean silently serve a different file."
```

---

## Task 3: 错误码映射（唯一允许做映射的地方，D6）

**Files:**
- Create: `internal/mount/errno.go`
- Test: `internal/mount/errno_test.go`
- Modify: `internal/client/client.go`（构造错误时加 token 前缀）
- Modify: `internal/server/server.go`（同上）

**Interfaces:**
- Consumes: 无
- Produces:
  - errno 常量：`EPERM=1 ENOENT=2 EIO=5 EACCES=13 EEXIST=17 EXDEV=18 ENOTDIR=20 EISDIR=21 EINVAL=22 ENOSPC=28 ENOSYS=38 ENOTEMPTY=41 ELOOP=114`
  - `func errnoFor(err error) int` —— 认 token 前缀，认不出返回 `-EIO`
  - token 常量：`tokenNotFound` `tokenNotDir` `tokenIsDir` `tokenNotEmpty` `tokenExists` `tokenPermission` `tokenCrossClient` `tokenOffline`
  - `func withToken(token string, err error) error` —— 生产端用，产出 `"<token>: <原文>"`
  - 定点错误：`errNotFound` `errNotDir` `errIsDir` `errNotEmpty` `errExists` `errPermission` `errCrossClient` `errOffline`

- [ ] **Step 1: Write the failing test**

`internal/mount/errno_test.go`：

```go
package mount

import (
	"errors"
	"testing"
)

func TestErrnoForKnownTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"enoent: no such file or directory", -ENOENT},
		{"enotdir: not a directory", -ENOTDIR},
		{"eisdir: is a directory", -EISDIR},
		{"enotempty: client \"win\" is not empty", -ENOTEMPTY},
		{"eexist: name is taken", -EEXIST},
		{"eacces: permission denied", -EACCES},
		{"exdev: cross-client rename", -EXDEV},
		{"eio: connection is gone", -EIO},
	}
	for _, c := range cases {
		if got := errnoFor(errors.New(c.text)); got != c.want {
			t.Errorf("errnoFor(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// An unrecognised error must never be guessed at. Guessing wrong between
// ENOENT and ENOTEMPTY is the difference between "the file is gone" and "this
// directory still has contents", and the second one is what protects data.
func TestErrnoForUnknownIsEIO(t *testing.T) {
	for _, text := range []string{
		"",
		"some other failure",
		"the system cannot find the file specified", // localized, no token
		"no such file or directory",                 // bare text, no token
		"enoent",                                    // token with no separator
		"ENOENT: shouty token",                      // tokens are case-sensitive
	} {
		if got := errnoFor(errors.New(text)); got != -EIO {
			t.Errorf("errnoFor(%q) = %d, want -EIO (%d)", text, got, -EIO)
		}
	}
}

func TestErrnoForNilIsZero(t *testing.T) {
	if got := errnoFor(nil); got != 0 {
		t.Errorf("errnoFor(nil) = %d, want 0", got)
	}
}

// The prefix must be a whole token, not a prefix of a longer word, or a
// message that happens to begin with those letters would be misclassified.
func TestErrnoForRequiresTokenBoundary(t *testing.T) {
	for _, text := range []string{"enoentish: not a token", "enoentx: not a token"} {
		if got := errnoFor(errors.New(text)); got != -EIO {
			t.Errorf("errnoFor(%q) = %d, want -EIO", text, got)
		}
	}
}

func TestWithTokenRoundTrips(t *testing.T) {
	err := withToken(tokenNotFound, errors.New("no such file or directory"))
	if got := errnoFor(err); got != -ENOENT {
		t.Errorf("errnoFor(withToken(...)) = %d, want -ENOENT", got)
	}
	if err.Error() != "enoent: no such file or directory" {
		t.Errorf("text = %q, want the token prefixed to the original", err.Error())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run TestErrno -v`
Expected: FAIL —— `undefined: errnoFor`、`undefined: ENOENT`

- [ ] **Step 3: Write `internal/mount/errno.go`**

```go
package mount

import (
	"errors"
	"fmt"
	"strings"
)

// The errno values cgofuse uses on both platforms. They are declared here
// rather than taken from syscall, because syscall's values are
// platform-specific and the core must build everywhere. The values are the
// package's own constants, measured: ENOENT=2 EIO=5 EACCES=13 EPERM=1
// EXDEV=18 EEXIST=17 ENOTDIR=20 EISDIR=21 EINVAL=22 ENOSPC=28 ENOTEMPTY=41
// ELOOP=114.
const (
	EPERM     = 1
	ENOENT    = 2
	EIO       = 5
	EACCES    = 13
	EEXIST    = 17
	EXDEV     = 18
	ENOTDIR   = 20
	EISDIR    = 21
	EINVAL    = 22
	ENOSPC    = 28
	ENOSYS    = 38
	ENOTEMPTY = 41
	ELOOP     = 114
)

// Error tokens. The wire carries errors as free text (the Err field is a
// string) and the owner's error is operating-system-specific and localized:
// the same missing file says "no such file or directory" on Linux and "The
// system cannot find the file specified" on Windows. Matching that text from
// the mount layer is not maintainable, so producers prefix a stable token and
// errnoFor reads only the token.
const (
	tokenNotFound    = "enoent"
	tokenNotDir      = "enotdir"
	tokenIsDir       = "eisdir"
	tokenNotEmpty    = "enotempty"
	tokenExists      = "eexist"
	tokenPermission  = "eacces"
	tokenCrossClient = "exdev"
	tokenOffline     = "eio"
)

// withToken prefixes an error with a stable token, keeping the original text
// for a human reader.
func withToken(token string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", token, err)
}

// errnoFor is the only place in the mount layer that converts an error into an
// errno. An error it does not recognise becomes EIO, which is the conservative
// direction: reporting a failure is better than reporting a success the caller
// will act on.
func errnoFor(err error) int {
	if err == nil {
		return 0
	}
	// Sentinels first, so a wrapped sentinel works without any text at all.
	switch {
	case errors.Is(err, errNotFound):
		return -ENOENT
	case errors.Is(err, errNotDir):
		return -ENOTDIR
	case errors.Is(err, errIsDir):
		return -EISDIR
	case errors.Is(err, errNotEmpty):
		return -ENOTEMPTY
	case errors.Is(err, errExists):
		return -EEXIST
	case errors.Is(err, errPermission):
		return -EACCES
	case errors.Is(err, errCrossClient):
		return -EXDEV
	case errors.Is(err, errOffline):
		return -EIO
	}
	if token, ok := tokenOf(err.Error()); ok {
		return errnoForToken(token)
	}
	return -EIO
}

// tokenOf extracts a leading "<token>: " prefix. The separator is required so
// that a message merely beginning with those letters is not misread.
func tokenOf(text string) (string, bool) {
	i := strings.Index(text, ": ")
	if i <= 0 {
		return "", false
	}
	return text[:i], true
}

func errnoForToken(token string) int {
	switch token {
	case tokenNotFound:
		return -ENOENT
	case tokenNotDir:
		return -ENOTDIR
	case tokenIsDir:
		return -EISDIR
	case tokenNotEmpty:
		return -ENOTEMPTY
	case tokenExists:
		return -EEXIST
	case tokenPermission:
		return -EACCES
	case tokenCrossClient:
		return -EXDEV
	case tokenOffline:
		return -EIO
	}
	return -EIO
}

// Sentinel errors, used in tests and wherever an error never reaches the wire.
var (
	errNotFound    = errors.New(tokenNotFound + ": not found")
	errNotDir      = errors.New(tokenNotDir + ": not a directory")
	errIsDir       = errors.New(tokenIsDir + ": is a directory")
	errNotEmpty    = errors.New(tokenNotEmpty + ": directory is not empty")
	errExists      = errors.New(tokenExists + ": name is taken")
	errPermission  = errors.New(tokenPermission + ": permission denied")
	errCrossClient = errors.New(tokenCrossClient + ": cross-client rename")
	errOffline     = errors.New(tokenOffline + ": the connection is gone")
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mount/ -run TestErrno -v`
Expected: PASS

- [ ] **Step 5: Add the tokens to the producers**

在 `internal/client/` 里把"名字已存在 / 不存在 / 不是目录 / 目录非空 / 权限"这几类错误用 `withToken` 包起来，**在错误产生的最近处**（能判断出类别的那一行），不要在传输层猜。具体位置由实现在 `internal/client/` 里搜索现有错误构造点决定。

同样处理 `internal/server/`：`replyRemove` 等处的 `client: %q is not empty` 要变成 `enotempty: client: %q is not empty`。

**不要**改错误的人类可读部分 —— 现有 CLI 输出与测试断言依赖它，token 只是**前缀**。

> **一个已知的先例**：`internal/viewer` 的 `ErrCrossClient` 就是这条路的原型（它在发出任何帧之前就拒绝了），新建的定点错误与它同一形态。

- [ ] **Step 6: Add the coverage test that finds missing tokens**

`internal/mount/errno_test.go` 追加：

```go
// Every condition the mount layer must distinguish has to be enumerated here.
// A missing entry degrades to EIO, which is safe but wrong, and safe-and-wrong
// is exactly the kind of bug no other test will notice.
func TestEveryClassifiedErrorCarriesAToken(t *testing.T) {
	cases := []struct {
		what string
		err  error
		want int
	}{
		{"remove non-empty directory", withToken(tokenNotEmpty, errors.New(`client: "win" is not empty`)), -ENOTEMPTY},
		{"read missing file", withToken(tokenNotFound, errors.New("no such file")), -ENOENT},
		{"mkdir over a file", withToken(tokenExists, errors.New("name is taken")), -EEXIST},
		{"list under a file", withToken(tokenNotDir, errors.New("not a directory")), -ENOTDIR},
		{"rename across clients", withToken(tokenCrossClient, errors.New("cross-client rename")), -EXDEV},
	}
	for _, c := range cases {
		if got := errnoFor(c.err); got != c.want {
			t.Errorf("%s: errnoFor = %d, want %d", c.what, got, c.want)
		}
	}
}
```

> **实现者注意**：请把 `internal/client/` 与 `internal/server/` 里**所有**能对应到上述 errno 的错误构造点都列进来并保持一一对应。清单不完整是这个测试唯一会失效的方式。

- [ ] **Step 7: Run the whole tree**

Run: `go test ./... 2>&1 | tail -15`
Expected: 九个包全 `ok`

- [ ] **Step 8: Commit**

```bash
git add internal/mount/errno.go internal/mount/errno_test.go internal/client/ internal/server/
git commit -m "feat(mount): map errors by stable token prefix, never by message text

The wire carries errors as free text and the owner's error is
operating-system-specific and localized: the same missing file reads 'no such
file or directory' on Linux and 'The system cannot find the file specified' on
Windows. Matching that text from the mount layer would be unmaintainable, and a
wrong guess between ENOENT and ENOTEMPTY is the difference between 'the file is
gone' and 'this directory still has contents'.

Producers now prefix a token (enoent:, enotempty:, ...) and keep the original
text after it, so existing CLI output is unchanged. errnoFor reads only the
token and returns EIO for anything it does not recognise; tests pin both the
known tokens and the refusal to guess."
```

---

## Task 4: 读块缓冲与全局内存预算（D3 的两级上限）

**Files:**
- Create: `internal/mount/budget.go`
- Test: `internal/mount/budget_test.go`

**Interfaces:**
- Consumes: Task 3 的错误码
- Produces:
  - `const chunkSize = 128 << 10`
  - `func newChunkCache(perOpen, global int) *chunkCache`
  - `func (c *chunkCache) get(fh uint64, chunk int64) ([]byte, bool)`
  - `func (c *chunkCache) put(fh uint64, chunk int64, data []byte)`
  - `func (c *chunkCache) dropOpen(fh uint64)`
  - `func (c *chunkCache) total() int`
  - `func (c *chunkCache) openTotal(fh uint64) int`

- [ ] **Step 1: Write the failing test**

`internal/mount/budget_test.go`：

```go
package mount

import "testing"

func TestChunkCacheHitsAndMisses(t *testing.T) {
	c := newChunkCache(1<<20, 4<<20)
	if _, ok := c.get(1, 0); ok {
		t.Fatal("empty cache reported a hit")
	}
	c.put(1, 0, []byte("hello"))
	got, ok := c.get(1, 0)
	if !ok || string(got) != "hello" {
		t.Fatalf("get after put = %q, %v", got, ok)
	}
	if _, ok := c.get(1, 1); ok {
		t.Error("a different chunk must not hit")
	}
	if _, ok := c.get(2, 0); ok {
		t.Error("a different handle must not hit")
	}
}

// The per-open limit bounds one handle; only the global limit bounds the
// process. The original design said memory was pinned at 64 MiB "no matter how
// large the file", which is true for one file and false for N.
func TestPerOpenLimitEvicts(t *testing.T) {
	perOpen := 3 * len("xxxx")
	c := newChunkCache(perOpen, 1<<20)
	for i := int64(0); i < 5; i++ {
		c.put(1, i, []byte("xxxx"))
	}
	if got := c.openTotal(1); got > perOpen {
		t.Errorf("openTotal = %d, exceeds the per-open limit %d", got, perOpen)
	}
}

// The test the original design could not have written, because it claimed a
// single knob bounded everything.
func TestGlobalLimitBoundsAllOpens(t *testing.T) {
	global := 8 * len("xxxx")
	c := newChunkCache(1<<20, global)
	for fh := uint64(1); fh <= 10; fh++ {
		for i := int64(0); i < 20; i++ {
			c.put(fh, i, []byte("xxxx"))
		}
	}
	if got := c.total(); got > global {
		t.Errorf("total = %d, exceeds the global limit %d", got, global)
	}
}

func TestDropOpenReleasesItsMemory(t *testing.T) {
	c := newChunkCache(1<<20, 1<<20)
	c.put(1, 0, []byte("aaaa"))
	c.put(2, 0, []byte("bbbb"))
	before := c.total()
	c.dropOpen(1)
	if c.total() >= before {
		t.Errorf("total did not shrink: %d then %d", before, c.total())
	}
	if _, ok := c.get(1, 0); ok {
		t.Error("dropped handle still hits")
	}
	if _, ok := c.get(2, 0); !ok {
		t.Error("dropping one handle must not disturb another")
	}
}

// A chunk larger than the whole budget must not be cached, and must not panic.
func TestOversizedChunkIsNotCached(t *testing.T) {
	c := newChunkCache(4, 4)
	c.put(1, 0, []byte("this is much longer than the budget"))
	if c.total() > 4 {
		t.Errorf("total = %d, want it bounded by 4", c.total())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestChunkCache|TestPerOpen|TestGlobal|TestDropOpen|TestOversized' -v`
Expected: FAIL —— `undefined: newChunkCache`

- [ ] **Step 3: Write `internal/mount/budget.go`**

```go
package mount

import "container/list"

// chunkSize is the unit a ranged read is broken into. proto.MaxPayload is
// 1 MiB and the probe measured the per-frame cost rising with frame size
// (64 KiB took 533us, 256 KiB 1000us, 1 MiB 3000us), so 1 MiB — a full frame —
// is the worst choice. 128 KiB stays inside the frame with room to spare.
const chunkSize = 128 << 10

type chunkKey struct {
	fh    uint64
	chunk int64
}

// chunkCache holds read chunks for every open file under two limits: a
// per-open cap so one large file cannot crowd out the others, and a global cap
// so the process's memory is actually bounded. The original design had only
// the first and described it as bounding everything, which is true for one
// open and false for N.
type chunkCache struct {
	perOpen int
	global  int
	bytes   map[uint64]int
	items   map[chunkKey][]byte
	// order is the eviction queue, most recently used at the back.
	order *list.List
	index map[chunkKey]*list.Element
}

func newChunkCache(perOpen, global int) *chunkCache {
	return &chunkCache{
		perOpen: perOpen,
		global:  global,
		bytes:   map[uint64]int{},
		items:   map[chunkKey][]byte{},
		order:   list.New(),
		index:   map[chunkKey]*list.Element{},
	}
}

func (c *chunkCache) get(fh uint64, chunk int64) ([]byte, bool) {
	k := chunkKey{fh, chunk}
	data, ok := c.items[k]
	if !ok {
		return nil, false
	}
	if el, ok := c.index[k]; ok {
		c.order.MoveToBack(el)
	}
	return data, true
}

func (c *chunkCache) put(fh uint64, chunk int64, data []byte) {
	if len(data) > c.global {
		// Larger than the entire budget: caching it would evict everything
		// else and still not fit, so serve it and forget it.
		return
	}
	k := chunkKey{fh, chunk}
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = data
	c.index[k] = c.order.PushBack(k)
	c.bytes[fh] += len(data)
	c.evictOpen(fh)
	c.evictGlobal()
}

// evictOpen trims one handle down to the per-open cap, oldest first.
func (c *chunkCache) evictOpen(fh uint64) {
	for c.bytes[fh] > c.perOpen {
		el := c.oldestOf(fh)
		if el == nil {
			return
		}
		c.removeElement(el)
	}
}

// evictGlobal trims the whole cache to the global cap, evicting across handles
// oldest-first. This is what keeps N concurrent opens bounded.
func (c *chunkCache) evictGlobal() {
	for c.total() > c.global {
		el := c.order.Front()
		if el == nil {
			return
		}
		c.removeElement(el)
	}
}

func (c *chunkCache) oldestOf(fh uint64) *list.Element {
	for el := c.order.Front(); el != nil; el = el.Next() {
		if el.Value.(chunkKey).fh == fh {
			return el
		}
	}
	return nil
}

func (c *chunkCache) removeElement(el *list.Element) {
	k := el.Value.(chunkKey)
	data := c.items[k]
	delete(c.items, k)
	delete(c.index, k)
	c.bytes[k.fh] -= len(data)
	if c.bytes[k.fh] <= 0 {
		delete(c.bytes, k.fh)
	}
	c.order.Remove(el)
}

func (c *chunkCache) dropOpen(fh uint64) {
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if el.Value.(chunkKey).fh == fh {
			c.removeElement(el)
		}
		el = next
	}
	delete(c.bytes, fh)
}

func (c *chunkCache) total() int {
	n := 0
	for _, b := range c.bytes {
		n += b
	}
	return n
}

func (c *chunkCache) openTotal(fh uint64) int { return c.bytes[fh] }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mount/ -run 'TestChunkCache|TestPerOpen|TestGlobal|TestDropOpen|TestOversized' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mount/budget.go internal/mount/budget_test.go
git commit -m "feat(mount): read chunk cache with per-open and global limits

The first design of this claimed a 64 MiB cap pinned memory 'no matter how
large the file'. That holds for one open and is false for N, so the CLI would
have had one knob that bounded nothing in the case that matters. There are now
two limits, and TestGlobalLimitBoundsAllOpens is the test the old design could
not have written."
```

---

## Task 5: open 句柄表（读状态 + 写缓冲 + 脏标记）

**Files:**
- Create: `internal/mount/handle.go`
- Test: `internal/mount/handle_test.go`

**Interfaces:**
- Consumes: Task 3（错误码）、Task 4（`chunkCache`）
- Produces:
  - `type Mount struct { ... }`（**在这里定型**，后续任务只往里加字段）
  - `func New(caller Caller, opts Options) *Mount`
  - `type Options struct { BufferLimit, BufferTotal int; AttrTimeout, EntryTimeout time.Duration; RootMode uint32; WriteLimit int }`
  - `func DefaultOptions() Options` —— `64<<20`/`256<<20`/`1s`/`1s`/`0o755`/`8<<20`
  - `type openFile struct { path string; fh uint64; kind node.Kind; dirty bool; writes map[int64][]byte; size int64; writeBytes int; cache *chunkCache }`
  - `func (m *Mount) newHandle(path string, kind node.Kind) uint64`
  - `func (m *Mount) handle(fh uint64) (*openFile, bool)`
  - `func (m *Mount) closeHandle(fh uint64)`
  - `func (o *openFile) write(off int64, data []byte) int` —— 返回 errno
  - `func (o *openFile) content() []byte` —— 把稀疏的写缓冲拼成完整内容
  - `func (o *openFile) truncateTo(size int64) int`

- [ ] **Step 1: Write the failing test**

`internal/mount/handle_test.go`：

```go
package mount

import (
	"testing"

	"peerfold/internal/node"
)

func TestHandleAllocationAndLookup(t *testing.T) {
	m := newTestMount(t)
	a := m.newHandle("/a.txt", node.KindFile)
	b := m.newHandle("/b.txt", node.KindFile)
	if a == b {
		t.Fatal("two handles share a number")
	}
	if _, ok := m.handle(a); !ok {
		t.Error("allocated handle not found")
	}
	m.closeHandle(a)
	if _, ok := m.handle(a); ok {
		t.Error("closed handle still found")
	}
	if _, ok := m.handle(b); !ok {
		t.Error("closing one handle disturbed another")
	}
	// A closed handle must not be handed out again while the process runs,
	// because the kernel may still hold the old number.
	if c := m.newHandle("/c.txt", node.KindFile); c == a {
		t.Error("handle number was recycled")
	}
}

func TestWriteInsideAndBeyondCurrentSize(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandle("/f", node.KindFile)
	h, _ := m.handle(fh)

	if code := h.write(0, []byte("hello")); code != 0 {
		t.Fatalf("write = %d", code)
	}
	if got := string(h.content()); got != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	if !h.dirty {
		t.Error("a write must mark the handle dirty")
	}
	// A write past the end must be zero-filled, not silently joined.
	if code := h.write(8, []byte("!")); code != 0 {
		t.Fatalf("sparse write = %d", code)
	}
	if got := string(h.content()); got != "hello\x00\x00\x00!" {
		t.Errorf("sparse content = %q", got)
	}
}

func TestWriteOverwritesInPlace(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandle("/f", node.KindFile)
	h, _ := m.handle(fh)
	h.write(0, []byte("hello"))
	h.write(1, []byte("EY"))
	if got := string(h.content()); got != "hEYlo" {
		t.Errorf("content = %q, want %q", got, "hEYlo")
	}
}

// A write that would push the buffer past its limit must fail loudly. Silently
// truncating would be "saving quietly loses data", which is the failure this
// whole layer exists to avoid.
func TestWriteOverLimitIsRefused(t *testing.T) {
	m := newTestMount(t)
	m.opts.WriteLimit = 8
	fh := m.newHandle("/f", node.KindFile)
	h, _ := m.handle(fh)
	if code := h.write(0, []byte("12345678")); code != 0 {
		t.Fatalf("exactly at the limit should succeed, got %d", code)
	}
	if code := h.write(8, []byte("x")); code != -ENOSPC {
		t.Errorf("write past the limit = %d, want -ENOSPC (%d)", code, -ENOSPC)
	}
	// And the earlier content must be intact.
	if got := string(h.content()); got != "12345678" {
		t.Errorf("content after refusal = %q, want it unchanged", got)
	}
}

func TestTruncateShrinksAndGrows(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandle("/f", node.KindFile)
	h, _ := m.handle(fh)
	h.write(0, []byte("hello world"))
	if code := h.truncateTo(5); code != 0 {
		t.Fatalf("truncate = %d", code)
	}
	if got := string(h.content()); got != "hello" {
		t.Errorf("after shrink = %q, want %q", got, "hello")
	}
	if !h.dirty {
		t.Error("truncate must mark the handle dirty")
	}
	if code := h.truncateTo(8); code != 0 {
		t.Fatalf("grow = %d", code)
	}
	if got := string(h.content()); got != "hello\x00\x00\x00" {
		t.Errorf("after grow = %q", got)
	}
}

func TestSeededSizeSurvivesNoWrites(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandle("/f", node.KindFile)
	h, _ := m.handle(fh)
	h.size = 100
	if h.dirty {
		t.Error("a fresh handle must not be dirty")
	}
	if len(h.content()) != 100 {
		t.Errorf("content length = %d, want 100", len(h.content()))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run TestHandle -v`
Expected: FAIL —— `undefined: Mount`、`undefined: newHandle`

- [ ] **Step 3: Write `internal/mount/handle.go`**

```go
package mount

import (
	"sort"
	"time"

	"peerfold/internal/node"
)

// Options carries everything the operator can tune. The two buffer numbers are
// the D3 pair: one bounds a handle, the other bounds the process.
type Options struct {
	BufferLimit  int           // per open
	BufferTotal  int           // all opens together
	WriteLimit   int           // one open's accumulated writes
	AttrTimeout  time.Duration
	EntryTimeout time.Duration
	RootMode     uint32
}

func DefaultOptions() Options {
	return Options{
		BufferLimit:  64 << 20,
		BufferTotal:  256 << 20,
		WriteLimit:   8 << 20,
		AttrTimeout:  time.Second,
		EntryTimeout: time.Second,
		RootMode:     0o755,
	}
}

// Mount is the operation core. Its methods return an errno the way cgofuse
// wants it (0 or a negative value), not a Go error, because that is the shape
// of the interface on both platforms and converting once at the boundary is
// cheaper than converting at every call site.
type Mount struct {
	caller Caller
	opts   Options

	// fh numbers are never reused within a run: the kernel can hold a stale
	// number after a release, and handing the same number to a different file
	// would let that stale reference touch the wrong path.
	nextFh uint64
	files  map[uint64]*openFile
	cache  *chunkCache
}

func New(caller Caller, opts Options) *Mount {
	return &Mount{
		caller: caller,
		opts:   opts,
		files:  map[uint64]*openFile{},
		cache:  newChunkCache(opts.BufferLimit, opts.BufferTotal),
	}
}

// openFile is one open handle: the read cache it uses and the writes it has
// accumulated but not yet flushed.
//
// Writes accumulate in memory because the wire has no partial write — a Put
// carries a whole file. So a handle can be dirty for its entire life and the
// flush happens at release, fsync, or before any operation that would move or
// delete the path underneath it.
type openFile struct {
	path  string
	fh    uint64
	kind  node.Kind
	size  int64
	dirty bool

	// writes maps the start offset of each accumulated write run to its
	// bytes. Two runs that overlap are coalesced in content().
	writes     map[int64][]byte
	writeBytes int
}

func (m *Mount) newHandle(path string, kind node.Kind) uint64 {
	m.nextFh++
	fh := m.nextFh
	m.files[fh] = &openFile{
		path:   path,
		fh:     fh,
		kind:   kind,
		writes: map[int64][]byte{},
	}
	return fh
}

func (m *Mount) handle(fh uint64) (*openFile, bool) {
	h, ok := m.files[fh]
	return h, ok
}

func (m *Mount) closeHandle(fh uint64) {
	delete(m.files, fh)
	m.cache.dropOpen(fh)
}

// write records a write at off. It returns 0 or -ENOSPC; it never truncates.
func (o *openFile) write(off int64, data []byte) int {
	if off < 0 {
		return -EINVAL
	}
	end := off + int64(len(data))
	if end > o.size {
		o.size = end
	}
	// The limit is on the handle's accumulated bytes, and it is checked before
	// recording so a refusal leaves the previous content untouched.
	grown := len(data)
	if o.writeBytes+grown > 0 && o.writeBytes+grown > o.limit() {
		return -ENOSPC
	}
	o.writes[off] = append([]byte(nil), data...)
	o.writeBytes += grown
	o.dirty = true
	return 0
}

// limit is set from the Mount's options at open time; stored on the handle so
// openFile does not need a back-pointer.
func (o *openFile) limit() int { return o.writeLimit }

func (o *openFile) truncateTo(size int64) int {
	if size < 0 {
		return -EINVAL
	}
	o.size = size
	// Drop everything past the new end, then let content() rebuild the rest.
	for off, data := range o.writes {
		if off >= size {
			delete(o.writes, off)
			continue
		}
		if off+int64(len(data)) > size {
			o.writes[off] = data[:size-off]
		}
	}
	o.writeBytes = 0
	for _, d := range o.writes {
		o.writeBytes += len(d)
	}
	o.dirty = true
	return 0
}

// content materialises the current bytes of the handle: the write runs applied
// in order over a zero-filled buffer of the handle's size. Sparse regions are
// zeros, which is what a filesystem guarantees for a hole.
func (o *openFile) content() []byte {
	out := make([]byte, o.size)
	offs := make([]int64, 0, len(o.writes))
	for off := range o.writes {
		offs = append(offs, off)
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	for _, off := range offs {
		data := o.writes[off]
		if off >= int64(len(out)) {
			continue
		}
		copy(out[off:], data)
	}
	return out
}
```

**说明 `o.writeLimit`**：在 `newHandle` 里给它赋值 —— `m.files[fh].writeLimit = m.opts.WriteLimit`。上面把字段放在了 `openFile` 上而不是加一个回指针。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mount/ -run TestHandle -v`
Expected: PASS

> **注意**：`newTestMount`（Task 2）里要初始化 `cache` —— `&Mount{caller: f, opts: DefaultOptions(), cache: newChunkCache(...)}`，否则 `closeHandle` 会 nil panic。请一并修正。

- [ ] **Step 5: Commit**

```bash
git add internal/mount/handle.go internal/mount/handle_test.go
git commit -m "feat(mount): open handle table with accumulated writes

Writes accumulate per handle because the wire has no partial write — a Put
carries a whole file — so a handle is dirty from its first write until release,
fsync, or any operation that moves the path underneath it.

The write limit refuses with ENOSPC instead of truncating: silently dropping
the tail of what someone typed is exactly 'saving quietly loses data'."
```

---

## Task 6: `Getattr` / `Readdir` / `Statfs`

**Files:**
- Create: `internal/mount/ops.go`
- Test: `internal/mount/ops_test.go`

**Interfaces:**
- Consumes: Tasks 2–5
- Produces:
  - `func (m *Mount) Getattr(path string, st *Stat, fh uint64) int`
  - `func (m *Mount) Readdir(path string, fill func(name string, st *Stat, off int64) bool, off int64, fh uint64) int`
  - `func (m *Mount) Statfs(path string, st *Statfs) int`
  - `type Stat struct { Mode uint32; Size int64; Mtime int64; Nlink uint32 }` —— **操作核自己的类型**，不是 cgofuse 的（核不导入 cgofuse）
  - `type Statfs struct { Bsize, Blocks, Bfree, Bavail, Files, Ffree, Namemax uint64 }`
  - `func typeBits(kind node.Kind) uint32` —— `KindDir` → `1<<31`，`KindFile` → `1<<15`，与 cgofuse 的 `S_IFDIR`/`S_IFREG` 一致

> **为什么核有自己的 Stat**：`cgofuse` 的 `Stat_t` 有 15 个字段且平台相关。核只需要 4 个，宿主适配器负责填满那个大结构。这样核的测试不依赖 cgofuse。

- [ ] **Step 1: Write the failing test**

`internal/mount/ops_test.go`：

```go
package mount

import (
	"testing"

	"peerfold/internal/node"
)

func TestGetattrRoot(t *testing.T) {
	m := newTestMount(t)
	var st Stat
	if code := m.Getattr("/", &st, 0); code != 0 {
		t.Fatalf("Getattr(/) = %d", code)
	}
	if st.Mode&(1<<31) == 0 {
		t.Errorf("root Mode = %o, want the directory bit", st.Mode)
	}
	if st.Nlink == 0 {
		t.Error("Nlink must not be 0: some tools use it to detect a broken fs")
	}
}

func TestGetattrFile(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 42)
	var st Stat
	if code := m.Getattr("/a.txt", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if st.Mode&(1<<15) == 0 {
		t.Errorf("Mode = %o, want the regular-file bit", st.Mode)
	}
	if st.Size != 42 {
		t.Errorf("Size = %d, want 42", st.Size)
	}
}

func TestGetattrSymlinkIsElooped(t *testing.T) {
	m := newTestMount(t)
	m.fake.addSymlink("/link")
	var st Stat
	if code := m.Getattr("/link", &st, 0); code != -ELOOP {
		t.Errorf("Getattr(symlink) = %d, want -ELOOP (%d)", code, -ELOOP)
	}
}

func TestGetattrMissing(t *testing.T) {
	m := newTestMount(t)
	var st Stat
	if code := m.Getattr("/nope", &st, 0); code != -ENOENT {
		t.Errorf("Getattr(missing) = %d, want -ENOENT", code)
	}
}

func TestStatfsReportsSomethingUsable(t *testing.T) {
	m := newTestMount(t)
	var st Statfs
	if code := m.Statfs("/", &st); code != 0 {
		t.Fatalf("Statfs = %d", code)
	}
	if st.Bsize == 0 {
		t.Error("Bsize must not be 0: a zero block size makes some tools divide by zero")
	}
	if st.Namemax == 0 {
		t.Error("Namemax must not be 0")
	}
}

func TestReaddirListsChildrenPlusDots(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	m.fake.addFile("/d/a.txt", 1)
	m.fake.addFile("/d/b.txt", 2)

	var names []string
	code := m.Readdir("/d", func(name string, st *Stat, off int64) bool {
		names = append(names, name)
		return true
	}, 0, 0)
	if code != 0 {
		t.Fatalf("Readdir = %d", code)
	}
	want := map[string]bool{"./": true, "../": true, "a.txt": true, "b.txt": true}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected entry %q", n)
		}
		delete(want, n)
	}
	for n := range want {
		t.Errorf("missing entry %q (got %v)", n, names)
	}
}

// A symlink in a listing must be visible but flagged, never passed off as a
// regular file with a plausible size.
func TestReaddirMarksSymlink(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	m.fake.addSymlink("/d/link")

	var got *Stat
	m.Readdir("/d", func(name string, st *Stat, off int64) bool {
		if name == "link" {
			got = st
		}
		return true
	}, 0, 0)
	if got == nil {
		t.Fatal("link not listed")
	}
	if got.Mode&(1<<27) == 0 {
		t.Errorf("link Mode = %o, want the symlink bit", got.Mode)
	}
}

func TestReaddirOnFileIsNotDir(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	if code := m.Readdir("/a.txt", func(string, *Stat, int64) bool { return true }, 0, 0); code != -ENOTDIR {
		t.Errorf("Readdir(file) = %d, want -ENOTDIR", code)
	}
}

func TestReaddirRespectsFillReturningFalse(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		m.fake.addFile("/d/"+n, 1)
	}
	count := 0
	m.Readdir("/d", func(string, *Stat, int64) bool {
		count++
		return count < 3 // stop early, as the kernel does when the buffer fills
	}, 0, 0)
	if count > 3 {
		t.Errorf("fill was called %d times after it returned false", count)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestGetattr|TestReaddir|TestStatfs' -v`
Expected: FAIL —— `undefined: Stat`、`undefined: (m *Mount).Getattr`

- [ ] **Step 3: Write `internal/mount/ops.go`**

```go
package mount

import (
	"sort"

	"peerfold/internal/node"
)

// The type bits, matching cgofuse's S_IFDIR/S_IFREG/S_IFLNK. The core declares
// them itself because it does not import cgofuse.
const (
	modeDir     = 1 << 31
	modeRegular = 1 << 15
	modeSymlink = 1 << 27
)

// Stat is the four fields the core actually needs. The host adapter copies
// them into whatever the platform's stat structure looks like.
type Stat struct {
	Mode  uint32
	Size  int64
	Mtime int64
	Nlink uint32
}

// Statfs is synthetic: there is no single true capacity for a federation of
// other machines' disks, so the numbers only have to be self-consistent and
// non-zero (a zero block size makes some tools divide by zero).
type Statfs struct {
	Bsize   uint64
	Blocks  uint64
	Bfree   uint64
	Bavail  uint64
	Files   uint64
	Ffree   uint64
	Namemax uint64
}

func typeBits(kind node.Kind) uint32 {
	switch kind {
	case node.KindDir:
		return modeDir
	case node.KindSymlink:
		return modeSymlink
	default:
		return modeRegular
	}
}

// statFor turns an index entry into a Stat.
func statFor(n node.Node) Stat {
	mode := uint32(n.Mode) & 0o7777
	mode |= typeBits(n.Kind)
	// The index records the full FileMode in Mode now (Task 1), so the symlink
	// bit comes through on its own; make sure the type bits are not doubled.
	mode &^= modeDir | modeRegular | modeSymlink
	mode |= typeBits(n.Kind)
	nlink := uint32(1)
	if n.Kind == node.KindDir {
		nlink = 2
	}
	return Stat{Mode: mode, Size: n.Size, Mtime: n.Mtime, Nlink: nlink}
}

func (m *Mount) Getattr(path string, st *Stat, fh uint64) int {
	n, code := m.lookup(path)
	if code != 0 {
		return code
	}
	// A dirty handle reports the size it is about to have, not the size the
	// index last heard about, or a program that just wrote past the old end
	// would see its own write disappear on the next stat.
	if h, ok := m.handle(fh); ok && h.dirty {
		n.Size = h.size
	}
	*st = statFor(n)
	return 0
}

func (m *Mount) Readdir(path string, fill func(name string, st *Stat, off int64) bool, off int64, fh uint64) int {
	n, code := m.lookup(path)
	if code != 0 {
		return code
	}
	if n.Kind != node.KindDir {
		return -ENOTDIR
	}
	children, err := m.caller.List(path)
	if err != nil {
		return errnoFor(err)
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })

	// The dots are synthesised: the caller's List returns real entries only.
	if !fill("./", nil, 0) {
		return 0
	}
	if !fill("../", nil, 0) {
		return 0
	}
	for _, c := range children {
		st := statFor(c)
		if c.Kind == node.KindSymlink {
			// Visible, but flagged. The mount layer refuses to open it, and a
			// caller that stats it gets ELOOP rather than a plausible size.
			st.Mode |= modeSymlink
		}
		if !fill(baseName(c.Path), &st, 0) {
			return 0
		}
	}
	return 0
}

func (m *Mount) Statfs(path string, st *Statfs) int {
	if _, code := m.lookup(path); code != 0 {
		return code
	}
	// Synthetic and deliberately optimistic: reporting a full disk when the
	// federation has room would break writes for no reason. Reporting
	// plentiful space is the safe direction for a read-only-until-you-write
	// view of other machines' disks.
	const block = 4096
	*st = Statfs{
		Bsize:   block,
		Blocks:  1 << 30 / block, // 1 GiB of claimed capacity
		Bfree:   1 << 29 / block,
		Bavail:  1 << 29 / block,
		Files:   1 << 20,
		Ffree:   1 << 19,
		Namemax: 255,
	}
	return 0
}

// baseName is the last element of a slash path.
func baseName(p string) string {
	if i := lastSlash(p); i >= 0 {
		return p[i+1:]
	}
	return p
}

func lastSlash(p string) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mount/ -run 'TestGetattr|TestReaddir|TestStatfs' -v`
Expected: PASS

- [ ] **Step 5: Run the whole mount package**

Run: `go test ./internal/mount/ -v 2>&1 | tail -20`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/mount/ops.go internal/mount/ops_test.go
git commit -m "feat(mount): Getattr, Readdir, Statfs

The core has its own four-field Stat rather than using cgofuse's fifteen-field
platform-dependent one, so the operation core stays free of the FUSE library
and its tests run anywhere.

A dirty handle reports the size it is about to have, not the index's last known
size: otherwise a program that wrote past the old end sees its own write
vanish on the next stat."
```

---

## Task 7: `Open` / `Opendir` / `Read` —— 分块读与 `n = -1`

**Files:**
- Modify: `internal/mount/ops.go`
- Test: `internal/mount/read_test.go`

**Interfaces:**
- Consumes: Tasks 4–6
- Produces:
  - `func (m *Mount) Open(path string, mode int) (uint64, int)`
  - `func (m *Mount) Opendir(path string) (uint64, int)`
  - `func (m *Mount) Read(path string, buf []byte, off int64, fh uint64) int` —— 返回**读到的字节数**（这是 cgofuse 的语义，探针里我搞错的就是这个）
  - `const OReadOnly = 0`, `OReadWrite = 2`（cgofuse 的 flag 值，核自己声明）

- [ ] **Step 1: Write the failing test**

`internal/mount/read_test.go`：

```go
package mount

import (
	"strings"
	"testing"
)

func TestOpenMissingIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if _, code := m.Open("/nope", OReadOnly); code != -ENOENT {
		t.Errorf("Open(missing) = %d, want -ENOENT", code)
	}
}

func TestOpenSymlinkIsElooped(t *testing.T) {
	m := newTestMount(t)
	m.fake.addSymlink("/link")
	if _, code := m.Open("/link", OReadOnly); code != -ELOOP {
		t.Errorf("Open(symlink) = %d, want -ELOOP", code)
	}
}

func TestOpenDirectoryAsFileIsEisdir(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	// Opening a directory for writing is the case editors hit when they try to
	// save over a directory.
	if _, code := m.Open("/d", OReadWrite); code != -EISDIR {
		t.Errorf("Open(dir, write) = %d, want -EISDIR", code)
	}
	// Reading a directory as a file is EISDIR too; Opendir is the other path.
	if _, code := m.Open("/d", OReadOnly); code != -EISDIR {
		t.Errorf("Open(dir, read) = %d, want -EISDIR", code)
	}
}

func TestReadWholeSmallFile(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, code := m.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	buf := make([]byte, 64)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 5 {
		t.Fatalf("Read = %d, want 5", n)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("content = %q", buf[:n])
	}
}

// The kernel reads at a 4 KiB offset into a small file and expects 0, not an
// error. This is the read-ahead that every real filesystem loop does.
func TestReadPastEndIsZero(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadOnly)
	buf := make([]byte, 16)
	if n := m.Read("/a.txt", buf, 4096, fh); n != 0 {
		t.Errorf("Read past EOF = %d, want 0", n)
	}
}

// A read that spans more than one chunk must stitch them together, because the
// caller asked for one buffer and the wire answers in chunkSize pieces.
func TestReadAcrossChunks(t *testing.T) {
	m := newTestMount(t)
	m.opts.BufferLimit = chunkSize * 4
	m.opts.BufferTotal = chunkSize * 8
	m.cache = newChunkCache(m.opts.BufferLimit, m.opts.BufferTotal)

	body := strings.Repeat("A", chunkSize) + strings.Repeat("B", chunkSize)
	m.fake.addFile("/big", len(body))
	m.fake.data["/big"] = []byte(body)

	fh, _ := m.Open("/big", OReadOnly)
	buf := make([]byte, chunkSize+10)
	// Start ten bytes before the chunk boundary so the request spans two.
	n := m.Read("/big", buf, chunkSize-10, fh)
	if n != len(buf) {
		t.Fatalf("Read = %d, want %d", n, len(buf))
	}
	want := strings.Repeat("A", 10) + strings.Repeat("B", chunkSize)
	if string(buf) != want {
		t.Errorf("stitched content is wrong at the seam")
	}
}

// The second read of the same range must be served from the cache, or every
// sequential scan re-fetches from the owner.
func TestReadIsCached(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadOnly)
	buf := make([]byte, 5)
	m.Read("/a.txt", buf, 0, fh)
	before := len(m.fake.callLog())
	m.Read("/a.txt", buf, 0, fh)
	after := m.fake.callLog()
	for _, c := range after[before:] {
		if strings.HasPrefix(c, "Read ") {
			t.Errorf("cached read still hit the wire: %v", after[before:])
		}
	}
}

// A write through the handle must be visible to a subsequent read of the same
// handle without any flush: programs write and then read back.
func TestReadSeesOwnWrites(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)
	h.write(0, []byte("HELLO"))
	buf := make([]byte, 5)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 5 || string(buf[:n]) != "HELLO" {
		t.Errorf("read after write = %q (%d bytes), want HELLO", buf[:n], n)
	}
}

func TestReadUnknownHandleFallsBackToPath(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	// fh 0 is what cgofuse passes when the kernel read without an open.
	buf := make([]byte, 5)
	n := m.Read("/a.txt", buf, 0, 0)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Errorf("read with fh 0 = %q (%d)", buf[:n], n)
	}
}

func TestOpendirThenReaddir(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	m.fake.addFile("/d/a.txt", 1)
	fh, code := m.Opendir("/d")
	if code != 0 {
		t.Fatalf("Opendir = %d", code)
	}
	var seen int
	if code := m.Readdir("/d", func(string, *Stat, int64) bool { seen++; return true }, 0, fh); code != 0 {
		t.Errorf("Readdir = %d", code)
	}
	if seen == 0 {
		t.Error("Readdir saw nothing after Opendir")
	}
}

func TestOpendirOnFile(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	if _, code := m.Opendir("/a.txt"); code != -ENOTDIR {
		t.Errorf("Opendir(file) = %d, want -ENOTDIR", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestOpen|TestRead|TestOpendir' -v`
Expected: FAIL —— `undefined: (m *Mount).Open`

- [ ] **Step 3: Implement in `internal/mount/ops.go`**

```go
// cgofuse's open flags. The core declares them because it does not import
// cgofuse; host.go passes through whatever the platform gave it.
const (
	OReadOnly  = 0
	OReadWrite = 2
)

func (m *Mount) Open(path string, mode int) (uint64, int) {
	cleaned, err := Clean(path)
	if err != nil {
		return 0, -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return 0, code
	}
	if n.Kind == node.KindDir {
		return 0, -EISDIR
	}
	fh := m.newHandle(cleaned, n.Kind)
	h, _ := m.handle(fh)
	h.size = n.Size
	h.writeLimit = m.opts.WriteLimit
	return fh, 0
}

func (m *Mount) Opendir(path string) (uint64, int) {
	cleaned, err := Clean(path)
	if err != nil {
		return 0, -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return 0, code
	}
	if n.Kind != node.KindDir {
		return 0, -ENOTDIR
	}
	return m.newHandle(cleaned, node.KindDir), 0
}

// Read returns the number of bytes placed in buf, following cgofuse's
// convention: 0 means end of file, a negative value is an errno.
//
// The wire's own Read takes n where a negative n means "to the end" and n == 0
// returns an empty slice rather than an error, so this breaks the request into
// chunkSize blocks and always asks for a positive count. Passing the caller's
// remaining length straight through would fail on the very common request for
// a large range.
func (m *Mount) Read(path string, buf []byte, off int64, fh uint64) int {
	if off < 0 {
		return -EINVAL
	}
	if len(buf) == 0 {
		return 0
	}
	h, haveHandle := m.handle(fh)
	path, code := m.readPath(path, fh)
	if code != 0 {
		return code
	}

	// A dirty handle answers from its own buffer: a program that writes and
	// reads back must not see the owner's stale copy.
	if haveHandle && h.dirty {
		content := h.content()
		if off >= int64(len(content)) {
			return 0
		}
		return copy(buf, content[off:])
	}

	total := 0
	for total < len(buf) {
		chunkIdx := (off + int64(total)) / chunkSize
		chunkOff := chunkIdx * chunkSize
		chunk, ok := m.cache.get(fh, chunkIdx)
		if !ok {
			data, err := m.caller.Read(path, chunkOff, chunkSize)
			if err != nil {
				if total > 0 {
					return total // partial success beats losing what we have
				}
				return errnoFor(err)
			}
			chunk = data
			m.cache.put(fh, chunkIdx, chunk)
		}
		if len(chunk) == 0 {
			break // end of file
		}
		within := (off + int64(total)) - chunkOff
		if within >= int64(len(chunk)) {
			break
		}
		n := copy(buf[total:], chunk[within:])
		total += n
		if chunkOff+int64(len(chunk)) > off+int64(total) {
			break // the chunk ran out before the buffer did
		}
		if len(chunk) < chunkSize {
			break // short chunk means the file ended
		}
	}
	return total
}

// readPath resolves the path to use for a read. An unknown handle (cgofuse
// passes 0 when the kernel read without an open) falls back to the path.
func (m *Mount) readPath(path string, fh uint64) (string, int) {
	if h, ok := m.handle(fh); ok {
		return h.path, 0
	}
	cleaned, err := Clean(path)
	if err != nil {
		return "", -EINVAL
	}
	if _, code := m.lookup(cleaned); code != 0 {
		return "", code
	}
	return cleaned, 0
}
```

- [ ] **Step 4: Add the `writeLimit` field to `openFile`**

在 `handle.go` 的 `openFile` 里加：

```go
	writeLimit int
```

并删掉 Task 5 里那个"给 `o.writeLimit` 赋值"的旁注 —— 现在它在 `Open` 与 `newHandle` 的调用点上赋值。**在 `newHandle` 内部一并赋值**更安全：

```go
func (m *Mount) newHandle(path string, kind node.Kind) uint64 {
	m.nextFh++
	fh := m.nextFh
	m.files[fh] = &openFile{
		path:       path,
		fh:         fh,
		kind:       kind,
		writes:     map[int64][]byte{},
		writeLimit: m.opts.WriteLimit,
	}
	return fh
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/mount/ -run 'TestOpen|TestRead|TestOpendir' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/mount/ops.go internal/mount/handle.go internal/mount/read_test.go
git commit -m "feat(mount): Open, Opendir, and chunked Read

Read returns a byte count the way cgofuse wants, and always asks the wire for a
positive count: the wire's own Read treats n < 0 as 'to the end' and n == 0 as
an empty slice rather than an error, so forwarding the caller's remaining
length straight through would break the common request for a large range.

A dirty handle answers from its own buffer, so a program that writes and reads
back does not see the owner's stale copy. A read that spans chunks stitches
them at the seam, which is tested at the boundary rather than in the middle."
```

---

## Task 8: 写路径 —— `Create` / `Write` / `Truncate` / `Fsync` / `Flush` / `Release`（D10 的强制回写表）

**这是整个计划风险最高的任务。** 强制回写表里的每一条漏掉都会变成"保存悄悄丢数据"，而且**漏掉不会让任何测试失败** —— 除非专门写测试去逼它。

**Files:**
- Modify: `internal/mount/ops.go`
- Modify: `internal/mount/handle.go`（加 `flush`）
- Test: `internal/mount/write_test.go`

**Interfaces:**
- Consumes: Tasks 5–7
- Produces:
  - `func (m *Mount) flush(fh uint64) int` —— 把脏 handle 整文件 `Put` 出去；幂等
  - `func (m *Mount) flushPath(path string) int` —— 回写**所有**路径匹配的脏 handle（`rename`/`unlink`/`truncate` 前用）
  - `func (m *Mount) Create(path string, mode uint32) (uint64, int)`
  - `func (m *Mount) Write(path string, buf []byte, off int64, fh uint64) int`
  - `func (m *Mount) Truncate(path string, size int64, fh uint64) int`
  - `func (m *Mount) Fsync(path string, datasync bool, fh uint64) int`
  - `func (m *Mount) Flush(path string, fh uint64) int`
  - `func (m *Mount) Release(path string, fh uint64) int`

- [ ] **Step 1: Write the failing test**

`internal/mount/write_test.go`：

```go
package mount

import (
	"strings"
	"testing"
)

func TestCreateMakesAWritableHandle(t *testing.T) {
	m := newTestMount(t)
	fh, code := m.Create("/new.txt", 0o644)
	if code != 0 {
		t.Fatalf("Create = %d", code)
	}
	h, ok := m.handle(fh)
	if !ok {
		t.Fatal("Create returned a handle that does not exist")
	}
	if h.size != 0 {
		t.Errorf("new file size = %d, want 0", h.size)
	}
}

func TestCreateOnSymlinkIsElooped(t *testing.T) {
	m := newTestMount(t)
	m.fake.addSymlink("/link")
	if _, code := m.Create("/link", 0o644); code != -ELOOP {
		t.Errorf("Create(symlink) = %d, want -ELOOP", code)
	}
}

func TestCreateInMissingParent(t *testing.T) {
	m := newTestMount(t)
	if _, code := m.Create("/nope/x.txt", 0o644); code != -ENOENT {
		t.Errorf("Create(missing parent) = %d, want -ENOENT", code)
	}
}

func TestWriteThenFlushPutsWholeFile(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadWrite)
	if n := m.Write("/a.txt", []byte("HELLO"), 0, fh); n != 5 {
		t.Fatalf("Write = %d, want 5", n)
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after flush = %q, want HELLO", got)
	}
}

// The single most important behaviour in this task. An editor that writes and
// calls fsync, then loses power, must not lose the save. A whole-file
// write-back that only happens at release makes fsync return success having
// written nothing.
func TestFsyncFlushesBeforeReturning(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)

	if code := m.Fsync("/a.txt", false, fh); code != 0 {
		t.Fatalf("Fsync = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after fsync = %q, want HELLO — fsync returned success without writing", got)
	}
}

func TestFsyncOnCleanHandleIsCheap(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadOnly)
	if code := m.Fsync("/a.txt", false, fh); code != 0 {
		t.Fatalf("Fsync(clean) = %d", code)
	}
	// No Put should have happened.
	for _, c := range m.fake.callLog() {
		if strings.HasPrefix(c, "Put ") {
			t.Errorf("a clean fsync wrote the file: %v", c)
		}
	}
}

func TestReleaseFlushesAndFreesTheHandle(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after release = %q, want HELLO", got)
	}
	if _, ok := m.handle(fh); ok {
		t.Error("release did not free the handle")
	}
}

// Release runs when the last reference goes away, and losing the connection is
// not an error the caller can act on. It must still return success for a clean
// handle, and must not lose the data for a dirty one.
func TestReleaseIsIdempotent(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadWrite)
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("first release = %d", code)
	}
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Errorf("second release = %d, want 0", code)
	}
}

func TestTruncateFlushesInPlace(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 11)
	m.fake.data["/a.txt"] = []byte("hello world")
	fh, _ := m.Open("/a.txt", OReadWrite)
	if code := m.Truncate("/a.txt", 5, fh); code != 0 {
		t.Fatalf("Truncate = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "hello" {
		t.Errorf("after truncate = %q, want hello", got)
	}
}

// Truncate arrives by path with fh 0 (the kernel uses this form for O_TRUNC
// and for `> file`), which means there is no handle to consult. It must still
// work.
func TestTruncateByPathWithoutHandle(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 11)
	m.fake.data["/a.txt"] = []byte("hello world")
	if code := m.Truncate("/a.txt", 0, 0); code != 0 {
		t.Fatalf("Truncate(path) = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "" {
		t.Errorf("after truncate to 0 = %q, want empty", got)
	}
}

// A dirty open followed by a write must not multiply the Put: the handle's
// buffer is the truth, and flushing twice would be two whole-file uploads.
func TestFlushTwiceOnlyPutsOnce(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)
	m.Flush("/a.txt", fh)
	puts := 0
	for _, c := range m.fake.callLog() {
		if strings.HasPrefix(c, "Put ") {
			puts++
		}
	}
	m.Flush("/a.txt", fh)
	for _, c := range m.fake.callLog() {
		if strings.HasPrefix(c, "Put ") {
			puts++
		}
	}
	if puts != 1 {
		t.Errorf("Put called %d times, want 1: a clean flush must not re-upload", puts)
	}
}

// Write over the handle's limit must be refused, and the refusal must not have
// damaged what was already written.
func TestWriteOverLimitLeavesEarlierWritesIntact(t *testing.T) {
	m := newTestMount(t)
	m.opts.WriteLimit = 8
	m.fake.addFile("/a.txt", 4)
	m.fake.data["/a.txt"] = []byte("abcd")
	fh, _ := m.Open("/a.txt", OReadWrite)
	if n := m.Write("/a.txt", []byte("abcd"), 0, fh); n != 4 {
		t.Fatalf("first write = %d", n)
	}
	if n := m.Write("/a.txt", []byte("efghijkl"), 4, fh); n != -ENOSPC {
		t.Errorf("write past limit = %d, want -ENOSPC (%d)", n, -ENOSPC)
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "abcdabcd" {
		t.Errorf("flushed %q, want the refused write to have changed nothing", got)
	}
}

func TestWriteToMissingPathIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if n := m.Write("/nope", []byte("x"), 0, 0); n != -ENOENT {
		t.Errorf("Write(missing) = %d, want -ENOENT", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestCreate|TestWrite|TestFsync|TestFlush|TestRelease|TestTruncate' -v`
Expected: FAIL —— `undefined: (m *Mount).flush`

- [ ] **Step 3: Implement the flush core in `internal/mount/ops.go`**

```go
// flush writes one handle's accumulated content as a whole file. It is the
// single place a mutation reaches the owner, which is why every entry in the
// forced-flush table calls it.
//
// It is idempotent: after a successful Put the handle is clean, so a second
// flush is a no-op. That matters because Fsync, Flush and Release can all
// arrive for the same handle in any order, and three whole-file uploads of a
// 40 MB file is a real cost.
func (m *Mount) flush(fh uint64) int {
	h, ok := m.handle(fh)
	if !ok || !h.dirty {
		return 0
	}
	body := h.content()
	if err := m.caller.Put(h.path, bytes.NewReader(body)); err != nil {
		// Keep the handle dirty: the write did not land, and clearing the flag
		// would discard the only copy of the user's content.
		return errnoFor(err)
	}
	h.dirty = false
	h.writes = map[int64][]byte{}
	h.writeBytes = 0
	return 0
}

// flushPath flushes every dirty handle for a path. The kernel does not always
// hand a handle to operations that move a file: rename, unlink and a
// path-based truncate all arrive without one, and a dirty open that is renamed
// must have its content follow the name.
func (m *Mount) flushPath(path string) int {
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	for fh, h := range m.files {
		if h.path == cleaned && h.dirty {
			if code := m.flush(fh); code != 0 {
				return code
			}
		}
	}
	return 0
}

func (m *Mount) Create(path string, mode uint32) (uint64, int) {
	cleaned, err := Clean(path)
	if err != nil {
		return 0, -EINVAL
	}
	if _, _, code := m.lookupParent(cleaned); code != 0 {
		return 0, code
	}
	// An existing symlink is refused rather than replaced; writing through the
	// name would land on the target.
	if existing, code := m.lookup(cleaned); code == 0 {
		if existing.Kind == node.KindDir {
			return 0, -EISDIR
		}
	} else if code != -ENOENT {
		return 0, code
	}
	fh := m.newHandle(cleaned, node.KindFile)
	h, _ := m.handle(fh)
	h.size = 0
	// A newly created file is dirty from the start: O_CREAT with nothing
	// written still means "this file exists now", and it does not exist at the
	// owner until something is Put.
	h.dirty = true
	return fh, 0
}

// Write returns the number of bytes accepted, following cgofuse's convention.
func (m *Mount) Write(path string, buf []byte, off int64, fh uint64) int {
	h, ok := m.handle(fh)
	if !ok {
		// The kernel can write by path. Open an implicit handle so the write
		// is not lost.
		cleaned, err := Clean(path)
		if err != nil {
			return -EINVAL
		}
		if _, code := m.lookup(cleaned); code != 0 {
			return code
		}
		newFh := m.newHandle(cleaned, node.KindFile)
		h, _ = m.handle(newFh)
		fh = newFh
	}
	if h.kind == node.KindDir {
		return -EISDIR
	}
	if code := h.write(off, buf); code != 0 {
		return code
	}
	return len(buf)
}

func (m *Mount) Truncate(path string, size int64, fh uint64) int {
	if h, ok := m.handle(fh); ok && h.dirty {
		if code := h.truncateTo(size); code != 0 {
			return code
		}
		return m.flush(fh)
	}
	// Path form: read the whole file, cut it, write it back.
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return code
	}
	if n.Kind == node.KindDir {
		return -EISDIR
	}
	body, err := m.caller.Read(cleaned, 0, -1)
	if err != nil {
		return errnoFor(err)
	}
	if size < int64(len(body)) {
		body = body[:size]
	} else if size > int64(len(body)) {
		grown := make([]byte, size)
		copy(grown, body)
		body = grown
	}
	if err := m.caller.Put(cleaned, bytes.NewReader(body)); err != nil {
		return errnoFor(err)
	}
	return 0
}

// Fsync is the first entry in the forced-flush table, and the one whose
// omission is hardest to notice: it returns success either way.
func (m *Mount) Fsync(path string, datasync bool, fh uint64) int {
	return m.flush(fh)
}

// Flush is called on every close() of a file descriptor, which can be many
// times per open, so it must be cheap when there is nothing to do.
func (m *Mount) Flush(path string, fh uint64) int {
	return m.flush(fh)
}

func (m *Mount) Release(path string, fh uint64) int {
	if _, ok := m.handle(fh); !ok {
		return 0 // already released; must not be an error
	}
	code := m.flush(fh)
	// Free the handle even when the flush failed: the kernel is done with it,
	// and keeping it would leak. The error is returned so the caller sees it.
	m.closeHandle(fh)
	return code
}
```

在 `ops.go` 的 import 里加 `"bytes"`。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mount/ -run 'TestCreate|TestWrite|TestFsync|TestFlush|TestRelease|TestTruncate' -v`
Expected: PASS

- [ ] **Step 5: Add the "which entry points flush" test**

这个测试**枚举强制回写表的每一条**，这样将来有人加一条改变路径的操作却忘了冲刷时，会因为清单不完整而暴露。

```go
// Every entry in the forced-flush table, enumerated. A new operation that
// changes a path without flushing shows up here as a missing case.
func TestForcedFlushTable(t *testing.T) {
	cases := []struct {
		name string
		run  func(m *Mount, fh uint64) int
	}{
		{"Fsync", func(m *Mount, fh uint64) int { return m.Fsync("/a.txt", false, fh) }},
		{"Flush", func(m *Mount, fh uint64) int { return m.Flush("/a.txt", fh) }},
		{"Release", func(m *Mount, fh uint64) int { return m.Release("/a.txt", fh) }},
		{"Truncate", func(m *Mount, fh uint64) int { return m.Truncate("/a.txt", 3, fh) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestMount(t)
			m.fake.addFile("/a.txt", 5)
			m.fake.data["/a.txt"] = []byte("hello")
			fh, _ := m.Open("/a.txt", OReadWrite)
			m.Write("/a.txt", []byte("HELLO"), 0, fh)

			c.run(m, fh)

			body := m.fake.data["/a.txt"]
			if string(body) == "hello" {
				t.Errorf("%s returned without writing: the save is still only in memory", c.name)
			}
		})
	}
}
```

- [ ] **Step 6: Run the whole mount package**

Run: `go test ./internal/mount/ -v 2>&1 | tail -20`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/mount/ops.go internal/mount/write_test.go
git commit -m "feat(mount): write path with a forced-flush table

The wire has no partial write — a Put carries a whole file — so a handle
accumulates writes and flushes as one whole-file upload. Every entry in the
forced-flush table must actually flush before returning success: Fsync, Flush,
Release, and a dirty Truncate. TestFsyncFlushesBeforeReturning and
TestForcedFlushTable exist because the failure they catch is silent.

flush() is idempotent, because Fsync, Flush and Release can all arrive for one
handle and three whole-file uploads of a 40 MB file is a real cost.

A failed Put leaves the handle dirty rather than clearing the flag: the write
did not land, and clearing it would discard the only copy of the content."
```

---

## Task 9: `Mkdir` / `Unlink` / `Rmdir` / `Rename` —— 改名前先冲刷（D10 的第二半）

**Files:**
- Modify: `internal/mount/ops.go`
- Test: `internal/mount/dir_test.go`

**Interfaces:**
- Consumes: Task 8（`flush`, `flushPath`）
- Produces:
  - `func (m *Mount) Mkdir(path string, mode uint32) int`
  - `func (m *Mount) Unlink(path string) int`
  - `func (m *Mount) Rmdir(path string) int`
  - `func (m *Mount) Rename(oldPath, newPath string) int`

- [ ] **Step 1: Write the failing test**

`internal/mount/dir_test.go`：

```go
package mount

import "testing"

func TestMkdirCreatesAndRefusesExisting(t *testing.T) {
	m := newTestMount(t)
	if code := m.Mkdir("/new", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d", code)
	}
	if code := m.Mkdir("/new", 0o755); code != -EEXIST {
		t.Errorf("Mkdir over an existing name = %d, want -EEXIST (%d)", code, -EEXIST)
	}
}

func TestMkdirMissingParentIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if code := m.Mkdir("/nope/new", 0o755); code != -ENOENT {
		t.Errorf("Mkdir with missing parent = %d, want -ENOENT", code)
	}
}

// A file and a directory are different objects; creating one where the other
// exists must say which, because callers branch on it.
func TestMkdirOverFileIsEexist(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	if code := m.Mkdir("/a.txt", 0o755); code != -EEXIST {
		t.Errorf("Mkdir over a file = %d, want -EEXIST", code)
	}
}

func TestUnlinkFile(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	if code := m.Unlink("/a.txt"); code != 0 {
		t.Fatalf("Unlink = %d", code)
	}
	if _, code := m.lookup("/a.txt"); code != -ENOENT {
		t.Error("file still present after unlink")
	}
}

// Some programs call unlink on a directory. That is EISDIR or EPERM, never
// success, or they would believe the directory is gone.
func TestUnlinkDirectoryIsRefused(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	if code := m.Unlink("/d"); code != -EISDIR && code != -EPERM {
		t.Errorf("Unlink(dir) = %d, want -EISDIR or -EPERM", code)
	}
	if _, code := m.lookup("/d"); code != 0 {
		t.Error("Unlink removed a directory")
	}
}

func TestUnlinkSymlinkRemovesTheLink(t *testing.T) {
	m := newTestMount(t)
	m.fake.addSymlink("/link")
	// The mount layer refuses to *open* a symlink, but removing it is safe and
	// is the only way to get rid of one through the mount.
	if code := m.Unlink("/link"); code != 0 {
		t.Errorf("Unlink(symlink) = %d, want 0", code)
	}
}

func TestRmdirEmpty(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	if code := m.Rmdir("/d"); code != 0 {
		t.Fatalf("Rmdir = %d", code)
	}
}

func TestRmdirNonEmptyIsRefused(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	m.fake.addFile("/d/a.txt", 1)
	if code := m.Rmdir("/d"); code != -ENOTEMPTY {
		t.Errorf("Rmdir(non-empty) = %d, want -ENOTEMPTY (%d)", code, -ENOTEMPTY)
	}
}

func TestRmdirFileIsNotDir(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	if code := m.Rmdir("/a.txt"); code != -ENOTDIR {
		t.Errorf("Rmdir(file) = %d, want -ENOTDIR", code)
	}
}

func TestRmdirRootIsRefused(t *testing.T) {
	m := newTestMount(t)
	if code := m.Rmdir("/"); code != -EINVAL && code != -EPERM {
		t.Errorf("Rmdir(/) = %d, want a refusal", code)
	}
}

func TestRenameFile(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	m.fake.data["/a.txt"] = []byte("x")
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	if _, code := m.lookup("/b.txt"); code != 0 {
		t.Error("target missing after rename")
	}
	if _, code := m.lookup("/a.txt"); code != -ENOENT {
		t.Error("source still present after rename")
	}
}

// The case the design flagged as the one most likely to be missed: an editor
// writes a temporary file, fsyncs it, renames it over the original, and never
// releases it before the rename. The content must follow the name.
func TestRenameFlushesDirtyOpenFirst(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/tmp.txt", 0)
	m.fake.addFile("/real.txt", 5)
	m.fake.data["/real.txt"] = []byte("old!!")

	fh, _ := m.Open("/tmp.txt", OReadWrite)
	m.Write("/tmp.txt", []byte("new content"), 0, fh)

	if code := m.Rename("/tmp.txt", "/real.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	if got := string(m.fake.data["/real.txt"]); got != "new content" {
		t.Errorf("after rename the content is %q, want 'new content' — the dirty write did not follow the name", got)
	}
}

// And a rename of a path that has a dirty open must not leave the write behind
// on the old name either.
func TestRenameLeavesNothingBehind(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 0)
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("data"), 0, fh)
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	if got, ok := m.fake.data["/a.txt"]; ok {
		t.Errorf("write was left at the old name: %q", got)
	}
}

func TestRenameUpdatesTheHandlePath(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 0)
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Rename("/a.txt", "/b.txt")
	h, ok := m.handle(fh)
	if !ok {
		t.Fatal("handle vanished across a rename")
	}
	if h.path != "/b.txt" {
		t.Errorf("handle path = %q, want /b.txt — a later flush would recreate the old name", h.path)
	}
}

func TestRenameOntoExistingFile(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	m.fake.data["/a.txt"] = []byte("a")
	m.fake.addFile("/b.txt", 1)
	m.fake.data["/b.txt"] = []byte("b")
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename over existing = %d", code)
	}
	if got := string(m.fake.data["/b.txt"]); got != "a" {
		t.Errorf("after rename = %q, want %q", got, "a")
	}
}

func TestRenameMissingSource(t *testing.T) {
	m := newTestMount(t)
	if code := m.Rename("/nope", "/b.txt"); code != -ENOENT {
		t.Errorf("Rename(missing) = %d, want -ENOENT", code)
	}
}

func TestRenameIntoMissingDirectory(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	if code := m.Rename("/a.txt", "/nope/b.txt"); code != -ENOENT {
		t.Errorf("Rename(into missing dir) = %d, want -ENOENT", code)
	}
}

// Renaming the root, or onto the root, has no meaning.
func TestRenameRootIsRefused(t *testing.T) {
	m := newTestMount(t)
	if code := m.Rename("/", "/x"); code == 0 {
		t.Error("Rename(/) succeeded")
	}
	if code := m.Rename("/a", "/" ); code == 0 {
		t.Error("Rename onto / succeeded")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestMkdir|TestUnlink|TestRmdir|TestRename' -v`
Expected: FAIL —— `undefined: (m *Mount).Mkdir`

- [ ] **Step 3: Implement in `internal/mount/ops.go`**

```go
func (m *Mount) Mkdir(path string, mode uint32) int {
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	_, _, code := m.lookupParent(cleaned)
	if code != 0 {
		return code
	}
	if _, code := m.lookup(cleaned); code == 0 {
		return -EEXIST
	} else if code != -ENOENT {
		return code
	}
	if err := m.caller.Mkdir(cleaned); err != nil {
		return errnoFor(err)
	}
	return 0
}

func (m *Mount) Unlink(path string) int {
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		// lookup refuses symlinks with ELOOP, but removing one is safe and is
		// the only way to get rid of it through the mount. Fall back to a raw
		// existence check.
		if code != -ELOOP {
			return code
		}
		if !m.exists(cleaned) {
			return -ENOENT
		}
		n = node.Node{Path: cleaned, Kind: node.KindSymlink}
	}
	if n.Kind == node.KindDir {
		return -EISDIR
	}
	// A dirty open on the path means the content has not reached the owner.
	// Removing the path first would drop the only copy.
	if code := m.flushPath(cleaned); code != 0 {
		return code
	}
	if err := m.caller.Remove(cleaned); err != nil {
		return errnoFor(err)
	}
	m.discardHandles(cleaned)
	return 0
}

func (m *Mount) Rmdir(path string) int {
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	if cleaned == "/" {
		return -EPERM
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return code
	}
	if n.Kind != node.KindDir {
		return -ENOTDIR
	}
	children, err := m.caller.List(cleaned)
	if err != nil {
		return errnoFor(err)
	}
	if len(children) > 0 {
		return -ENOTEMPTY
	}
	if err := m.caller.Remove(cleaned); err != nil {
		return errnoFor(err)
	}
	return 0
}

func (m *Mount) Rename(oldPath, newPath string) int {
	oldClean, err := Clean(oldPath)
	if err != nil {
		return -EINVAL
	}
	newClean, err := Clean(newPath)
	if err != nil {
		return -EINVAL
	}
	if oldClean == "/" || newClean == "/" {
		return -EPERM
	}
	if _, code := m.lookup(oldClean); code != 0 {
		return code
	}
	if _, _, code := m.lookupParent(newClean); code != 0 {
		return code
	}
	// Flush before renaming, because the kernel does not always hand a handle
	// to a rename and the content has nowhere to go afterwards otherwise. This
	// is the editor pattern: write a temporary file, fsync, rename over the
	// original.
	if code := m.flushPath(oldClean); code != 0 {
		return code
	}
	if err := m.caller.Rename(oldClean, newClean); err != nil {
		return errnoFor(err)
	}
	// Any handle still open on the old path must follow the new name, or a
	// later flush would recreate the old name.
	for _, h := range m.files {
		if h.path == oldClean {
			h.path = newClean
		}
	}
	return 0
}

// exists answers whether a name is present at the owner without going through
// lookup, which refuses symlinks.
func (m *Mount) exists(path string) bool {
	parent, base, err := SplitParent(path)
	if err != nil {
		return false
	}
	children, err := m.caller.List(parent)
	if err != nil {
		return false
	}
	for _, c := range children {
		if baseName(c.Path) == base {
			return true
		}
	}
	return false
}

// discardHandles drops handles whose path is gone. Leaving them would let a
// flush recreate a file the user just deleted.
func (m *Mount) discardHandles(path string) {
	for fh, h := range m.files {
		if h.path == path {
			m.closeHandle(fh)
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mount/ -run 'TestMkdir|TestUnlink|TestRmdir|TestRename' -v`
Expected: PASS

- [ ] **Step 5: Run the whole mount package**

Run: `go test ./internal/mount/ -v 2>&1 | tail -20`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/mount/ops.go internal/mount/dir_test.go
git commit -m "feat(mount): Mkdir, Unlink, Rmdir, Rename

Rename flushes a dirty open on the source before calling the owner. This is the
editor pattern — write a temporary file, fsync it, rename it over the original,
often without releasing it first — and skipping the flush leaves the content in
memory while the name moves, so the save appears to succeed and the file is
unchanged.

Handles still open on the old path follow the new name, and handles on a
deleted path are dropped: either way a later flush would otherwise recreate a
name that no longer exists."
```

---

## Task 10: 事件泵、丢更新探测、"交出去的表"（D5 的前半）

**背景**：`viewer.emit` 在通道满时**静默丢弃**事件（无计数、无序号），`server.broadcast` 在推送超时后**永久 `delete` watcher**（连接仍可用于 List/Read）。两条路都会让挂载层永远服务一棵陈旧的树，而且**没有任何信号**。这个任务造出"我可能变聋了"的检测，Task 11 决定怎么办。

**Files:**
- Create: `internal/mount/watch.go`
- Test: `internal/mount/watch_test.go`
- Modify: `internal/proto/proto.go`（加 `EventBody.Seq`）
- Modify: `internal/viewer/viewer.go`（暴露序号与丢包计数）
- Modify: `internal/server/server.go`（`broadcast` 不再永久删 watcher）

**Interfaces:**
- Consumes: `Caller.Watch()`（Task 2）
- Produces:
  - `func (m *Mount) startWatch()`
  - `func (m *Mount) stopWatch()`
  - `func (m *Mount) noteInode(fh uint64, path string)` —— 记录"我把哪些 inode 交给了内核"
  - `func (m *Mount) handedOut() []string` —— 快照，供作废用
  - `func (m *Mount) markStale()` / `func (m *Mount) stale() bool`
  - `func (m *Mount) applyEvent(ns []node.Node)`

- [ ] **Step 1: Write the failing test**

`internal/mount/watch_test.go`：

```go
package mount

import (
	"testing"
	"time"

	"peerfold/internal/node"
)

func TestEventUpdatesTheIndex(t *testing.T) {
	m := newTestMount(t)
	ch := make(chan []node.Node, 4)
	m.fake.watch = ch
	m.startWatch()
	defer m.stopWatch()

	ch <- []node.Node{{Path: "/new.txt", Kind: node.KindFile, Size: 3}}
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/new.txt")
		return code == 0
	}, "index did not pick up the event")
}

func TestDeletedEventRemovesTheEntry(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 3)
	ch := make(chan []node.Node, 4)
	m.fake.watch = ch
	m.startWatch()
	defer m.stopWatch()

	ch <- []node.Node{{Path: "/a.txt", Kind: node.KindDeleted}}
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/a.txt")
		return code == -ENOENT
	}, "deleted event did not remove the entry")
}

// The whole point of tracking how many events arrived: if the transport drops
// some, nothing else in the system notices, because a drop is not an error.
func TestDropIsDetected(t *testing.T) {
	m := newTestMount(t)
	ch := make(chan []node.Node, 4)
	m.fake.watch = ch
	m.startWatch()
	defer m.stopWatch()

	// A gap in the sequence means events were lost.
	m.fake.watch = nil
	m.applyEvent([]node.Node{{Path: "/a.txt", Kind: node.KindFile, Seq: 5}})
	if m.stale() {
		t.Error("a first, unexpected sequence should be adopted, not treated as a gap")
	}
	m.applyEvent([]node.Node{{Path: "/b.txt", Kind: node.KindFile, Seq: 9}})
	if !m.stale() {
		t.Error("a sequence gap was not detected: the tree can now be silently wrong forever")
	}
}

func TestHandedOutTracksInodes(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandle("/a.txt", node.KindFile)
	m.noteInode(fh, "/a.txt")
	got := m.handedOut()
	if len(got) != 1 || got[0] != "/a.txt" {
		t.Errorf("handedOut = %v, want [/a.txt]", got)
	}
	m.closeHandle(fh)
	if len(m.handedOut()) != 0 {
		t.Errorf("handedOut after close = %v, want empty", m.handedOut())
	}
}

// FUSE has no whole-filesystem-invalidate primitive, so invalidation has to
// walk this table. It must therefore be bounded: caching every path ever seen
// would grow without limit on a long-running mount.
func TestHandedOutIsBounded(t *testing.T) {
	m := newTestMount(t)
	m.opts.InvalidateTable = 8
	for i := 0; i < 100; i++ {
		fh := m.newHandle("/f", node.KindFile)
		m.noteInode(fh, "/f")
	}
	if got := len(m.handedOut()); got > m.opts.InvalidateTable {
		t.Errorf("handedOut holds %d entries, want at most %d", got, m.opts.InvalidateTable)
	}
}

func TestWatchClosureMarksDisconnected(t *testing.T) {
	m := newTestMount(t)
	ch := make(chan []node.Node, 1)
	m.fake.watch = ch
	m.startWatch()

	close(ch)
	waitFor(t, 2*time.Second, func() bool { return m.disconnected() },
		"a closed watch channel did not mark the mount disconnected")
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal(msg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestEvent|TestDeleted|TestDrop|TestHandedOut|TestWatchClosure' -v`
Expected: FAIL —— `undefined: (m *Mount).startWatch`、`undefined: node.Node.Seq`

- [ ] **Step 3: Add `Seq` to `proto.EventBody`**

在 `internal/proto/proto.go` 的 `EventBody` 加字段。**这是加法改动，不改帧结构**：

```go
// Seq is a per-connection monotonic counter. The mount layer uses it to notice
// that events were lost: the transport is allowed to drop an event when a
// consumer is slow (viewer.emit does exactly that), and a drop is not an error
// anywhere else in the system, so without a counter a stale tree is invisible.
type EventBody struct {
	Seq    uint64     `json:"seq,omitempty"`
	Nodes  []NodeJSON `json:"nodes,omitempty"`
	// ... existing fields stay exactly as they are
}
```

> **实现者注意**：`omitempty` 让 `Seq` 缺失时编码结果与今天**逐字节相同**，所以老的对端不受影响。请**不要**改成非 `omitempty`，那会让每个事件多几个字节并可能破坏现有的 golden 测试。先跑 `go test ./internal/proto/ -v` 确认 21 个测试仍然全绿。

- [ ] **Step 4: Expose the sequence from `internal/viewer/viewer.go`**

`viewer.emit` 在 `select/default:` 里丢事件。给它加一个**每连接的发送序号**并写入 `EventBody.Seq`。要点：

- 序号在**成功入队之后**递增（不是之前），所以丢弃的事件会造成一个可见的缺口
- **不要**为了修丢包而把 `default:` 改成阻塞 —— 那会把 `viewer.Watch` 变成可能永久卡住的东西，而它现在保证不阻塞是有意设计的。这个任务只做**检测**

- [ ] **Step 5: Make `server.broadcast` stop permanently deleting watchers**

现在 `internal/server/server.go:568-585` 在一次 `SendWithin(env, eventBroadcastTimeout)`（5s）失败后 `delete(s.watchers, c)`，连接仍可用于 `List`/`Read`，而变更**再也不会到达**。

改成：**不删除**，而是记录该 watcher 已经落后（在下一个事件里发一个序号缺口）并在**连续多次**失败后再丢弃，同时**记录日志**。要点：

- 一次 5s 超时不代表连接坏了；它可能只是一次调度抖动
- 保留一个连续失败计数，超过阈值（建议 3）才移除，并**明确记录**"这个 watcher 被移除，它现在会看到陈旧的树"
- 移除时**不要**静默：`log.Printf` 一条包含 client 名的消息

- [ ] **Step 6: Write `internal/mount/watch.go`**

```go
package mount

import (
	"log"
	"sync"

	"peerfold/internal/node"
)

// startWatch consumes the caller's event channel until it closes. It is the
// only goroutine that touches the mount's index and the handle table, so
// nothing else in the core needs a mutex for them.
func (m *Mount) startWatch() {
	m.watchOnce.Do(func() {
		m.watchDone = make(chan struct{})
		go m.watchLoop()
	})
}

func (m *Mount) stopWatch() {
	if m.watchDone != nil {
		close(m.watchDone)
		m.watchDone = nil
	}
}

func (m *Mount) watchLoop() {
	ch := m.caller.Watch()
	for {
		select {
		case <-m.watchDone:
			return
		case ns, ok := <-ch:
			if !ok {
				m.setDisconnected(true)
				log.Printf("mount: the event stream ended; the tree is now stale until a reconnect")
				return
			}
			m.applyEvent(ns)
		}
	}
}

// applyEvent merges a batch into the index and checks the sequence.
func (m *Mount) applyEvent(ns []node.Node) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(ns) > 0 && ns[0].Seq != 0 {
		seq := ns[0].Seq
		if m.lastSeq != 0 && seq != m.lastSeq+1 {
			// A gap means the transport dropped events. Nothing else in the
			// system reports this, because a drop is not an error, so if we
			// do not notice it here the tree is silently wrong forever.
			log.Printf("mount: event sequence jumped from %d to %d, so events were lost; the tree will be refreshed", m.lastSeq, seq)
			m.setStaleLocked(true)
		}
		m.lastSeq = seq
	}

	for _, n := range ns {
		if n.Kind == node.KindDeleted {
			delete(m.index, n.Path)
			continue
		}
		m.index[n.Path] = n
	}
}

// stale reports whether an event was lost and the tree can no longer be
// trusted. Task 11 decides what to do about it.
func (m *Mount) stale() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isStale
}

func (m *Mount) setStaleLocked(v bool) { m.isStale = v }

func (m *Mount) disconnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isDisconnected
}

func (m *Mount) setDisconnected(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isDisconnected = v
}

// noteInode records that this path's attributes were handed to the kernel, so
// a later invalidation has something to walk. FUSE has no
// whole-filesystem-invalidate primitive, which is why this table has to exist
// at all; the cap keeps it from growing without limit over a long run.
func (m *Mount) noteInode(fh uint64, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handed[fh] = path
	if len(m.handed) <= m.opts.InvalidateTable {
		return
	}
	// Drop the oldest entries. Map iteration order is random, which is
	// acceptable here: the table is a best-effort cache hint, and the
	// attr_timeout bounds how wrong a missed entry can be.
	for k := range m.handed {
		delete(m.handed, k)
		if len(m.handed) <= m.opts.InvalidateTable {
			return
		}
	}
}

// handedOut snapshots the paths the kernel may be caching.
func (m *Mount) handedOut() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.handed))
	for _, p := range m.handed {
		out = append(out, p)
	}
	return out
}
```

- [ ] **Step 7: Extend `Mount` and `openFile` to hold this state**

在 `handle.go` 的 `Mount` 里加字段：

```go
	mu             sync.Mutex
	index          map[string]node.Node
	lastSeq        uint64
	isStale        bool
	isDisconnected bool
	handed         map[uint64]string
	watchOnce      sync.Once
	watchDone      chan struct{}
```

在 `Options` 里加 `InvalidateTable int`，`DefaultOptions()` 里给 `4096`。在 `New` 里初始化 `index` 与 `handed`。

> **关于 `m.fake.watch`**：Task 2 的 `fakeCaller.Watch()` 现在返回一个固定空通道。加一个 `watch chan []node.Node` 字段，`Watch()` 优先返回它，这样测试可以自己灌事件。

> **重要：`index` 与 `lookup` 的关系。** `lookup` 现在走 `m.caller.List(parent)`（每次一个往返）。改成**先查本地 `index`**，未命中再 `List` 并填充。这既让被丢弃后的事件仍有意义，也让重复 `getattr` 不必每次都走网络 —— 这正是 `attr_timeout = 1s` 能起作用的前提。**这个改动会影响 Task 6/7 的现有测试**（它们依赖 `fake.List`），所以请在改完后重跑整个包。

- [ ] **Step 8: Run the whole mount package**

Run: `go test ./internal/mount/ -v 2>&1 | tail -25`
Expected: PASS

- [ ] **Step 9: Run the whole tree and the new proto test**

Run: `go test ./internal/proto/ ./internal/server/ ./internal/viewer/ ./internal/e2e/ -v 2>&1 | tail -25`
Expected: PASS

- [ ] **Step 10: Commit**

```bash
git add internal/proto/proto.go internal/viewer/viewer.go internal/server/server.go internal/mount/
git commit -m "feat(mount): detect dropped and lost update events

Two paths lose updates silently today. viewer.emit drops an event when its
channel is full, with no counter and no error. server.broadcast deletes a
watcher permanently after one failed 5s send, while the connection stays usable
for List and Read — so a client keeps being answered from a tree that stopped
changing. Neither is visible to anything else in the system, because a drop is
not an error.

EventBody gains an additive, omitempty Seq so a gap is observable; emit bumps
it only after a successful enqueue, so a dropped event leaves a hole rather
than being indistinguishable from a quiet period. broadcast no longer deletes a
watcher on the first timeout: one timeout is a scheduling hiccup, and three is
a pattern, so the count now has to reach a threshold and removal is logged
instead of silent.

emit deliberately still does not block: viewer.Watch guarantees not to block,
and making it block would trade a lost event for a stalled reader."
```

---

## Task 11: 重连 supervisor、作废与离线语义（D5 后半 + D11）

**Files:**
- Create: `internal/mount/reconnect.go`
- Test: `internal/mount/reconnect_test.go`

**Interfaces:**
- Consumes: Task 10（`stale`/`disconnected`/`handedOut`）
- Produces:
  - `type Reconnector func() (Caller, error)`
  - `func (m *Mount) supervise(rc Reconnector, giveUp time.Duration)`
  - `func (m *Mount) invalidateAll()` —— 遍历"交出去的表"，`AttrTimeout` 内重新 `getattr`
  - `func (m *Mount) offline() bool` —— 离线时所有操作返回 `-EIO`
  - `Options.ReconnectMin` / `Options.ReconnectMax` / `Options.GiveUpAfter`

- [ ] **Step 1: Write the failing test**

`internal/mount/reconnect_test.go`：

```go
package mount

import (
	"errors"
	"testing"
	"time"

	"peerfold/internal/node"
)

func TestReconnectResubscribesAndInvalidates(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond

	newCh := make(chan []node.Node, 4)
	reconnected := make(chan struct{}, 1)
	rc := func() (Caller, error) {
		f := newFakeCaller()
		f.addDir("/")
		f.addFile("/fresh.txt", 4)
		f.watch = newCh
		select {
		case reconnected <- struct{}{}:
		default:
		}
		return f, nil
	}

	m.setDisconnected(true)
	go m.supervise(rc, time.Second)

	waitFor(t, 2*time.Second, func() bool { return !m.disconnected() },
		"supervisor did not clear the disconnected flag after a successful reconnect")
	select {
	case <-reconnected:
	default:
		t.Fatal("supervisor never called the reconnect function")
	}
	// After a reconnect the tree must be re-read, not assumed unchanged: the
	// whole point is that events were missed while disconnected.
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/fresh.txt")
		return code == 0
	}, "the new tree was not picked up after reconnecting")
}

// Offline must be EIO, never ENOENT. A program that sees ENOENT concludes the
// file was deleted and may act destructively on that conclusion; EIO makes it
// retry.
func TestOfflineReadsAreEioNotEnoent(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 3)
	m.setDisconnected(true)

	var st Stat
	if code := m.Getattr("/a.txt", &st, 0); code != -EIO {
		t.Errorf("offline Getattr = %d, want -EIO (%d)", code, -EIO)
	}
	if _, code := m.Open("/a.txt", OReadOnly); code != -EIO {
		t.Errorf("offline Open = %d, want -EIO", code)
	}
	if code := m.Mkdir("/d", 0o755); code != -EIO {
		t.Errorf("offline Mkdir = %d, want -EIO", code)
	}
}

func TestReconnectBacksOff(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 40 * time.Millisecond

	attempts := 0
	var stamps []time.Time
	rc := func() (Caller, error) {
		attempts++
		stamps = append(stamps, time.Now())
		if attempts < 4 {
			return nil, errors.New("still down")
		}
		f := newFakeCaller()
		f.addDir("/")
		f.watch = make(chan []node.Node)
		return f, nil
	}

	m.setDisconnected(true)
	done := make(chan struct{})
	go func() { m.supervise(rc, time.Second); close(done) }()
	waitFor(t, 3*time.Second, func() bool { return !m.disconnected() }, "never reconnected")

	if len(stamps) < 2 {
		t.Fatalf("only %d attempts; backoff was not retried", len(stamps))
	}
	first := stamps[1].Sub(stamps[0])
	if first < 4*time.Millisecond {
		t.Errorf("retry after %v, want at least the minimum backoff (5ms)", first)
	}
}

// A mount whose owner is gone for minutes must stop being a mount, rather than
// hanging every program that touches it forever.
func TestGiveUpAfterThresholdUnmounts(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = time.Millisecond
	m.opts.ReconnectMax = 2 * time.Millisecond

	rc := func() (Caller, error) { return nil, errors.New("permanently down") }
	m.setDisconnected(true)

	unmounted := make(chan struct{}, 1)
	m.onGiveUp = func() { unmounted <- struct{}{} }

	go m.supervise(rc, 30*time.Millisecond)
	select {
	case <-unmounted:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor never gave up, so the mount would hang forever")
	}
}

// A dirty open at disconnect time must be reported and dropped, never silently
// replayed: replaying it would overwrite whatever the owner's copy became.
func TestDirtyOpensAreDroppedAndLoggedOnDisconnect(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)

	var reported []string
	m.onDiscard = func(path string) { reported = append(reported, path) }

	m.discardDirtyOnDisconnect()

	if len(reported) != 1 || reported[0] != "/a.txt" {
		t.Errorf("reported %v, want [/a.txt] — a discarded edit must be named, not silent", reported)
	}
	if got := string(m.fake.data["/a.txt"]); got != "hello" {
		t.Errorf("owner copy is %q; a disconnect must not have replayed the dirty write", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestReconnect|TestOffline|TestGiveUp|TestDirtyOpens' -v`
Expected: FAIL —— `undefined: (m *Mount).supervise`

- [ ] **Step 3: Write `internal/mount/reconnect.go`**

```go
package mount

import (
	"log"
	"time"
)

// Reconnector establishes a fresh Caller. The core never constructs one
// itself, so the mount layer stays independent of the connection setup and the
// tests can drive failures directly.
type Reconnector func() (Caller, error)

// offline reports whether the mount has no live connection. While it is true
// every operation answers -EIO.
func (m *Mount) offline() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isDisconnected
}

// supervise reconnects after the event stream ends, backing off from
// ReconnectMin to ReconnectMax, and gives up after giveUp with no success.
//
// It does not retry forever: a mount whose owner is gone for minutes has to
// stop being a mount, or every program that touches the path hangs. On giving
// up it calls onGiveUp, which unmounts.
func (m *Mount) supervise(rc Reconnector, giveUp time.Duration) {
	deadline := time.Now().Add(giveUp)
	backoff := m.opts.ReconnectMin
	if backoff <= 0 {
		backoff = time.Second
	}
	max := m.opts.ReconnectMax
	if max < backoff {
		max = 30 * time.Second
	}

	for {
		if time.Now().After(deadline) {
			log.Printf("mount: no connection for %s; unmounting so callers fail fast instead of hanging", giveUp)
			if m.onGiveUp != nil {
				m.onGiveUp()
			}
			return
		}
		caller, err := rc()
		if err != nil {
			log.Printf("mount: reconnect failed (%v); retrying in %s", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > max {
				backoff = max
			}
			continue
		}
		m.adopt(caller)
		log.Printf("mount: reconnected; refreshing every path the kernel may be caching")
		m.refreshAll()
		return
	}
}

// adopt installs a new caller and restarts the watch on it.
func (m *Mount) adopt(caller Caller) {
	m.stopWatch()
	m.mu.Lock()
	m.caller = caller
	m.isDisconnected = false
	m.isStale = true
	m.lastSeq = 0
	m.index = map[string]node.Node{}
	m.watchDone = nil
	m.watchOnce = sync.Once{}
	m.mu.Unlock()
	m.startWatch()
}

// refreshAll re-reads the paths the kernel may be holding attributes for, so a
// reconnect actually corrects the view rather than just restoring the channel.
func (m *Mount) refreshAll() {
	paths := m.handedOut()
	for _, p := range paths {
		if _, err := m.caller.List(parentOf(p)); err == nil {
			continue
		}
	}
	m.mu.Lock()
	m.isStale = false
	m.mu.Unlock()
	m.invalidateAll()
}

// invalidateAll walks the "handed to the kernel" table and asks the host to
// drop its cached attributes. FUSE has no whole-filesystem-invalidate
// primitive, so this is the only way; host.go supplies the primitive.
func (m *Mount) invalidateAll() {
	if m.invalidatePath == nil {
		return
	}
	for _, p := range m.handedOut() {
		m.invalidatePath(p)
	}
}

// parentOf is path.Dir without importing path here a second time.
func parentOf(p string) string {
	if i := lastSlash(p); i > 0 {
		return p[:i]
	}
	return "/"
}

// discardDirtyOnDisconnect drops every dirty handle, reporting each path. It
// never replays them: the owner's copy may have changed while this mount was
// disconnected, and replaying a stale buffer would overwrite that change with
// no way to recover it.
func (m *Mount) discardDirtyOnDisconnect() {
	m.mu.Lock()
	var dirty []*openFile
	for _, h := range m.files {
		if h.dirty {
			dirty = append(dirty, h)
		}
	}
	m.mu.Unlock()
	for _, h := range dirty {
		log.Printf("mount: discarding unflushed edits to %s (the connection was lost)", h.path)
		if m.onDiscard != nil {
			m.onDiscard(h.path)
		}
		m.closeHandle(h.fh)
	}
}
```

- [ ] **Step 4: Add the hook fields to `Mount`**

在 `handle.go` 的 `Mount` 里加：

```go
	onGiveUp       func()
	onDiscard      func(string)
	invalidatePath func(string)
```

在 `Options` 里加 `ReconnectMin`、`ReconnectMax`、`GiveUpAfter`；`DefaultOptions` 给 `1s`/`30s`/`2min`。

- [ ] **Step 5: Make every operation honour the offline state**

在 `ops.go` 里，每个**会碰网络**的方法开头加：

```go
	if m.offline() {
		return -EIO
	}
```

**要加的位置**：`Getattr`（只在 `index` 未命中时）、`Readdir`、`Open`、`Opendir`、`Read`、`Write`、`Truncate`（path 形式）、`Create`、`Mkdir`、`Unlink`、`Rmdir`、`Rename`、`Fsync`、`Flush`、`Release`（`flush` 内部）。

> **一个必须注意的例外**：`Release` **不能**因为离线就返回 `EIO` 而不释放句柄 —— 内核不再关心它了，泄漏句柄比返回错误的 errno 更糟。`Release` 应该照样释放并返回 `-EIO`。

> **关于 `m.caller` 的替换**：`adopt` 会在另一个 goroutine 里改 `m.caller`，而操作核是从 cgofuse 的线程调用的。所以 `m.caller` 的读写要**走同一个 mutex**，或者把 `Caller` 换成一个原子的间接层。请实现者选简单可靠的那个（建议：给 `Mount` 加 `func (m *Mount) call() Caller` 在锁下取值，所有调用点改为 `m.call().List(...)`）。**漏掉这一步会变成数据竞争**，而本机没有 `-race` 能发现它。

- [ ] **Step 6: Run the whole mount package**

Run: `go test ./internal/mount/ -v 2>&1 | tail -25`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/mount/
git commit -m "feat(mount): reconnect with backoff, invalidate, and go offline as EIO

supervise backs off from 1s to 30s, re-subscribes on success, and refreshes
every path the kernel may be caching, because the point of reconnecting is that
events were missed while disconnected. It gives up after two minutes and
unmounts: a mount whose owner is gone must stop being a mount, or every program
that touches the path hangs forever.

Offline is EIO and never ENOENT. A program that sees ENOENT concludes the file
was deleted and may act destructively on that conclusion; EIO makes it retry.

A dirty open at disconnect time is reported by path and dropped, never
replayed. Replaying a stale buffer over whatever the owner's copy became would
destroy data with no way to recover it."
```

---

## Task 12: `cgofuseHost` —— 33 个方法的适配器

**背景**：`fuse.FileSystemBase` 是**空结构体**（没有默认实现），所以 `FileSystemInterface` 的 **33 个方法全都要实现**。这个任务是纯翻译，逻辑密度低，但**方法必须一个不少**，否则编译不过（这反而是好事：编译器会把清单钉死）。

**Files:**
- Create: `internal/mount/host.go`
- Test: `internal/mount/host_test.go`

**Interfaces:**
- Consumes: Tasks 2–11（操作核）、`github.com/winfsp/cgofuse/fuse`
- Produces:
  - `type cgofuseHost struct { *fuse.FileSystemBase; m *Mount }` —— **注意：`FileSystemBase` 是空结构体，所以必须显式实现所有方法**，不能靠继承
  - `func NewHost(caller Caller, opts Options) *fuse.FileSystemHost` —— 构造 `Mount`、包成 host、`SetCapReaddirPlus(true)`，把 `host.Notify` 接到 `m.invalidatePath`
  - `type viewerCaller struct { v *viewer.Viewer }` —— 实现 `Caller`，让真实 `*viewer.Viewer` 可用
  - `func NewViewerCaller(v *viewer.Viewer) Caller`

- [ ] **Step 1: Write the failing test**

`internal/mount/host_test.go`：

```go
package mount

import (
	"reflect"
	"testing"

	"github.com/winfsp/cgofuse/fuse"
)

// FileSystemBase is an empty struct, so every method of the interface has to
// be implemented on the adapter explicitly. This test fails to compile if one
// is missing, which is the point: the compiler is the checklist.
var _ fuse.FileSystemInterface = (*cgofuseHost)(nil)

func TestHostImplementsEveryMethod(t *testing.T) {
	want := reflect.TypeOf((*fuse.FileSystemInterface)(nil)).Elem().NumMethod()
	got := reflect.TypeOf(&cgofuseHost{}).NumMethod()
	if got < want {
		t.Errorf("cgofuseHost has %d methods, the interface has %d", got, want)
	}
	if want != 33 {
		t.Logf("note: the interface has %d methods, not the 33 this plan recorded; check for a cgofuse upgrade", want)
	}
}

func TestUnsupportedOperationsAreEnosys(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	if code := h.Chmod("/a", 0o644); code != -ENOSYS {
		t.Errorf("Chmod = %d, want -ENOSYS", code)
	}
	if code := h.Chown("/a", 0, 0); code != -ENOSYS {
		t.Errorf("Chown = %d, want -ENOSYS", code)
	}
	if code := h.Symlink("/a", "/b"); code != -ENOSYS {
		t.Errorf("Symlink = %d, want -ENOSYS: creating links through the mount is not supported", code)
	}
	if code := h.Link("/a", "/b"); code != -ENOSYS {
		t.Errorf("Link = %d, want -ENOSYS", code)
	}
	if code := h.Mknod("/a", 0, 0); code != -ENOSYS {
		t.Errorf("Mknod = %d, want -ENOSYS", code)
	}
	if code := h.Setxattr("/a", "x", nil, 0); code != -ENOSYS {
		t.Errorf("Setxattr = %d, want -ENOSYS", code)
	}
	if code := h.Utimens("/a", nil); code != -ENOSYS {
		t.Errorf("Utimens = %d, want -ENOSYS", code)
	}
}

func TestStatConversion(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 42)
	h := &cgofuseHost{m: m}

	var st fuse.Stat_t
	if code := h.Getattr("/a.txt", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if st.Size != 42 {
		t.Errorf("Size = %d, want 42", st.Size)
	}
	if st.Mode&fuse.S_IFREG == 0 {
		t.Errorf("Mode = %o, want the regular-file bit set", st.Mode)
	}
	if st.Mtim.Sec == 0 {
		t.Log("note: index Mtime was 0 in this fixture, so Mtim is legitimately 0")
	}
}

func TestStatDirectoryConversion(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	h := &cgofuseHost{m: m}

	var st fuse.Stat_t
	if code := h.Getattr("/d", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if st.Mode&fuse.S_IFDIR == 0 {
		t.Errorf("Mode = %o, want the directory bit set", st.Mode)
	}
}

// cgofuse's Read returns a byte count, so the adapter must pass the core's
// count straight through and not re-encode it.
func TestReadReturnsByteCount(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	h := &cgofuseHost{m: m}

	fh, code := h.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	buf := make([]byte, 16)
	if n := h.Read("/a.txt", buf, 0, fh); n != 5 {
		t.Errorf("Read = %d, want 5", n)
	}
	if string(buf[:5]) != "hello" {
		t.Errorf("content = %q", buf[:5])
	}
}

func TestWriteReturnsByteCount(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 0)
	h := &cgofuseHost{m: m}

	fh, _ := h.Open("/a.txt", OReadWrite)
	if n := h.Write("/a.txt", []byte("hello"), 0, fh); n != 5 {
		t.Errorf("Write = %d, want 5", n)
	}
	if code := h.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
}

// The fill callback cgofuse passes has a different shape from the core's, so
// the adapter must translate it, including the offset argument.
func TestReaddirTranslatesFill(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	m.fake.addFile("/d/a.txt", 1)
	h := &cgofuseHost{m: m}

	var names []string
	code := h.Readdir("/d", func(name string, st *fuse.Stat_t, ofst int64) bool {
		names = append(names, name)
		return true
	}, 0, 0)
	if code != 0 {
		t.Fatalf("Readdir = %d", code)
	}
	found := false
	for _, n := range names {
		if n == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("a.txt missing from %v", names)
	}
}

func TestStatfsConversion(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}
	var st fuse.Statfs_t
	if code := h.Statfs("/", &st); code != 0 {
		t.Fatalf("Statfs = %d", code)
	}
	if st.Bsize == 0 || st.Namemax == 0 {
		t.Errorf("Statfs gave Bsize=%d Namemax=%d, want both non-zero", st.Bsize, st.Namemax)
	}
}

func TestTruncateAndRelease(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	h := &cgofuseHost{m: m}

	fh, _ := h.Open("/a.txt", OReadWrite)
	h.Write("/a.txt", []byte("HELLO"), 0, fh)
	if code := h.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after release = %q, want HELLO", got)
	}
}

// Directories must be opened with Opendir, not Open: the two have different
// error codes and some programs rely on the difference.
func TestOpendirAndReleasedir(t *testing.T) {
	m := newTestMount(t)
	m.fake.addDir("/d")
	h := &cgofuseHost{m: m}

	fh, code := h.Opendir("/d")
	if code != 0 {
		t.Fatalf("Opendir = %d", code)
	}
	if code := h.Releasedir("/d", fh); code != 0 {
		t.Errorf("Releasedir = %d", code)
	}
}

func TestCreateAndMkdirThroughHost(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	if code := h.Mkdir("/d", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d", code)
	}
	fh, code := h.Create("/d/new.txt", 0, 0o644)
	if code != 0 {
		t.Fatalf("Create = %d", code)
	}
	h.Write("/d/new.txt", []byte("hi"), 0, fh)
	if code := h.Release("/d/new.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := string(m.fake.data["/d/new.txt"]); got != "hi" {
		t.Errorf("created file content = %q, want hi", got)
	}
}

func TestUnlinkRenameRmdirThroughHost(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 1)
	m.fake.data["/a.txt"] = []byte("x")
	h := &cgofuseHost{m: m}

	if code := h.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	if code := h.Unlink("/b.txt"); code != 0 {
		t.Fatalf("Unlink = %d", code)
	}
	m.fake.addDir("/empty")
	if code := h.Rmdir("/empty"); code != 0 {
		t.Fatalf("Rmdir = %d", code)
	}
}

// Fsync and Flush exist on the cgofuse interface; both are in the forced-flush
// table and must reach the core.
func TestFsyncAndFlushThroughHost(t *testing.T) {
	m := newTestMount(t)
	m.fake.addFile("/a.txt", 5)
	m.fake.data["/a.txt"] = []byte("hello")
	h := &cgofuseHost{m: m}

	fh, _ := h.Open("/a.txt", OReadWrite)
	h.Write("/a.txt", []byte("HELLO"), 0, fh)
	if code := h.Fsync("/a.txt", false, fh); code != 0 {
		t.Fatalf("Fsync = %d", code)
	}
	if got := string(m.fake.data["/a.txt"]); got != "HELLO" {
		t.Errorf("fsync through the host did not write: %q", got)
	}
	if code := h.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
}

// Getxattr and Listxattr must answer with no attributes rather than an error:
// a filesystem that errors can make some tools refuse to copy into it.
func TestXattrReadsReturnEmpty(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	if code, data := h.Getxattr("/a", "user.x"); code != -ENODATA && code != -ENOSYS && code != 0 {
		t.Errorf("Getxattr = %d, want 0, -ENODATA or -ENOSYS", code)
		_ = data
	}
	if code := h.Listxattr("/a", func(string) bool { return true }); code != 0 {
		t.Errorf("Listxattr = %d, want 0", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestHost|TestUnsupported|TestStat|TestReadReturns|TestWriteReturns|TestReaddirTranslates|TestOpendir|TestCreateAndMkdir|TestUnlinkRename|TestFsyncAndFlush|TestXattr' -v`
Expected: FAIL —— `undefined: cgofuseHost`

- [ ] **Step 3: Write `internal/mount/host.go`**

这是 33 个方法。**关键点**：

- `FileSystemBase` 是空结构体 —— 嵌入它没有提供任何默认实现，所以下面的方法全都要写
- `Read`/`Write` 的返回值是**字节数**，与核一致，直接透传
- `Readdir` 的 `fill` 签名与核不同（多了 `*fuse.Stat_t`），要翻译；核传 `nil` 的 stat 表示"."/".."，宿主直接传 `nil`
- `Stat` → `fuse.Stat_t`：`Mode` 直接放（核已经用了 `S_IFDIR`/`S_IFREG` 的位），`Size` 放 `Size`，`Mtime`（Unix 纳秒）拆成 `Mtim.Sec`/`Mtim.Nsec`
- **`Notify` 的接法**：`fuse.FileSystemHost.Notify(path, fuse.Notify*Mask)` 是 D5 的内核失效接口。在 `NewHost` 里把 `m.invalidatePath = func(p string) { host.Notify(p, fuse.NotifyAll) }`，但**注意 `host` 在包完之前还不存在**，所以要么用闭包捕获一个指针，要么在构造后用一次赋值。**必须确认 cgofuse 的 `Notify` 常量名**（`fuse.NotifyAll` / `NotifySelf` / `NotifyDir` / `NotifyDelete` / `NotifySize`）；用 `go doc github.com/winfsp/cgofuse/fuse FileSystemHost.Notify` 确认
- 不支持的操作返回 `-ENOSYS`：`Chmod`、`Chown`、`Link`、`Symlink`、`Mknod`、`Setxattr`、`Removexattr`、`Utimens`、`Access`
- `Getxattr` 返回 `(-ENODATA, nil)`（不是 `ENOSYS`：有些工具对 `ENOSYS` 的反应更糟）
- `Listxattr` 返回 0 且不调用 `fill`
- `Init`/`Destroy` 在 `Destroy` 里 `m.stopWatch()` 与最终冲刷；`Init` 里 `m.startWatch()`
- `Statfs` 用 `fuse.Statfs_t` 的全部字段（`Bsize`/`Frsize`/`Blocks`/`Bfree`/`Bavail`/`Files`/`Ffree`/`Favail`/`Namemax`）

完整骨架（实现者补齐方法体）：

```go
package mount

import (
	"context"
	"io"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"peerfold/internal/viewer"
)

// cgofuseHost adapts the operation core to the FUSE interface.
//
// fuse.FileSystemBase is an empty struct, so it provides no default
// implementations: every method of fuse.FileSystemInterface has to be written
// out here. host_test.go asserts the count, because a missing method is a
// compile error only where the interface is claimed.
type cgofuseHost struct {
	fuse.FileSystemBase
	m *Mount
}

// viewerCaller adapts *viewer.Viewer to Caller.
type viewerCaller struct{ v *viewer.Viewer }

func NewViewerCaller(v *viewer.Viewer) Caller { return &viewerCaller{v: v} }

func (c *viewerCaller) List(path string) ([]node.Node, error) { return c.v.List(path) }
func (c *viewerCaller) Read(path string, off int64, n int) ([]byte, error) {
	return c.v.Read(path, off, n)
}
func (c *viewerCaller) Put(path string, r io.Reader) error { return c.v.Put(path, r) }
func (c *viewerCaller) Mkdir(path string) error            { return c.v.Mkdir(path) }
func (c *viewerCaller) Remove(path string) error           { return c.v.Remove(path) }
func (c *viewerCaller) Rename(a, b string) error           { return c.v.Rename(a, b) }
func (c *viewerCaller) Watch() <-chan []node.Node          { return c.v.Watch() }

// NewHost builds the cgofuse host around the operation core.
func NewHost(caller Caller, opts Options) *fuse.FileSystemHost {
	m := New(caller, opts)
	h := &cgofuseHost{m: m}
	host := fuse.NewFileSystemHost(h)
	// Windows caches directory entries, so connect and disconnect have to be
	// reported as index changes or a reconnecting client is invisible.
	host.SetCapReaddirPlus(true)
	m.invalidatePath = func(p string) { host.Notify(p, fuse.NotifyAll) }
	return host
}

func (h *cgofuseHost) Init() {
	h.m.startWatch()
}

func (h *cgofuseHost) Destroy() {
	h.m.stopWatch()
	h.m.flushAll()
}

func (h *cgofuseHost) Getattr(path string, st *fuse.Stat_t, fh uint64) int {
	var core Stat
	if code := h.m.Getattr(path, &core, fh); code != 0 {
		return code
	}
	h.m.noteInode(fh, path)
	*st = fuse.Stat_t{
		Mode:  core.Mode,
		Nlink: core.Nlink,
		Size:  core.Size,
		Mtim:  fuse.NewTimespec(time.Unix(0, core.Mtime)),
		Atim:  fuse.NewTimespec(time.Unix(0, core.Mtime)),
		Ctim:  fuse.NewTimespec(time.Unix(0, core.Mtime)),
	}
	return 0
}

func (h *cgofuseHost) Readdir(path string, fill func(name string, st *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	return h.m.Readdir(path, func(name string, st *Stat, _ int64) bool {
		if st == nil {
			return fill(name, nil, ofst)
		}
		fs := fuse.Stat_t{
			Mode:  st.Mode,
			Nlink: st.Nlink,
			Size:  st.Size,
			Mtim:  fuse.NewTimespec(time.Unix(0, st.Mtime)),
		}
		return fill(name, &fs, ofst)
	}, ofst, fh)
}

func (h *cgofuseHost) Statfs(path string, st *fuse.Statfs_t) int {
	var core Statfs
	if code := h.m.Statfs(path, &core); code != 0 {
		return code
	}
	*st = fuse.Statfs_t{
		Bsize:   core.Bsize,
		Frsize:  core.Bsize,
		Blocks:  core.Blocks,
		Bfree:   core.Bfree,
		Bavail:  core.Bavail,
		Files:   core.Files,
		Ffree:   core.Ffree,
		Favail:  core.Ffree,
		Namemax: core.Namemax,
	}
	return 0
}

func (h *cgofuseHost) Open(path string, flags int) (int, uint64) {
	fh, code := h.m.Open(path, flags)
	return code, fh
}

func (h *cgofuseHost) Opendir(path string) (int, uint64) {
	fh, code := h.m.Opendir(path)
	return code, fh
}

func (h *cgofuseHost) Create(path string, flags int, mode uint32) (int, uint64) {
	fh, code := h.m.Create(path, mode)
	return code, fh
}

func (h *cgofuseHost) Read(path string, buff []byte, ofst int64, fh uint64) int {
	return h.m.Read(path, buff, ofst, fh)
}

func (h *cgofuseHost) Write(path string, buff []byte, ofst int64, fh uint64) int {
	return h.m.Write(path, buff, ofst, fh)
}

func (h *cgofuseHost) Truncate(path string, size int64, fh uint64) int {
	return h.m.Truncate(path, size, fh)
}

func (h *cgofuseHost) Fsync(path string, datasync bool, fh uint64) int {
	return h.m.Fsync(path, datasync, fh)
}

func (h *cgofuseHost) Flush(path string, fh uint64) int { return h.m.Flush(path, fh) }

func (h *cgofuseHost) Release(path string, fh uint64) int { return h.m.Release(path, fh) }

func (h *cgofuseHost) Releasedir(path string, fh uint64) int { return h.m.Release(path, fh) }

func (h *cgofuseHost) Unlink(path string) int { return h.m.Unlink(path) }
func (h *cgofuseHost) Rmdir(path string) int  { return h.m.Rmdir(path) }
func (h *cgofuseHost) Mkdir(path string, mode uint32) int { return h.m.Mkdir(path, mode) }
func (h *cgofuseHost) Rename(oldpath, newpath string) int { return h.m.Rename(oldpath, newpath) }

func (h *cgofuseHost) Fsyncdir(path string, datasync bool, fh uint64) int {
	return h.m.Flush(path, fh)
}

// Getxattr answers "no such attribute" rather than ENOSYS. Some tools treat
// ENOSYS as a broken filesystem and refuse to copy into it.
func (h *cgofuseHost) Getxattr(path string, name string) (int, []byte) {
	return -ENODATA, nil
}

func (h *cgofuseHost) Listxattr(path string, fill func(name string) bool) int { return 0 }

// The rest are not part of a peerfold share and answer ENOSYS.
func (h *cgofuseHost) Access(path string, mask uint32) int          { return -ENOSYS }
func (h *cgofuseHost) Chmod(path string, mode uint32) int           { return -ENOSYS }
func (h *cgofuseHost) Chown(path string, uid, gid uint32) int       { return -ENOSYS }
func (h *cgofuseHost) Link(oldpath, newpath string) int             { return -ENOSYS }
func (h *cgofuseHost) Mknod(path string, mode uint32, dev uint64) int { return -ENOSYS }
func (h *cgofuseHost) Readlink(path string) (int, string)           { return -ENOSYS, "" }
func (h *cgofuseHost) Removexattr(path string, name string) int     { return -ENOSYS }
func (h *cgofuseHost) Setxattr(path string, name string, value []byte, flags int) int {
	return -ENOSYS
}
func (h *cgofuseHost) Symlink(target string, newpath string) int { return -ENOSYS }
func (h *cgofuseHost) Utimens(path string, tmsp []fuse.Timespec) int { return -ENOSYS }
```

- [ ] **Step 4: Add `flushAll` and `ENODATA`**

`ops.go` 加：

```go
// flushAll writes every dirty handle. Called on unmount and on SIGINT, because
// neither is a crash and both are chances to save what the user typed.
func (m *Mount) flushAll() {
	m.mu.Lock()
	fhs := make([]uint64, 0, len(m.files))
	for fh := range m.files {
		fhs = append(fhs, fh)
	}
	m.mu.Unlock()
	for _, fh := range fhs {
		if code := m.flush(fh); code != 0 {
			log.Printf("mount: flushing handle %d failed (%d); the owner did not accept it", fh, code)
		}
	}
}
```

`errno.go` 加 `ENODATA = 61`（cgofuse 的值；用 `go doc` 确认后填准确）。

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/mount/ -v -run 'TestHost|TestUnsupported|TestStat|TestReadReturns|TestWriteReturns|TestReaddirTranslates|TestOpendir|TestCreateAndMkdir|TestUnlinkRename|TestFsyncAndFlush|TestXattr' 2>&1 | tail -25`
Expected: PASS

- [ ] **Step 6: Confirm the real cgofuse signatures with `go doc`**

Run: `go doc github.com/winfsp/cgofuse/fuse FileSystemHost.Notify`
Run: `go doc github.com/winfsp/cgofuse/fuse NotifyAll`
Run: `go doc github.com/winfsp/cgofuse/fuse NewTimespec`
Run: `go doc github.com/winfsp/cgofuse/fuse ENODATA`
Expected: 四个都打印出定义。**如果常量或函数名与上面写的不一样，按实际输出改**，并以实际输出为准。

- [ ] **Step 7: Build both platforms**

Run: `$env:CGO_ENABLED='0'; $env:GOOS='windows'; go build ./... 2>&1 | tail -10; echo "WINDOWS=$LASTEXITCODE"`
Expected: `WINDOWS=0`

Run: `$env:CGO_ENABLED='0'; $env:GOOS='linux'; go build ./internal/mount/ 2>&1 | tail -10; echo "LINUX_BUILD=$LASTEXITCODE"`
Expected: `LINUX_BUILD=0` —— `cgofuse` 在 **cgo 关闭**时 Linux 会编译失败，所以这条**可能非零**。如果非零，说明 Linux 侧必须有 cgo；那就把 Linux 的构建检查改成 `CGO_ENABLED=1 GOOS=linux`（本机没有 C 工具链，所以这条**只能在本机的交叉编译里失败**，如实记录并把 Linux 构建验证**列入"待外部验证"**）。

- [ ] **Step 8: Commit**

```bash
git add internal/mount/host.go internal/mount/host_test.go internal/mount/ops.go internal/mount/errno.go
git commit -m "feat(mount): cgofuse adapter implementing all 33 methods

fuse.FileSystemBase is an empty struct, so it supplies no default
implementations and every method of fuse.FileSystemInterface has to be written
out. host_test.go counts them, because a missing method is only a compile error
where the interface is claimed.

Read and Write pass the core's byte count straight through, since cgofuse uses
the same convention. Readdir translates the fill callback, which has a
different shape. Getxattr answers ENODATA rather than ENOSYS, because some
tools treat ENOSYS as a broken filesystem and refuse to copy into it.

The host wires FileSystemHost.Notify to the core's invalidation hook, which is
the only kernel-cache primitive FUSE offers and the reason the handed-out table
exists."
```

---

## Task 13: 平台壳 —— 挂载点语义在这一层分叉

**这是唯一一个两平台行为**必须**不同的地方。** Linux 要求挂载目录**已存在**，Windows 要求它**不存在**（WinFsp 自己创建）。实测依据见设计文档 §十六。

**Files:**
- Create: `internal/mount/runner.go`
- Create: `internal/mount/runner_windows.go`
- Create: `internal/mount/runner_linux.go`
- Test: `internal/mount/runner_test.go`

**Interfaces:**
- Consumes: Task 12（`NewHost`）
- Produces:
  - `type Runner struct { Mount string; Caller Caller; Opts Options; Reconnect Reconnector }`
  - `func (r *Runner) Run(ctx context.Context) error`
  - `func DefaultMountPoint() (string, error)` —— 平台相关，`~/peerfold` 或 `%USERPROFILE%\peerfold`
  - `func prepareMountPoint(path string) (cleanup func(), err error)` —— **平台相关，这是分叉所在**
  - `func mountArgs(opts Options) []string` —— 两个超时选项

- [ ] **Step 1: Write the failing test**

`internal/mount/runner_test.go`（**平台无关的部分**）：

```go
package mount

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultMountPointIsUnderHome(t *testing.T) {
	p, err := DefaultMountPoint()
	if err != nil {
		t.Fatalf("DefaultMountPoint: %v", err)
	}
	if !strings.HasSuffix(p, "peerfold") {
		t.Errorf("DefaultMountPoint = %q, want it to end in peerfold", p)
	}
	// It must be an absolute path: a relative mount point depends on the
	// process's working directory, which nobody expects.
	if !filepath.IsAbs(p) {
		t.Errorf("DefaultMountPoint = %q, want an absolute path", p)
	}
}

// The two timeouts are what bound how wrong the tree can be if the entire
// notification path fails silently, so they must actually be passed.
func TestMountArgsCarryTheTimeouts(t *testing.T) {
	opts := DefaultOptions()
	opts.AttrTimeout = 1500 * time.Millisecond
	opts.EntryTimeout = 2500 * time.Millisecond

	args := mountArgs(opts)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "attr_timeout=1.5") {
		t.Errorf("args %v do not set attr_timeout", args)
	}
	if !strings.Contains(joined, "entry_timeout=2.5") {
		t.Errorf("args %v do not set entry_timeout", args)
	}
	// negative_timeout must be 0: a cached negative lookup would hide a file
	// that another client just created.
	if !strings.Contains(joined, "negative_timeout=0") {
		t.Errorf("args %v do not set negative_timeout=0", args)
	}
}

// The mount point must not be pre-created on Windows. This is asserted here as
// a property of the shared code path so the Windows branch cannot quietly
// reintroduce the bug that cost an afternoon: WinFsp creates the directory
// itself and reports "mount point in use" if it already exists.
func TestPrepareMountPointIsPlatformSpecific(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mp")
	cleanup, err := prepareMountPoint(p)
	if err != nil {
		t.Fatalf("prepareMountPoint: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if _, err := os.Stat(p); err != nil && !os.IsNotExist(err) {
		t.Errorf("unexpected error stat-ing the mount point: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mount/ -run 'TestDefaultMountPoint|TestMountArgs|TestPrepareMountPoint' -v`
Expected: FAIL —— `undefined: DefaultMountPoint`

- [ ] **Step 3: Write `internal/mount/runner.go`**

```go
package mount

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// Runner holds a mount's configuration and runs it.
type Runner struct {
	Mount     string
	Caller    Caller
	Opts      Options
	Reconnect Reconnector
}

// mountArgs builds the FUSE option list. The three timeouts are the bound on
// how long the kernel may serve a stale answer if the entire notification path
// fails silently: a positive attr and entry timeout keep getattr from
// round-tripping on every access, and negative_timeout is 0 so a file another
// client just created is never hidden by a cached miss.
func mountArgs(opts Options) []string {
	secs := func(d time.Duration) string {
		return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	}
	return []string{
		"-o", "attr_timeout=" + secs(opts.AttrTimeout),
		"-o", "entry_timeout=" + secs(opts.EntryTimeout),
		"-o", "negative_timeout=0",
	}
}

// Run mounts and blocks until the mount goes away or ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	cleanup, err := prepareMountPoint(r.Mount)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	host := NewHost(r.Caller, r.Opts)
	m := hostCore(host)

	if r.Reconnect != nil {
		// The supervisor takes over after the event stream ends; it unmounts
		// by itself when the owner has been gone for too long.
		m.onGiveUp = func() { host.Unmount() }
		go func() {
			for {
				time.Sleep(200 * time.Millisecond)
				if m.disconnected() {
					m.supervise(r.Reconnect, r.Opts.GiveUpAfter)
					return
				}
			}
		}()
	}

	// SIGINT and SIGTERM are not crashes. Flushing here is the difference
	// between an interrupted session saving the user's last edit and losing it.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		select {
		case <-sig:
			log.Printf("mount: interrupted; flushing before unmounting")
			m.flushAll()
			host.Unmount()
		case <-ctx.Done():
			m.flushAll()
			host.Unmount()
		}
	}()

	log.Printf("mount: %s is ready", r.Mount)
	// Mount takes over the main loop and blocks until the filesystem goes
	// away. File IO must come from another process while this is running:
	// reading the mount point from this process deadlocks.
	if !host.Mount(r.Mount, mountArgs(r.Opts)) {
		return fmt.Errorf("mount: %s could not be mounted", r.Mount)
	}
	return nil
}
```

> **`hostCore` 与 `Runner.Reconnect` 的关系**：`NewHost` 返回的是 `*fuse.FileSystemHost`，操作核藏在 `cgofuseHost` 里。为了让 `Runner` 拿到它（`onGiveUp`、`disconnected`、`supervise` 都要用），给 `NewHost` 加一个返回核的变体，或者让 `NewHost` 返回 `(*fuse.FileSystemHost, *Mount)`。**建议后者**，它把"宿主与核"的配对关系显式化。计划里用 `hostCore(host)` 只是为了阅读清晰；实现者请直接把 `NewHost` 的签名改成返回两者。**这属于接口调整，不是设计变更。**

- [ ] **Step 4: Write `internal/mount/runner_windows.go`**

```go
//go:build windows

package mount

import (
	"os"
	"path/filepath"
)

// DefaultMountPoint is %USERPROFILE%\peerfold.
func DefaultMountPoint() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "peerfold"), nil
}

// prepareMountPoint makes sure the mount point does NOT exist.
//
// WinFsp creates the directory itself when it mounts and deletes it when the
// filesystem goes away (the directory is opened with FILE_FLAG_DELETE_ON_CLOSE
// and the mount is a reparse point). If the directory already exists, WinFsp
// refuses with "mount point in use" and Mount returns false immediately.
//
// This was measured, not assumed: an afternoon went into chasing an
// elevation theory because the probe pre-created the directory. See the design
// document's section 16.
func prepareMountPoint(path string) (cleanup func(), err error) {
	if _, statErr := os.Lstat(path); statErr == nil {
		// It exists. If it is empty, remove it so WinFsp can create its own.
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return nil, readErr
		}
		if len(entries) == 0 {
			return nil, os.Remove(path)
		}
		// A non-empty directory is either a stale mount or somebody's data.
		// Do not delete it; refuse and let the operator decide.
		return nil, errStr("mount: " + path + " exists and is not empty; " +
			"it is either a stale mount point or a real directory, and neither " +
			"should be removed automatically")
	}
	return nil, nil
}
```

> **注意 cleanup 是 nil**：WinFsp 自己管理那个目录，所以**不要**注册一个"卸载后删目录"的 cleanup —— 那会和 WinFsp 抢同一个动作。

- [ ] **Step 5: Write `internal/mount/runner_linux.go`**

```go
//go:build linux

package mount

import (
	"os"
	"path/filepath"
)

// DefaultMountPoint is ~/peerfold.
func DefaultMountPoint() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "peerfold"), nil
}

// prepareMountPoint makes sure the mount point EXISTS and is empty.
//
// This is the opposite of Windows, and the difference is not cosmetic: Linux
// FUSE mounts over an existing directory and hides its contents, while WinFsp
// creates the directory and refuses if it is already there. That is exactly why
// this behaviour lives in a platform file and not in the portable core.
func prepareMountPoint(path string) (cleanup func(), err error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		// Mounting over a non-empty directory would hide its contents without
		// warning, which is indistinguishable from losing them.
		return nil, errStr("mount: " + path + " is not empty; " +
			"mounting here would hide its contents without warning")
	}
	// Deliberately no cleanup that removes the directory: the operator created
	// it (or we did, on their behalf) and it may be pointed at again.
	return nil, nil
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/mount/ -run 'TestDefaultMountPoint|TestMountArgs|TestPrepareMountPoint' -v`
Expected: PASS（Windows 上跑 Windows 分支）

- [ ] **Step 7: Verify both platform files compile**

Run: `$env:CGO_ENABLED='0'; $env:GOOS='windows'; go vet ./internal/mount/ 2>&1 | tail -5; echo "WIN=$LASTEXITCODE"`
Expected: `WIN=0`

Run: `$env:CGO_ENABLED='1'; $env:GOOS='linux'; go vet ./internal/mount/ 2>&1 | tail -5; echo "LINUX=$LASTEXITCODE"`
Expected: 视本机有无 C 工具链而定。**如果失败是因为缺 `libfuse`，那是预期的**（Linux 侧需要 cgo + libfuse 头文件），如实记录为"待外部验证"，不要因此改设计。

- [ ] **Step 8: Commit**

```bash
git add internal/mount/runner.go internal/mount/runner_windows.go internal/mount/runner_linux.go internal/mount/runner_test.go
git commit -m "feat(mount): platform runners, where the mount-point semantics diverge

Linux requires the mount directory to already exist and be empty; Windows
requires it not to exist at all, because WinFsp creates it and deletes it with
the filesystem and refuses with 'mount point in use' if it is already there.
That inversion is the whole reason this behaviour lives in runner_windows.go
and runner_linux.go rather than in the portable core.

mounted with attr_timeout and entry_timeout positive and negative_timeout 0:
the positive pair is the bound on how long the kernel may serve a stale answer
if the entire notification path fails silently, and a cached negative lookup
would hide a file another client just created.

Run flushes on SIGINT and on context cancellation, because neither is a crash
and both are chances to save the user's last edit."
```

---

## Task 14: `peerfold mount` 子命令与配置

**Files:**
- Create: `cmd/peerfold/cmd_mount.go`
- Modify: `cmd/peerfold/main.go`（分发里加 `mount`）
- Modify: `internal/config/config.go`
- Test: `cmd/peerfold/mount_test.go`

**Interfaces:**
- Consumes: Task 13（`Runner`、`DefaultMountPoint`）
- Produces:
  - `peerfold mount --server <addr> --mount <path> [--buffer 64MiB] [--buffer-total 256MiB]`
  - 配置项：`mount_point`、`buffer_limit`、`buffer_total`

- [ ] **Step 1: Write the failing test**

`cmd/peerfold/mount_test.go`：

```go
package main

import (
	"testing"
)

func TestMountIsDispatched(t *testing.T) {
	// main's dispatcher must know the word; it is the only entry point that
	// turns the layer into something a person can run.
	for _, name := range []string{"mount"} {
		if !dispatcherKnows(name) {
			t.Errorf("%q is not in the dispatcher", name)
		}
	}
}

func TestMountRequiresAServer(t *testing.T) {
	if err := runMount([]string{"--mount", t.TempDir()}); err == nil {
		t.Error("mount without --server should fail")
	}
}

func TestMountRejectsUnknownOption(t *testing.T) {
	if err := runMount([]string{"--nonsense"}); err == nil {
		t.Error("mount with an unknown option should fail")
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int{
		"1024":     1024,
		"64MiB":    64 << 20,
		"256MiB":   256 << 20,
		"2GiB":     2 << 30,
		"512KiB":   512 << 10,
		"64M":      64 << 20,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "64XiB", "MiB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should fail", bad)
		}
	}
}
```

> **实现者注意**：`dispatcherKnows` 是占位名。请按 `cmd/peerfold/main.go` 现有分发器的实际形状改写这个测试（它现在是一个 `switch`，直接断言那个 switch 的行为或者把分发词表抽成一个可测的变量都行）。**不要**为了让测试编译而把 `main` 改成不可读的形状。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/peerfold/ -run 'TestMount|TestParseSize' -v`
Expected: FAIL —— `undefined: runMount`

- [ ] **Step 3: Write `cmd/peerfold/cmd_mount.go`**

要点：

- 参数：`--server`（必填，可重复或接多个）、`--mount`（默认 `DefaultMountPoint()`）、`--buffer`、`--buffer-total`、`--allow-write`（默认**开**，因为设计的成功标准是"用编辑器改完保存"）
- 未给 `--mount` 时打印将要使用的路径，让用户知道它落在哪
- 校验 `--mount` 是绝对路径
- 用 `viewer.New(...)` 建观察者，包成 `NewViewerCaller`，构造 `Reconnector` 在断线时重建
- 调 `Runner.Run(ctx)`
- SIGINT 由 `Runner` 处理，这里只需 `context.Background()`

- [ ] **Step 4: Add the config keys**

在 `internal/config/config.go` 加 `mount_point`、`buffer_limit`、`buffer_total`，与现有键同样的解析与默认值风格。跑 `go test ./internal/config/ -v` 确认原有的 6 个测试仍然全绿。

- [ ] **Step 5: Wire the dispatcher**

在 `cmd/peerfold/main.go` 的分发里加 `mount`。

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./cmd/peerfold/ -v 2>&1 | tail -20`
Expected: PASS，且原有的 50 个测试仍全绿

- [ ] **Step 7: Run the whole tree**

Run: `go test ./... 2>&1 | tail -15`
Expected: 九个包全 `ok`

- [ ] **Step 8: Commit**

```bash
git add cmd/peerfold/ internal/config/config.go
git commit -m "feat(cli): peerfold mount

The dispatcher gains the word that turns the whole layer into something a
person can run. --mount defaults to the platform's mount point and prints where
that is, because a mount nobody can find is not usable.

--allow-write defaults to on: the spec's success criterion is editing a file in
an editor and saving it, which is a write."
```

---

## Task 15: 真实挂载的端到端测试（本机能跑，这是唯一算数的验证）

**这是唯一一个证明"它真的能用"的任务。** 前面 14 个任务全是单元测试，它们证明操作核的逻辑对，但不证明内核会接受这个文件系统、不证明 `Read` 的返回值语义对（探针里我就是在这里错的）、不证明编辑器能保存。

**Files:**
- Create: `internal/mount/e2e_windows_test.go`
- Create: `internal/mount/e2e_linux_test.go`（同样的测试，但因缺 `/dev/fuse` 在本机跑不了）

**关键约束（来自实测）**：
- **`Mount()` 会阻塞**，所以挂载必须在**另一个进程**里跑，或者在测试里同一个进程的另一 goroutine 里跑但**绝不对挂载点做文件 IO**（那样会死锁）。**本计划选另一个进程** —— 它同时验证了真实的分工
- **测试结束必须真的卸载**，否则残留的挂载点会让下一次测试失败
- **不要预先创建挂载目录**（Windows）

**Interfaces:**
- Consumes: everything above
- Produces: `TestE2ERealMount` 及一组子测试

- [ ] **Step 1: Write the e2e test**

结构：测试启动一个**服务端 + 客户端**（复用 `internal/e2e` 的连接方式），起一个**挂载进程**（`go test` 编译出的小 helper，用 `os.Args` 触发），然后**从测试进程**对挂载点做真实文件操作。

```go
//go:build windows

package mount

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestE2ERealMount mounts a real filesystem via WinFsp and touches it from this
// process. It is the only test that proves the kernel accepts this filesystem
// and that an ordinary program can use it.
//
// It must not be run with `-run` filters that skip the setup, and it must
// always unmount: a leftover mount point makes the next run fail with
// "mount point in use".
func TestE2ERealMount(t *testing.T) {
	if os.Getenv("PEERFOLD_E2E_MOUNT") == "" {
		t.Skip("set PEERFOLD_E2E_MOUNT=1 to run the real-mount test")
	}
	// ... set up a server and a client, get the client's address ...

	mp := filepath.Join(t.TempDir(), "peerfold")
	// The mount process must be separate: Mount blocks on the FUSE main loop,
	// and touching the mount point from the mounter deadlocks.
	cmd := exec.Command(os.Args[0], "-test.run=TestMountHelper", "--", mp, addr)
	cmd.Env = append(os.Environ(), "PEERFOLD_MOUNT_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the mount helper: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// Wait for the mount to appear. WinFsp creates the directory itself, so
	// there is nothing to pre-create and nothing to poll for except the mount
	// becoming usable.
	waitFor(t, 20*time.Second, func() bool {
		_, err := os.Stat(mp)
		return err == nil
	}, "the mount point never appeared")

	t.Run("list", func(t *testing.T) {
		entries, err := os.ReadDir(mp)
		if err != nil {
			t.Fatalf("reading the mounted directory: %v", err)
		}
		_ = entries
	})

	t.Run("read", func(t *testing.T) {
		body, err := os.ReadFile(filepath.Join(mp, "hello.txt"))
		if err != nil {
			t.Fatalf("reading a file through the mount: %v", err)
		}
		// The exact content comes from the fixture; assert it is non-empty at
		// minimum, because a zero-length success here is the failure the probe
		// hit ("Incorrect function" / an empty read).
		if len(body) == 0 {
			t.Error("read returned no bytes through a mount that claims the file is non-empty")
		}
	})

	t.Run("write-and-read-back", func(t *testing.T) {
		p := filepath.Join(mp, "written.txt")
		if err := os.WriteFile(p, []byte("written through the mount"), 0o644); err != nil {
			t.Fatalf("writing through the mount: %v", err)
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading back: %v", err)
		}
		if string(got) != "written through the mount" {
			t.Errorf("read back %q, want what was written", got)
		}
	})

	t.Run("fsync-then-verify-owner", func(t *testing.T) {
		p := filepath.Join(mp, "synced.txt")
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := f.WriteString("durable"); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("fsync: %v", err)
		}
		// The owner's copy must already have the content: fsync returning
		// success without writing is exactly "saving quietly loses data".
		// Read it directly from the share, bypassing the mount.
		owner := filepath.Join(shareDir, "synced.txt")
		body, err := os.ReadFile(owner)
		if err != nil {
			t.Fatalf("reading the owner's copy: %v", err)
		}
		if string(body) != "durable" {
			t.Errorf("the owner has %q after fsync returned, want 'durable'", body)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})

	t.Run("mkdir-rename-unlink", func(t *testing.T) {
		d := filepath.Join(mp, "sub")
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Rename(filepath.Join(d, "a.txt"), filepath.Join(d, "b.txt")); err != nil {
			// a.txt may not exist; create it first
			_ = os.WriteFile(filepath.Join(d, "a.txt"), []byte("x"), 0o644)
			if err := os.Rename(filepath.Join(d, "a.txt"), filepath.Join(d, "b.txt")); err != nil {
				t.Fatalf("rename: %v", err)
			}
		}
		if err := os.Remove(filepath.Join(d, "b.txt")); err != nil {
			t.Fatalf("unlink: %v", err)
		}
		if err := os.Remove(d); err != nil {
			t.Fatalf("rmdir: %v", err)
		}
	})

	t.Run("editor-save-pattern", func(t *testing.T) {
		// The exact pattern the spec's success criterion describes: write a
		// temporary file, fsync, rename over the target. Task 9's rename
		// flush is what makes this work without an explicit close of the
		// temporary file first.
		target := filepath.Join(mp, "doc.txt")
		if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		tmp := filepath.Join(mp, ".doc.txt.swp")
		f, err := os.Create(tmp)
		if err != nil {
			t.Fatalf("create temp: %v", err)
		}
		if _, err := f.WriteString("v2"); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("sync temp: %v", err)
		}
		if err := os.Rename(tmp, target); err != nil {
			t.Fatalf("rename temp over target: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		if string(body) != "v2" {
			t.Errorf("target is %q after the editor pattern, want v2", body)
		}
	})

	t.Run("directory-listing-matches-owner", func(t *testing.T) {
		throughMount, err := os.ReadDir(mp)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		direct, err := os.ReadDir(shareDir)
		if err != nil {
			t.Fatalf("reading the share directly: %v", err)
		}
		if len(throughMount) != len(direct) {
			t.Errorf("the mount lists %d entries, the share has %d", len(throughMount), len(direct))
		}
	})
}

// TestMountHelper is the mount process. It is selected by an environment
// variable and does nothing under a normal test run.
func TestMountHelper(t *testing.T) {
	if os.Getenv("PEERFOLD_MOUNT_HELPER") == "" {
		t.Skip("not the mount helper")
	}
	// ... construct the caller, then run the mount and block ...
}
```

- [ ] **Step 2: Run it**

Run: `$env:PEERFOLD_E2E_MOUNT='1'; go test ./internal/mount/ -run TestE2ERealMount -v -timeout 300s 2>&1 | tail -40`
Expected: **这是关键时刻。** 可能大面积失败 —— 探针里 `Read` 就没通。如实记录每一条失败，**按 `systematic-debugging` 处理**，不要为了让测试变绿而放宽断言。

- [ ] **Step 3: Fix what fails, one root cause at a time**

按失败顺序逐个修。已知的两条最可能的坑：

1. **`Read` 的返回值语义**：cgofuse 的 `Read` 要返回**读到的字节数**（不是 0），`Readdir` 要用 `fill` 并返回 0。探针里我在 `Readdir` 上搞错了，症状是 `dir /b` 空、`.NET ReadAllText` 报 `Incorrect function`
2. **`Getattr` 的 `Mtim`**：有些程序（资源管理器）对 `Mtim` 为 0 反应很差；确认 `fuse.NewTimespec` 的用法

- [ ] **Step 4: Add the "nothing is pre-created" regression test**

```go
// The mistake that cost an afternoon: pre-creating the mount directory makes
// WinFsp refuse with "mount point in use". This pins the behaviour.
func TestWindowsMountPointMustNotPreexist(t *testing.T) {
	dir := t.TempDir()
	mp := filepath.Join(dir, "mp")
	if err := os.Mkdir(mp, 0o755); err != nil {
		t.Fatal(err)
	}
	// An empty existing directory is cleaned up so WinFsp can create its own.
	cleanup, err := prepareMountPoint(mp)
	if err != nil {
		t.Fatalf("prepareMountPoint on an empty dir: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if _, err := os.Stat(mp); !os.IsNotExist(err) {
		t.Error("an empty pre-existing mount point must be removed, not left for WinFsp to trip over")
	}

	// A non-empty one is refused rather than deleted.
	if err := os.Mkdir(mp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, "keep.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareMountPoint(mp); err == nil {
		t.Error("a non-empty mount point must be refused, not silently emptied")
	}
	if _, err := os.Stat(filepath.Join(mp, "keep.txt")); err != nil {
		t.Error("prepareMountPoint deleted a file inside a non-empty mount point")
	}
}
```

- [ ] **Step 5: Run the whole tree plus the e2e test**

Run: `$env:PEERFOLD_E2E_MOUNT='1'; go test ./... -timeout 600s 2>&1 | tail -20`
Expected: 九个包全 `ok`，`internal/mount` 包含真实挂载测试

- [ ] **Step 6: Verify no mount is left behind**

Run: `Get-Process | Where-Object { $_.ProcessName -match 'peerfold' }; & 'C:\Program Files (x86)\WinFsp\bin\launchctl-x64.exe' list`
Expected: 没有残留进程，`launchctl list` 只有 `OK`

- [ ] **Step 7: Commit**

```bash
git add internal/mount/e2e_windows_test.go internal/mount/e2e_linux_test.go
git commit -m "test(mount): real end-to-end mount, the only test that proves it works

Everything before this proves the operation core's logic; none of it proves the
kernel accepts the filesystem, that Read's return convention is right, or that
an ordinary program can save a file. The probe's read path returned the wrong
thing and only showed up under a real mount.

The mount runs in a separate process because Mount blocks on the FUSE main
loop and touching the mount point from the mounter deadlocks. The editor-save
pattern is exercised exactly as the spec describes it: write a temporary file,
fsync, rename over the target."
```

---

## 交付标准

全部 15 个任务完成后，逐条核对：

- [ ] `go build ./...` exit 0（`CGO_ENABLED=0 GOOS=windows`）
- [ ] `go vet ./...` exit 0
- [ ] `gofmt -l .` 为空
- [ ] `go test ./...` 九个包全 `ok`，且 v1 的 276 个测试一个不少
- [ ] `go test ./internal/mount/ -run TestE2ERealMount` 通过（真实挂载，Windows）
- [ ] `peerfold mount --server <addr>` 之后 `dir %USERPROFILE%\peerfold` 能列出远端共享的内容
- [ ] 用记事本打开挂载点里的文件、改、保存，远端文件内容变了
- [ ] 杀掉客户端后挂载点消失，且**没有残留挂载**
- [ ] **如实写明**：Linux 的 cgofuse 路径需要 cgo + libfuse，本机无法验证，标注"待外部验证"

## 一处必须诚实交代的边界

本机**没有** `/dev/fuse`、**没有** C 工具链、**没有** libfuse 头文件，所以：

- Windows 侧：**已实测**（目录挂载、元数据、WinFsp 自动创建与清理）
- Linux 侧：**只能交叉编译**，集成测试**跑不了**

所以本计划的验收措辞必须是 **"Windows 已验证 / Linux 待外部验证"**，不能写成"两平台都验证了"。`runner_linux.go` 的代码照写、测试照写，但它们在本机**没有被执行过** —— 这个事实要写在交付说明里，不能靠读者自己推断。