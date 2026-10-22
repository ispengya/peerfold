package server_test

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/identity"
	"github.com/ispengya/peerfold/internal/proto"
	"github.com/ispengya/peerfold/internal/server"
)

type secureFixture struct {
	srv         *server.Server
	addr        string
	fingerprint string
	tokens      string
	clientToken string
	viewerToken string
}

func startSecure(t *testing.T) secureFixture {
	t.Helper()
	dir := t.TempDir()
	id, err := identity.LoadOrCreateServer(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	tokensPath := filepath.Join(dir, "tokens.json")
	clientToken, err := identity.AddToken(tokensPath, "win", identity.RoleClient, false)
	if err != nil {
		t.Fatalf("AddToken(client): %v", err)
	}
	viewerToken, err := identity.AddToken(tokensPath, "mount", identity.RoleViewer, true)
	if err != nil {
		t.Fatalf("AddToken(viewer): %v", err)
	}

	s := server.NewWithOptions("127.0.0.1:0", server.Options{
		TLSConfig:  id.TLSConfig(),
		TokensPath: tokensPath,
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

	return secureFixture{
		srv: s, addr: s.Addr().String(), fingerprint: id.Fingerprint(),
		tokens: tokensPath, clientToken: clientToken, viewerToken: viewerToken,
	}
}

// dialSecure sends one hello over a pinned TLS connection and returns the reply.
func dialSecure(t *testing.T, addr, fingerprint, name, role, token string, compress bool) proto.Envelope {
	t.Helper()
	cfg, err := identity.ClientTLSConfig(fingerprint)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	raw, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn := proto.NewConn(raw)

	body, err := json.Marshal(proto.HelloBody{
		Name: name, Role: role, Token: token, Compress: compress,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err != nil {
		t.Fatalf("Send hello: %v", err)
	}
	env, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv hello reply: %v", err)
	}
	return env
}

func TestSecureServerAcceptsOnlyTheTokenBoundToTheNameAndRole(t *testing.T) {
	f := startSecure(t)

	env := dialSecure(t, f.addr, f.fingerprint, "win", proto.RoleClient, f.clientToken, true)
	if env.Err != "" {
		t.Fatalf("valid client token refused: %s", env.Err)
	}
	if env.Kind != proto.KindHelloOK {
		t.Fatalf("hello reply kind = %q", env.Kind)
	}
	var ok proto.HelloOKBody
	if err := json.Unmarshal(env.Body, &ok); err != nil {
		t.Fatalf("decode hello_ok: %v", err)
	}
	if !ok.Compress {
		t.Error("compression was not negotiated on a secure connection")
	}

	for _, tc := range []struct {
		name  string
		role  string
		token string
	}{
		{"win", proto.RoleClient, "wrong"},
		{"win", proto.RoleClient, ""},
		{"win", proto.RoleViewer, f.clientToken}, // right token, wrong role
		{"other", proto.RoleClient, f.clientToken},
	} {
		env := dialSecure(t, f.addr, f.fingerprint, tc.name, tc.role, tc.token, false)
		if env.Err == "" {
			t.Errorf("hello name=%q role=%q token=%q was accepted", tc.name, tc.role, tc.token)
		}
		if strings.Contains(env.Err, f.clientToken) {
			t.Errorf("the refusal echoed the token: %q", env.Err)
		}
	}

	// A viewer must also name the principal its token belongs to.
	env = dialSecure(t, f.addr, f.fingerprint, "mount", proto.RoleViewer, f.viewerToken, true)
	if env.Err != "" {
		t.Fatalf("valid viewer token refused: %s", env.Err)
	}
}

func TestSecureServerRefusesAPlaintextHandshake(t *testing.T) {
	f := startSecure(t)

	raw, err := net.DialTimeout("tcp", f.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(2 * time.Second))
	conn := proto.NewConn(raw)
	body, _ := json.Marshal(proto.HelloBody{Name: "win", Role: proto.RoleClient, Token: f.clientToken})
	if err := conn.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err == nil {
		if env, err := conn.Recv(); err == nil && env.Err == "" {
			t.Fatal("a plaintext hello was accepted by a TLS server")
		}
	}

	if live := f.srv.Registry().Live(); len(live) != 0 {
		t.Fatalf("Live() = %v, want empty: a plaintext peer must not register", live)
	}
}

func TestAuthenticationWithoutTLSIsRefusedAtStartup(t *testing.T) {
	s := server.NewWithOptions("127.0.0.1:0", server.Options{TokensPath: filepath.Join(t.TempDir(), "tokens.json")})
	if s.Addr() != nil {
		t.Fatal("a server with tokens but no TLS bound a listener")
	}
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("Err() = %v, want a TLS requirement", err)
	}
}
