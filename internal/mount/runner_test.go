package mount

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDefaultMountPointIsAbsoluteAndNamedPeerfold pins the one thing every
// platform agrees on: the default is an absolute directory called peerfold.
// Windows prefers D:\peerfold when that drive exists and falls back to the
// user profile otherwise; Linux keeps it under the user's home.
func TestDefaultMountPointIsAbsoluteAndNamedPeerfold(t *testing.T) {
	p, err := DefaultMountPoint()
	if err != nil {
		t.Fatalf("DefaultMountPoint: %v", err)
	}
	if !strings.HasSuffix(p, "peerfold") {
		t.Errorf("DefaultMountPoint = %q, want it to end in peerfold", p)
	}
	// It must be absolute. A relative mount point depends on the process's
	// working directory, which is not a thing any operator expects a mount to do.
	if !filepath.IsAbs(p) {
		t.Errorf("DefaultMountPoint = %q, want an absolute path", p)
	}
}

// The timeouts are the bound on how wrong the tree can be if the whole
// notification path fails silently, so they have to be passed rather than
// defaulted to the library's.
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
	// zero, not a positive number: a cached negative lookup would hide a file
	// that another client has just created.
	if !strings.Contains(joined, "negative_timeout=0") {
		t.Errorf("args %v do not set negative_timeout=0", args)
	}
}

// TestMountArgsAlwaysCarryNegativeTimeout guards the one value that is a
// constant rather than an option. It is the difference between a peer's new file
// appearing immediately and it being invisible for as long as the entry timeout.
func TestMountArgsAlwaysCarryNegativeTimeout(t *testing.T) {
	for _, opts := range []Options{DefaultOptions(), {}, {AttrTimeout: 1}} {
		joined := strings.Join(mountArgs(opts), " ")
		if !strings.Contains(joined, "negative_timeout=0") {
			t.Errorf("args %v do not set negative_timeout=0", mountArgs(opts))
		}
	}
}

// TestPrepareMountPointLeavesNothingWhenItSucceeds is the property both
// platforms share: whatever the per-platform rule about the directory is,
// prepareMountPoint either returns an error or leaves a state the mount can use.
//
// The two platforms reach it differently -- Windows needs the path to be absent
// when Mount is called, Linux needs it to exist and be empty -- so this asserts
// the shared half and leaves the divergence to the platform-specific tests below,
// which only run on the platform they describe.
func TestPrepareMountPointLeavesNothingWhenItSucceeds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mp")
	cleanup, err := prepareMountPoint(p)
	if err != nil {
		t.Fatalf("prepareMountPoint: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
}

// TestPrepareMountPointRefusesANonEmptyDirectory is the safety property, and it
// is the same on both platforms for different reasons: on Linux, mounting over a
// non-empty directory hides its contents without warning; on Windows, a
// non-empty directory is either a stale mount or somebody's data, and neither is
// something to delete automatically.
func TestPrepareMountPointRefusesANonEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mp")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "keepme"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanup, err := prepareMountPoint(p)
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("prepareMountPoint accepted a non-empty directory; mounting there " +
			"would hide its contents, and on Windows it would delete somebody's data")
	}

	// The refusal has to name the reason. A preparation step that goes on to try
	// to clear the directory fails too -- it is not empty, so the remove fails and
	// it returns *an* error -- and a test that only asked for "some error" would
	// pass on that. What is being pinned here is that the non-empty case is
	// recognised rather than stumbled into.
	// "exists and is not empty" is the refusal's own wording. Asking only for
	// "not empty" is not enough: Windows puts that phrase in the error it returns
	// when the remove of a non-empty directory fails, and a preparation step that
	// skipped this check would stumble into that failure and satisfy the looser
	// assertion while doing the wrong thing.
	if !strings.Contains(err.Error(), "exists and is not empty") {
		t.Errorf("prepareMountPoint refused a non-empty directory with %q; it "+
			"should have said the directory is not empty", err)
	}
	// The file must still be there. A refusing call that deleted the directory on
	// its way out would be worse than one that accepted it.
	if _, statErr := os.Stat(filepath.Join(p, "keepme")); statErr != nil {
		t.Errorf("prepareMountPoint removed the contents of a directory it refused: %v", statErr)
	}
}

