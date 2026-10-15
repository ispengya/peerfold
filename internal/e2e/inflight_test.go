package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestE2EChangesSurviveAnAnswerInFlight pins that a change landing in the middle
// of a forwarded answer is still applied.
//
// A client's change event is its own frame, and an answer is several frames: a
// head, a content stream, a closing frame. The server reads that answer on the
// only goroutine allowed to read the owner's connection — which is also the
// goroutine that would normally receive the event. Reading the answer with a
// body-read that has nowhere to put an event throws it away, and a thrown-away
// event is permanent: nothing rescans, so the index disagrees with the
// supplier's disk until that client reconnects. This is the design's named
// "the tree lies" failure.
//
// A large read is what makes the window wide enough to hit reliably: the answer
// occupies the connection for long enough that changes on disk have somewhere to
// land other than between two requests.
func TestE2EChangesSurviveAnAnswerInFlight(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{})

	// A large file so the read stream is long enough to interleave with.
	big := make([]byte, 12<<20)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	watchClient(t, join(t, addr, "win", "Documents="+root))
	v := dialViewer(t, addr)

	waitFor(t, "the big file to be indexed", func() bool {
		return hasPath(t, v, "win/Documents", "Documents/big.bin")
	})

	// Start a big read, then create files underneath it while it streams.
	readDone := make(chan error, 1)
	go func() {
		_, err := v.Cat("win/Documents/big.bin")
		readDone <- err
	}()

	const n = 30
	created := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("during-%02d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
		created = append(created, "Documents/"+name)
		time.Sleep(3 * time.Millisecond)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("Cat: %v", err)
	}

	// Every created file must reach the index, and stay there.
	deadline := time.Now().Add(5 * time.Second)
	var missing []string
	for {
		missing = missing[:0]
		for _, name := range created {
			if !hasPath(t, v, "win/Documents", name) {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d of %d changes never reached the index: %v", len(missing), n, missing)
}
