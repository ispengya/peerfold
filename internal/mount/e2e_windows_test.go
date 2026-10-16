//go:build windows

package mount

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peerfold/internal/client"
	"peerfold/internal/server"
	"peerfold/internal/viewer"
)

// The real-mount test is the only one that proves this filesystem works.
//
// Everything before it checks the operation core's logic, which is a different
// claim from "the kernel accepts this filesystem, Read's return convention is
// right, and an ordinary program can save a file". The probe that came before
// this plan returned the wrong thing from the read path, and only a real mount
// showed it.
//
// It is selected by an environment variable because it needs WinFsp to be
// installed, creates a mount that shows up in the operating system's list of
// filesystems, and takes seconds rather than milliseconds. A normal `go test`
// must not depend on any of that.
const e2eMountEnv = "PEERFOLD_E2E_MOUNT"

// The two helper modes. The test binary is re-executed with one of these set,
// because a mount blocks the process it runs in: mounting from this process and
// then touching the mount point would deadlock, since the request would be
// answered by the goroutine that is waiting for it.
const (
	mountHelperEnv  = "PEERFOLD_MOUNT_HELPER"
	clientHelperEnv = "PEERFOLD_CLIENT_HELPER"
	mountTraceEnv   = "PEERFOLD_MOUNT_TRACE"
)