// TestRunnerRunRefusesAnUnusableMountPoint checks that Run reports the mount
// point problem rather than failing later with something less specific.
func TestRunnerRunRefusesAnUnusableMountPoint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mp")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := newFakeCaller()
	f.addDir("/")
	r := &Runner{Mount: p, Caller: f, Opts: DefaultOptions()}
	if err := r.Run(t.Context()); err == nil {
		t.Error("Run accepted a mount point it cannot use")
	}
}

// TestRunRefusesANilCaller keeps the failure at the call rather than at the first
// operation, where a nil Caller would panic somewhere inside the mount with no
// mention of how it got there.
func TestRunRefusesANilCaller(t *testing.T) {
	r := &Runner{Mount: filepath.Join(t.TempDir(), "mp"), Opts: DefaultOptions()}
	if err := r.Run(context.Background()); err == nil {
		t.Error("Run accepted a nil caller")
	}
}

// TestRunRefusesAnEmptyMountPoint is the same argument for the mount point. An
// empty string reaches the platform rule, where on Linux it becomes MkdirAll("")
// and fails with a message about a path instead of about the missing argument.
func TestRunRefusesAnEmptyMountPoint(t *testing.T) {
	f := newFakeCaller()
	f.addDir("/")
	r := &Runner{Caller: f, Opts: DefaultOptions()}
	if err := r.Run(context.Background()); err == nil {
		t.Error("Run accepted an empty mount point")
	}
}

// TestRunRefusesANonEmptyMountPoint exercises the preparation step through Run
// rather than through prepareMountPoint, which is the only way to see that Run
// actually calls it. A Run that skipped preparation would go straight to
// host.Mount and fail there instead, or -- worse, and this is what happened --
// succeed in a way that hides the directory's contents.
func TestRunRefusesANonEmptyMountPoint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mp")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "keepme"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := newFakeCaller()
	f.addDir("/")
	r := &Runner{Mount: p, Caller: f, Opts: DefaultOptions()}
	if err := r.Run(context.Background()); err == nil {
		t.Error("Run accepted a non-empty mount point")
	}
	if _, statErr := os.Stat(filepath.Join(p, "keepme")); statErr != nil {
		t.Errorf("Run removed the contents of a directory it should have refused: %v", statErr)
	}
}

// TestShutdownSaveWritesTheBufferedEdit is the Ctrl-C path.
//
// The save is the only thing between an operator interrupting a mount and losing
// their last edit, so it is tested directly rather than through Run: Run spends
// its time blocked inside the kernel's mount loop, and every way of reaching the
// save from outside that would need a real mount in a second process, which is
// the end-to-end test's job.
func TestShutdownSaveWritesTheBufferedEdit(t *testing.T) {
	f := newFakeCaller()
	f.addDir("/")
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", []byte("old"))

	m := New(f, DefaultOptions())
	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	// Write returns a byte count: a negative value is an errno.
	if n := m.Write("/a.txt", []byte("new"), 0, fh); n < 0 {
		t.Fatalf("Write = %d, want a byte count", n)
	}
	if got := string(f.data["/a.txt"]); got != "old" {
		t.Fatalf("the edit reached the owner before the save; %q", got)
	}

	m.shutdownSave()

	if got := string(f.data["/a.txt"]); got != "new" {
		t.Errorf("shutdownSave did not write the buffered edit: bytes are %q, want %q. "+
			"An interrupted mount is meant to save what was typed, not to lose it.",
			got, "new")
	}
}

// TestRunAlwaysRejectsAMountPointItDidNotPrepare is the guard against Run
// ignoring the preparation step entirely.
//
// The path is a regular file, which no platform's mount can use, so the only two
// possible outcomes are "Run refused, and said why" and "Run went on to the
// kernel". The second is what a skipped preparation produces, and it is a hang
// rather than an error -- WinFsp is handed a path it cannot use and the mount
// loop never returns. So the check is bounded: a Run still inside the mount loop
// after five seconds failed this test, and the goroutine is left to the kernel
// because there is no way to unblock it from here.
func TestRunAlwaysRejectsAMountPointItDidNotPrepare(t *testing.T) {
	p := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(p, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeCaller()
	f.addDir("/")
	r := &Runner{Mount: p, Caller: f, Opts: DefaultOptions()}

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Run accepted a regular file as a mount point")
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not refuse a regular file as a mount point and did not " +
			"return: it went past the preparation step and into the mount loop. " +
			"WinFsp refuses a path it cannot use, so a skipped prepareMountPoint " +
			"shows up as a mount that never comes up rather than as an error.")
	}
}
