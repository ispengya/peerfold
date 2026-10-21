package viewer_test

import (
	"bytes"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/proto"
	"github.com/ispengya/peerfold/internal/viewer"
)

// TestPoolReadsRunInParallelAndOnlyThePrimaryWatches pins both halves of the
// connection-pool contract:
//
//   - data connections really do overlap, so a mount's parallel readers are no
//     longer queued behind one connection's single in-flight request;
//   - the extra connections ask not to be watched, so the server does not build
//     and send event batches to a connection with no consumer.
func TestPoolReadsRunInParallelAndOnlyThePrimaryWatches(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var watches []bool
	var active, peak int32
	var conns int32

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&conns, 1)
			go func() {
				defer c.Close()
				pc := proto.NewConn(c)
				env, err := pc.Recv()
				if err != nil || env.Kind != proto.KindHello {
					return
				}
				var hb proto.HelloBody
				if err := json.Unmarshal(env.Body, &hb); err != nil {
					return
				}
				mu.Lock()
				watches = append(watches, hb.Watching())
				mu.Unlock()
				if err := pc.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHelloOK}); err != nil {
					return
				}
				for {
					req, err := pc.Recv()
					if err != nil || req.Kind != proto.KindRead {
						return
					}
					n := atomic.AddInt32(&active, 1)
					for {
						p := atomic.LoadInt32(&peak)
						if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
							break
						}
					}
					time.Sleep(40 * time.Millisecond)
					atomic.AddInt32(&active, -1)

					head := proto.Envelope{ID: req.ID, Kind: proto.KindReadOK}
					if err := pc.Send(head); err != nil {
						return
					}
					body := bytes.Repeat([]byte("a"), 8)
					if err := pc.SendBody(req.ID, body); err != nil {
						return
					}
					if err := pc.Send(head); err != nil {
						return
					}
				}
			}()
		}
	}()

	primary, err := viewer.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = primary.Close() })

	pool := viewer.NewConnPool(primary, 2)
	t.Cleanup(func() { _ = pool.Close() })

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := pool.Read("win/a", 0, 8)
			if err != nil {
				t.Errorf("pool.Read: %v", err)
				return
			}
			if len(got) != 8 {
				t.Errorf("pool.Read returned %d bytes, want 8", len(got))
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&peak); got < 2 {
		t.Errorf("peak concurrent reads = %d, want 2: the pool serialised them", got)
	}
	if got := atomic.LoadInt32(&conns); got != 3 {
		t.Errorf("server saw %d connections, want 3 (one primary and two data connections)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(watches) != 3 {
		t.Fatalf("recorded %d hellos, want 3", len(watches))
	}
	watching := 0
	for _, w := range watches {
		if w {
			watching++
		}
	}
	if watching != 1 {
		t.Errorf("%d connections asked to watch, want exactly the primary", watching)
	}
}