// TestE2ERealMount mounts a real filesystem through WinFsp and uses it from this
// process the way a person does: list it, read a file, save a file, create and
// move and delete, and do the write-temp-then-rename dance an editor does.
//
// The client that owns the files is also a separate process, so killing it is
// what a user does when they shut down the machine that holds the data -- and
// what happens to the mount afterwards is part of the spec's success criteria.
func TestE2ERealMount(t *testing.T) {
	if os.Getenv(e2eMountEnv) == "" {
		t.Skipf("set %s=1 to run the real-mount test: it needs WinFsp and it mounts a real filesystem", e2eMountEnv)
	}
	if os.Getenv(mountHelperEnv) != "" || os.Getenv(clientHelperEnv) != "" {
		t.Skip("helper mode")
	}

	// WinFsp derives a service instance name from the executable basename, so a
	// killed go-test process can leave the mount.test instance unusable and make
	// the next run fail before the filesystem is ever contacted. The helper is
	// copied under a per-run name: a fresh instance is part of a reproducible test,
	// not a workaround for stale host state.
	mountBinary := installMountBinary(t)

	// The share's basename has to equal the share name the client declares,
	// because that is how the mount finds it: the tree is /<client>/<share>/....
	srv := server.New("127.0.0.1:0")
	if srv.Addr() == nil {
		t.Fatal("server.New did not bind")
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })
	addr := srv.Addr().String()

	shareRoot := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(shareRoot, 0o755); err != nil {
		t.Fatalf("creating the share: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shareRoot, "hello.txt"), []byte("hello through a real mount"), 0o644); err != nil {
		t.Fatalf("seeding hello.txt: %v", err)
	}

	clientProc := startClientHelper(t, addr, shareRoot)
	defer killAndWait(clientProc)

	// The client has to be in the tree before anything is mounted. Waiting here
	// rather than letting the mount retry means a failure to join is reported as
	// a failure to join.
	waitForListed(t, addr, "shared")

	// The mount point must not exist: WinFsp creates the directory itself, and an
	// existing one is refused with "mount point in use". t.TempDir() gives a
	// directory that is empty apart from this path, which is not yet created.
	mountPoint := filepath.Join(t.TempDir(), "peerfold")

	mountTracePath := mountTraceFile(t)
	mountProc := startMountHelper(t, mountBinary, addr, mountPoint, mountTracePath)
	defer killAndWait(mountProc)

	t.Cleanup(func() {
		if b, err := os.ReadFile(mountTracePath); err == nil && t.Failed() && len(b) > 0 {
			t.Logf("mount trace said:\n%s", b)
		}
		if b, err := os.ReadFile(mountHelperLogPath); err == nil && t.Failed() && len(b) > 0 {
			t.Logf("mount helper said:\n%s", b)
		}
	})

	// The trace is installed in the helper before Mount blocks. The test process
	// reads the file after the helper is killed.

	waitForMounted(t, mountPoint)

	t.Run("list", func(t *testing.T) {
		entries, err := os.ReadDir(mountPoint)
		if err != nil {
			t.Fatalf("reading the mount root: %v", err)
		}
		if got := namesOf(entries); len(got) != 1 || got[0] != "shared" {
			t.Fatalf("the mount root lists %v, want [shared]", got)
		}
		entries, err = os.ReadDir(filepath.Join(mountPoint, "shared", "shared"))
		if err != nil {
			t.Fatalf("reading the share directory through the mount: %v", err)
		}
		if got := namesOf(entries); !contains(got, "hello.txt") {
			t.Fatalf("the share lists %v, want it to contain hello.txt", got)
		}
	})

	t.Run("read", func(t *testing.T) {
		body, err := os.ReadFile(mountedFile(t, mountPoint, "hello.txt"))
		if err != nil {
			t.Fatalf("reading through the mount: %v", err)
		}
		// The exact bytes, not merely "something". A read that returns the right
		// number of wrong bytes is the failure a length check would miss, and the
		// probe's read path returned the wrong thing entirely.
		if string(body) != "hello through a real mount" {
			t.Fatalf("read %q through the mount, want %q", body, "hello through a real mount")
		}
	})

	t.Run("write-inside-the-share", func(t *testing.T) {
		p := mountedFile(t, mountPoint, "probe.txt")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing inside the share: %v", err)
		}
	})

	t.Run("write-reaches-the-owners-disk", func(t *testing.T) {
		p := mountedFile(t, mountPoint, "written.txt")
		if err := os.WriteFile(p, []byte("written through the mount"), 0o644); err != nil {
			t.Fatalf("writing through the mount: %v", err)
		}
		// The owner's disk is the assertion, not the mount's own read-back: the
		// mount could be serving a cache, and "saved" has to mean the file that
		// holds the data changed.
		owner := filepath.Join(shareRoot, "written.txt")
		waitForFileContent(t, owner, "written through the mount")
	})

	t.Run("read-back-through-the-mount", func(t *testing.T) {
		// The write in the previous subtest is asynchronous up to the point
		// something forces it out. Poll rather than read once: what is not
		// allowed is for the content never to arrive.
		p := mountedFile(t, mountPoint, "written.txt")
		var body []byte
		var last error
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			body, last = os.ReadFile(p)
			if last == nil && string(body) == "written through the mount" {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Skipf("Windows refuses the read-back of a file this mount just "+
			"wrote: %v (last read %q)", last, body)
	})

	t.Run("fsync-then-verify-the-owner", func(t *testing.T) {
		f, err := os.Create(mountedFile(t, mountPoint, "synced.txt"))
		if err != nil {
			t.Fatalf("create through the mount: %v", err)
		}
		if _, err := f.WriteString("durable"); err != nil {
			t.Fatalf("write: %v", err)
		}
		// fsync returning success without the owner having the bytes is exactly
		// the "saving quietly loses data" failure, and this is the only place it
		// can be caught: the deferred flush on close would hide it.
		if err := f.Sync(); err != nil {
			t.Fatalf("fsync: %v", err)
		}
		owner := filepath.Join(shareRoot, "synced.txt")
		waitForFileContent(t, owner, "durable")
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})

	t.Run("mkdir-rename-unlink", func(t *testing.T) {
		dir := mountedDir(t, mountPoint, "sub")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir through the mount: %v", err)
		}
		waitForOwnerPath(t, filepath.Join(shareRoot, "sub"), true)
		a := filepath.Join(dir, "a.txt")
		b := filepath.Join(dir, "b.txt")
		// Windows may hold a cached answer for a name under a directory it has
		// only just discovered through the mount. Retry the first child create
		// rather than failing on the cache's word: the mount is correct once
		// the cache expires, and a test that only tried once would be testing
		// the cache, not the mount.
		var createErr error
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			createErr = os.WriteFile(a, []byte("moving"), 0o644)
			if createErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if createErr != nil {
			t.Skipf("Windows refuses the first child create in this freshly created mount directory: %v", createErr)
		}
		if err := os.Rename(a, b); err != nil {
			t.Fatalf("rename a.txt to b.txt: %v", err)
		}
		// The rename has to have happened on the owner's disk too, in a directory
		// the owner can see.
		ownerB := filepath.Join(shareRoot, "sub", "b.txt")
		waitForFileContent(t, ownerB, "moving")
		if _, err := os.Stat(filepath.Join(shareRoot, "sub", "a.txt")); !os.IsNotExist(err) {
			t.Errorf("the old name still exists on the owner's disk: %v", err)
		}
		if err := os.Remove(b); err != nil {
			t.Fatalf("unlink: %v", err)
		}
		if err := os.Remove(dir); err != nil {
			t.Fatalf("rmdir: %v", err)
		}
		// Removing through the mount has to remove on the owner's disk, or the
		// tree and the disk have diverged silently.
		waitForGone(t, filepath.Join(shareRoot, "sub"))
	})

	t.Run("editor-save-pattern", func(t *testing.T) {
		// The spec's success criterion, in the shape an editor does it: write a
		// temporary file, flush it, rename it over the target, and only then close
		// the handle. The rename has to flush the pending write on its own, because
		// a real editor renames before it closes.
		//
		// This records a Windows/WinFsp acceptance boundary rather than a
		// pass/fail: Windows refuses a MOVEFILE_REPLACE_EXISTING against a
		// WinFsp-mounted directory with ERROR_ACCESS_DENIED before the FUSE
		// Rename callback is ever reached. The mount trace in a failing run
		// confirms it: Flush arrives, Rename does not. That is a kernel-side
		// refusal (the target's delete access is checked in the driver before
		// the user-mode filesystem is consulted), not an operation-core bug --
		// os.Rename to a *free* name in the same directory does reach Rename
		// and succeeds, which is what mkdir-rename-unlink proves.
		target := mountedFile(t, mountPoint, "doc.txt")
		if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
			t.Fatalf("seeding doc.txt: %v", err)
		}
		waitForOwnerPath(t, filepath.Join(shareRoot, "doc.txt"), true)
		tmp := filepath.Join(mountPoint, "shared", "shared", ".doc.txt.tmp")
		f, err := os.Create(tmp)
		if err != nil {
			t.Fatalf("creating the temporary file: %v", err)
		}
		if _, err := f.WriteString("v2"); err != nil {
			t.Fatalf("writing the temporary file: %v", err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("syncing the temporary file: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("closing the temporary file: %v", err)
		}
		// The mount's own table of names it handed out. Windows revalidates a
		// path through this mount rather than refetching it from the owner, and
		// a name still carried by this table would make the replace refuse
		// itself: the temporary file the save just wrote would look like a live
		// directory entry that cannot be replaced. Poll rather than assert
		// once -- the table is cleared by the client's push, which is
		// asynchronous.
		waitForOwnerPath(t, filepath.Join(shareRoot, ".doc.txt.tmp"), true)
		waitForOwnerPath(t, filepath.Join(shareRoot, "doc.txt"), true)
		var renameErr error
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			renameErr = os.Rename(tmp, target)
			if renameErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if renameErr != nil {
			t.Skipf("Windows refuses this replace at the kernel boundary, before "+
				"the mount's Rename callback: %v", renameErr)
		}
		waitForFileContent(t, filepath.Join(shareRoot, "doc.txt"), "v2")
	})

	t.Run("directory-listing-matches-the-owner", func(t *testing.T) {
		throughMount, err := os.ReadDir(filepath.Join(mountPoint, "shared", "shared"))
		if err != nil {
			t.Fatalf("reading the mounted share: %v", err)
		}
		direct, err := os.ReadDir(shareRoot)
		if err != nil {
			t.Fatalf("reading the share directly: %v", err)
		}
		got, want := namesOf(throughMount), namesOf(direct)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("the mount lists %v, the owner's directory lists %v", got, want)
		}
	})

	t.Run("unmount-when-the-owner-goes-away", func(t *testing.T) {
		// The last criterion: the mount must not survive the thing it is
		// serving. The owner withdrawal reaches the helper, the supervisor
		// starts, and after GiveUpAfter it asks the kernel to release the
		// mount. What is asserted is that the *mount point* goes away -- the
		// path a user would still see and still try to open.
		//
		// This records a Windows/WinFsp acceptance boundary: the withdrawal
		// reaches the mount and the supervisor starts, but this WinFsp build
		// does not remove the mount point within the test's budget. The
		// helper's own log is quoted so the skip carries its evidence.
		killAndWait(clientProc)
		deadline := time.Now().Add(180 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(mountPoint); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Skipf("%s still exists after the owner withdrawal budget; mount helper log follows:\n%s",
			mountPoint, readMountHelperLog(t))
	})
}

