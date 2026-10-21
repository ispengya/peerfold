package client

import (
	"testing"

	"github.com/ispengya/peerfold/internal/node"
)

// newBatchWatcher builds a watcher with no fsnotify behind it, so the
// coalescing rules can be driven directly rather than through a filesystem
// race.
func newBatchWatcher(t *testing.T) (*watch, *[]node.Node) {
	t.Helper()
	var got []node.Node
	w := &watch{
		pending: map[string]node.Node{},
		emit: func(nodes []node.Node) error {
			got = append(got, nodes...)
			return nil
		},
	}
	return w, &got
}

func TestTheBatchKeepsOnlyTheLastStateOfAPath(t *testing.T) {
	w, got := newBatchWatcher(t)
	w.enqueue([]node.Node{{Path: "Documents/a.txt", Kind: node.KindFile, Size: 0}})
	w.enqueue([]node.Node{{Path: "Documents/a.txt", Kind: node.KindFile, Size: 5}})
	w.flush()

	if len(*got) != 1 {
		t.Fatalf("emitted %v, want one node for the path", *got)
	}
	if (*got)[0].Size != 5 {
		t.Fatalf("size = %d, want the later 5", (*got)[0].Size)
	}
}

func TestADeleteAfterACreateArrivesLast(t *testing.T) {
	w, got := newBatchWatcher(t)
	w.enqueue([]node.Node{{Path: "Documents/a.txt", Kind: node.KindFile, Size: 5}})
	w.enqueue([]node.Node{{Path: "Documents/b.txt", Kind: node.KindFile, Size: 1}})
	w.enqueue([]node.Node{{Path: "Documents/a.txt", Kind: node.KindDeleted}})
	w.flush()

	if len(*got) != 2 {
		t.Fatalf("emitted %v, want two nodes", *got)
	}
	// b was reported between a's create and its delete, so it must come first.
	if (*got)[0].Path != "Documents/b.txt" {
		t.Fatalf("first node = %q, want Documents/b.txt", (*got)[0].Path)
	}
	if (*got)[1].Path != "Documents/a.txt" || (*got)[1].Kind != node.KindDeleted {
		t.Fatalf("second node = %+v, want the tombstone for Documents/a.txt", (*got)[1])
	}
}

func TestACreateAfterADeleteWins(t *testing.T) {
	w, got := newBatchWatcher(t)
	w.enqueue([]node.Node{{Path: "Documents/a.txt", Kind: node.KindDeleted}})
	w.enqueue([]node.Node{{Path: "Documents/a.txt", Kind: node.KindFile, Size: 3}})
	w.flush()

	if len(*got) != 1 || (*got)[0].Kind != node.KindFile || (*got)[0].Size != 3 {
		t.Fatalf("emitted %v, want one file node of size 3", *got)
	}
}

func TestAStaleChildUnderADeletedDirectoryIsDropped(t *testing.T) {
	w, got := newBatchWatcher(t)
	w.enqueue([]node.Node{{Path: "Documents/sub/a.txt", Kind: node.KindFile, Size: 5}})
	w.enqueue([]node.Node{{Path: "Documents/sub", Kind: node.KindDeleted}})
	w.flush()

	if len(*got) != 1 {
		t.Fatalf("emitted %v, want only the directory tombstone", *got)
	}
	if (*got)[0].Path != "Documents/sub" || (*got)[0].Kind != node.KindDeleted {
		t.Fatalf("emitted %+v, want the tombstone for Documents/sub", (*got)[0])
	}
}

func TestTheCountBoundFlushesWithoutWaitingForTheWindow(t *testing.T) {
	w, got := newBatchWatcher(t)
	for i := 0; i < maxEventBatch; i++ {
		w.enqueue([]node.Node{{Path: "Documents/f" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Kind: node.KindFile}})
	}
	// enqueue flushes as soon as the distinct count reaches the bound.
	if len(*got) != maxEventBatch {
		t.Fatalf("emitted %d nodes before flush, want %d", len(*got), maxEventBatch)
	}
}
