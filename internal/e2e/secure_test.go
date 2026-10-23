package e2e

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/client"
	"github.com/ispengya/peerfold/internal/identity"
	"github.com/ispengya/peerfold/internal/server"
	"github.com/ispengya/peerfold/internal/viewer"
)

// The secure flow end to end: a pinned TLS server, a token bound to the
// supplier's name, a viewer token, and the ordinary read/write path over it.
func TestE2ESecureFlow(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.LoadOrCreateServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	tokensPath := filepath.Join(dir, "tokens.json")
	const enrollSecret = "ZTJlLWVucm9sbC1zZWNyZXQ"

	s := server.NewWithOptions("127.0.0.1:0", server.Options{
		TLSConfig:    id.TLSConfig(),
		TokensPath:   tokensPath,
		EnrollSecret: enrollSecret,
	})
	if s.Addr() == nil {
		t.Fatalf("secure server did not bind: %v", s.Err())
	}
	go func() {
		if err := s.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { s.Close() })
	addr := s.Addr().String()

	tlsCfg, err := identity.ClientTLSConfig(id.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}

	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	sh, err := client.NewShares([]string{"Documents=" + root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Dial(client.Options{
		Server: addr, Name: "win", Shares: sh,
		TLSConfig: tlsCfg, Token: enrollSecret,
	})
	if err != nil {
		t.Fatalf("secure client dial: %v", err)
	}
	defer c.Close()
	if c.IssuedToken() == "" {
		t.Fatal("client enrollment issued no token")
	}
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}

	v, err := viewer.DialMountWithConfig(addr, "laptop", "/mnt/secure", viewer.DialConfig{
		TLS: tlsCfg, Token: enrollSecret,
	})
	if err != nil {
		t.Fatalf("secure viewer dial: %v", err)
	}
	defer v.Close()
	viewerToken := v.IssuedToken()
	if viewerToken == "" {
		t.Fatal("viewer enrollment issued no token")
	}

	got, err := v.Cat("win/Documents/a.txt")
	if err != nil || string(got) != "aaaa" {
		t.Fatalf("Cat = %q, %v", got, err)
	}
	if err := v.Put("win/Documents/b.txt", strings.NewReader("written over TLS")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "b.txt")); err != nil || string(b) != "written over TLS" {
		t.Fatalf("owner file = %q, %v", b, err)
	}

	// Pool connections must reuse the authenticated principal, not just the
	// transport.
	second, err := v.NewReadViewer()
	if err != nil {
		t.Fatalf("NewReadViewer: %v", err)
	}
	defer second.Close()
	if nodes, err := second.List("win/Documents"); err != nil || len(nodes) != 1 {
		t.Fatalf("pooled viewer List = %v, %v", nodes, err)
	}

	// A wrong token must not get a viewer connection at all.
	bad, err := viewer.DialMountWithConfig(addr, "laptop", "/mnt/bad", viewer.DialConfig{
		TLS: tlsCfg, Token: "wrong",
	})
	if err == nil {
		bad.Close()
		t.Fatal("a viewer with the wrong token was accepted")
	}

}