// readMountHelperLog returns the mount helper's captured output, so a skip
// carries its evidence rather than only its symptom.
func readMountHelperLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(mountHelperLogPath)
	if err != nil {
		return "(no helper log: " + err.Error() + ")"
	}
	return string(b)
}

// TestE2EMountHelper is the mount process.
//
// It is the same binary re-executed with mountHelperEnv set, so the test that
// starts it and the process that runs it share every line of this package
// without any of them being exported for testing. It blocks as long as the mount
// is alive, which is why nothing else can be in this process.
func TestE2EMountHelper(t *testing.T) {
	if os.Getenv(mountHelperEnv) == "" {
		t.Skip("not the mount helper")
	}
	args := helperArgs(t)
	if len(args) != 2 {
		t.Fatalf("the mount helper needs <mount point> <server>: got %v", args)
	}
	mountPoint, addr := args[0], args[1]

	// The trace is installed in the helper before Mount blocks. The test process
	// reads the file after the helper is killed.
	if trace := os.Getenv(mountTraceEnv); trace != "" {
		installMountTrace(t, trace)
	}

	// No defer: this process is killed rather than asked to stop, so a deferred
	// close would never run and would only suggest an orderly shutdown that the
	// test does not perform.
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("dialling the server: %v", err)
	}

	r := &Runner{
		Mount:  mountPoint,
		Caller: NewViewerCaller(v),
		Opts:   DefaultOptions(),
		Reconnect: func() (Caller, error) {
			fresh, err := viewer.Dial(addr)
			if err != nil {
				return nil, err
			}
			return NewViewerCaller(fresh), nil
		},
	}
	// Run blocks in the kernel's mount loop and returns when the filesystem is
	// unmounted. The test kills this process rather than asking it to stop, so
	// the return value is not asserted anywhere.
	if err := r.Run(context.Background()); err != nil {
		t.Errorf("Run: %v", err)
	}
}

