package identity_test

import (
	"crypto/tls"
	"net"
	"path/filepath"
	"testing"

	"github.com/ispengya/peerfold/internal/identity"
)

func TestServerCertificateIsStableAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	first, err := identity.LoadOrCreateServer(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateServer: %v", err)
	}
	second, err := identity.LoadOrCreateServer(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("fingerprint changed across loads: %s then %s", first.Fingerprint(), second.Fingerprint())
	}
	if _, err := identity.NormalizeFingerprint(first.Fingerprint()); err != nil {
		t.Fatalf("fingerprint is not normalized: %v", err)
	}
}

func TestClientRejectsTheWrongPinnedCertificate(t *testing.T) {
	dir := t.TempDir()
	srv, err := identity.LoadOrCreateServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	other, err := identity.LoadOrCreateServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
			}(c)
		}
	}()

	good, err := identity.ClientTLSConfig(srv.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), good)
	if err != nil {
		t.Fatalf("dial with the pinned fingerprint: %v", err)
	}
	conn.Close()

	bad, err := identity.ClientTLSConfig(other.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if c, err := tls.Dial("tcp", ln.Addr().String(), bad); err == nil {
		c.Close()
		t.Fatal("a connection whose certificate does not match the pin was accepted")
	}
}

func TestTokensAreHashedAndBoundToNameAndRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	token, err := identity.AddToken(path, "laptop", identity.RoleClient, false)
	if err != nil {
		t.Fatalf("AddToken: %v", err)
	}

	rec, ok := identity.CheckToken(path, "laptop", identity.RoleClient, token)
	if !ok {
		t.Fatal("the token just created did not verify")
	}
	if rec.Name != "laptop" || rec.Role != identity.RoleClient {
		t.Fatalf("record = %+v", rec)
	}
	if identity.TokenHash(token) == token {
		t.Fatal("the token file must not contain the token in plaintext")
	}
	if _, ok := identity.CheckToken(path, "laptop", identity.RoleViewer, token); ok {
		t.Error("a client token was accepted for the viewer role")
	}
	if _, ok := identity.CheckToken(path, "other", identity.RoleClient, token); ok {
		t.Error("a token was accepted under another name")
	}
	if _, ok := identity.CheckToken(path, "laptop", identity.RoleClient, token+"x"); ok {
		t.Error("a modified token was accepted")
	}

	rotated, err := identity.AddToken(path, "laptop", identity.RoleClient, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := identity.CheckToken(path, "laptop", identity.RoleClient, token); ok {
		t.Error("the old token survived rotation")
	}
	if _, ok := identity.CheckToken(path, "laptop", identity.RoleClient, rotated); !ok {
		t.Error("the rotated token was not accepted")
	}
}

// A handshake that is not TLS must not be mistaken for one: the client config
// is what enforces that, and it does so by refusing a plain server.
func TestClientTLSConfigRefusesAPlaintextServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cfg, err := identity.ClientTLSConfig(fp)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
	if err == nil {
		conn.Close()
		t.Fatal("a TLS client completed a handshake with a plaintext listener")
	}
}

func TestLoginCodeRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.LoadOrCreateServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := identity.LoadOrCreateEnrollSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := identity.LoadOrCreateEnrollSecret(dir)
	if err != nil || again != secret {
		t.Fatalf("enrollment secret changed across loads: %q -> %q (%v)", secret, again, err)
	}

	code := identity.LoginCode{Fingerprint: id.Fingerprint(), Secret: secret}
	got, err := identity.ParseLoginCode(code.String())
	if err != nil {
		t.Fatalf("ParseLoginCode: %v", err)
	}
	if got != code {
		t.Fatalf("round trip = %+v, want %+v", got, code)
	}

	for _, bad := range []string{
		"",
		"PF1." + id.Fingerprint(),
		"PF2." + id.Fingerprint() + "." + secret,
		"PF1.nope." + secret,
		"PF1." + id.Fingerprint() + ".",
		"PF1." + id.Fingerprint() + ".not base64!",
	} {
		if _, err := identity.ParseLoginCode(bad); err == nil {
			t.Errorf("ParseLoginCode(%q) succeeded", bad)
		}
	}
}