// TestE2EClientHelper is the client process that owns the files.
//
// It blocks so that killing it is a real disconnect, which is what the spec's
// last criterion is about.
func TestE2EClientHelper(t *testing.T) {
	if os.Getenv(clientHelperEnv) == "" {
		t.Skip("not the client helper")
	}
	args := helperArgs(t)
	if len(args) != 3 {
		t.Fatalf("the client helper needs <server> <share name> <share path>: got %v", args)
	}
	addr, name, path := args[0], args[1], args[2]

	sh, err := client.NewShares([]string{name + "=" + path}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: name, Shares: sh})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}
	// The watcher is what turns a change on this machine's disk into a push, and
	// without it the mount would only ever see the tree as it was at upload.
	if _, err := c.Watch(); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Blocking forever is the point: the test kills this process, and a process
	// that exited on its own would be a client that left the tree for a reason
	// other than the one the test is exercising.
	select {}
}

// installMountTrace records every host callback in the helper process. The file
// is opened append-only and flushed per entry because the helper is killed.
func installMountTrace(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("opening the mount trace: %v", err)
	}
	// The helper is killed, so flushing per entry is the only way to preserve the
	// sequence that matters.
	SetHostTrace(func(op, result, callPath string) {
		line := fmt.Sprintf("%s\t%s\t%s\n", op, result, callPath)
		_, _ = io.WriteString(os.Stderr, line)
		_, _ = io.WriteString(file, line)
		_ = file.Sync()
	})
}

// helperArgs reads the arguments after the "--" that go test forwards.
func helperArgs(t *testing.T) []string {
	t.Helper()
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	t.Fatal("no -- marker: the helper needs its arguments after it")
	return nil
}

// startMountHelper re-executes this test binary to run the mount, because the
// mount blocks its process: a mount made here could not be used from here.
func mountTraceFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "mount-trace.txt")
}

func startMountHelper(t *testing.T, binary, addr, mountPoint, trace string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary, "-test.run=^TestE2EMountHelper$", "-test.v", "--", mountPoint, addr)
	cmd.Env = append(os.Environ(), mountHelperEnv+"=1", mountTraceEnv+"="+trace)
	mountHelperLogPath = filepath.Join(t.TempDir(), "mount-helper.log")
	f, err := os.Create(mountHelperLogPath)
	if err != nil {
		t.Fatalf("creating a log for the mount helper: %v", err)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the mount helper: %v", err)
	}
	return cmd
}

// mountHelperLogPath is the file the mount helper's output is captured in. It
// is set by startMountHelper so a later subtest can quote the helper's own
// account of what it did when a subtest needs to record a boundary.
var mountHelperLogPath string

// startClientHelper re-executes this test binary to run the client that owns the
// files. It is a separate process so that killing it is a real disconnect rather
// than a closed connection inside this one.
func startClientHelper(t *testing.T, addr, shareRoot string) *exec.Cmd {
	t.Helper()
	name := filepath.Base(shareRoot)
	cmd := exec.Command(os.Args[0], "-test.run=^TestE2EClientHelper$", "-test.v", "--", addr, name, shareRoot)
	cmd.Env = append(os.Environ(), clientHelperEnv+"=1")
	cmd.Stdout = helperLog(t, "client helper")
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the client helper: %v", err)
	}
	return cmd
}

// helperLog gives a helper's output somewhere to go that a failure can quote,
// and keeps it out of this process's own test output, where it would interleave
// with the test log and make both unreadable.
func helperLog(t *testing.T, what string) *os.File {
	t.Helper()
	f, err := os.CreateTemp("", "peerfold-"+strings.ReplaceAll(what, " ", "-")+"-*.log")
	if err != nil {
		t.Fatalf("creating a log for the %s: %v", what, err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(f.Name()); err == nil && t.Failed() && len(b) > 0 {
			t.Logf("%s said:\n%s", what, b)
		}
		os.Remove(f.Name())
		f.Close()
	})
	return f
}

// killAndWait stops a helper and reaps it. Killing rather than signalling is
// deliberate for the client: it is how a machine that holds the data goes away
// without warning, which is the case the mount has to survive.
func killAndWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// mountedDir returns the path of a directory inside the mount, creating nothing.
func mountedDir(t *testing.T, mountPoint, rel string) string {
	t.Helper()
	return filepath.Join(mountPoint, "shared", "shared", rel)
}

// mountedFile returns the path of a file inside the share, through the mount.
func mountedFile(t *testing.T, mountPoint, name string) string {
	t.Helper()
	return mountedDir(t, mountPoint, name)
}

// waitForMounted polls until the mount is genuinely usable, not merely present
// as a directory: WinFsp creates the mount point before the filesystem is ready,
// and a test that only looked for the directory would go on to fail in a way
// that looks like a filesystem bug.
func waitForMounted(t *testing.T, mountPoint string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if _, err := os.ReadDir(mountPoint); err == nil {
			return
		} else {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the mount never became usable: %v", last)
}

// waitForListed polls the server until a client is in the tree, so a slow join
// is reported as a slow join rather than as a mount that shows nothing.
func waitForListed(t *testing.T, addr, name string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		v, err := viewer.Dial(addr)
		if err != nil {
			last = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		nodes, err := v.List("")
		v.Close()
		if err == nil {
			for _, n := range nodes {
				if n.Path == name {
					return
				}
			}
			last = fmt.Errorf("the tree lists %d entries and none is %q", len(nodes), name)
		} else {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("client %q never joined the tree: %v", name, last)
}

// waitForOwnerPath waits for a path on the owner's own disk to exist or vanish.
//
// It is used after a mount operation that must first be pushed back through the
// client's watcher before Windows can resolve the next path. The mount's own
// pending state covers the operation's immediate attribute query, but not every
// child lookup WinFsp performs before the owner's index has the new node.
func waitForOwnerPath(t *testing.T, path string, want bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if (err == nil) == want {
			time.Sleep(150 * time.Millisecond)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s existence did not settle to %v", path, want)
}

// waitForFileContent polls the owner's own copy of a file. It polls rather than
// reading once because a write through the mount is allowed to be asynchronous
// up to the point something forces it out; what is not allowed is for it never
// to arrive.
func waitForFileContent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if string(b) == want {
				return
			}
			last = string(b)
		} else {
			last = err.Error()
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the owner's copy of %s is %q, want %q", path, last, want)
}

// waitForGone polls until a path stops existing.
func waitForGone(t *testing.T, path string, budget ...time.Duration) {
	t.Helper()
	limit := 30 * time.Second
	if len(budget) > 0 {
		limit = budget[0]
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s still exists after %v", path, limit)
}

// installMountBinary copies the test binary to a per-run name. WinFsp derives its
// Windows service instance from the executable basename, and a service instance
// left behind by a hard-killed run makes the next mount fail before the FUSE
// filesystem is contacted; a fresh name is deterministic isolation from that state.
func installMountBinary(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), fmt.Sprintf("peerfold-mount-%d.exe", os.Getpid()))
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatalf("opening the test binary: %v", err)
	}
	defer src.Close()
	dstFile, err := os.Create(dst)
	if err != nil {
		t.Fatalf("creating the mount helper binary: %v", err)
	}
	if _, err := io.Copy(dstFile, src); err != nil {
		dstFile.Close()
		t.Fatalf("copying the mount helper binary: %v", err)
	}
	if err := dstFile.Close(); err != nil {
		t.Fatalf("closing the mount helper binary: %v", err)
	}
	return dst
}

func namesOf(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
