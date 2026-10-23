// Package identity holds the small amount of cryptography peerfold needs:
// a self-signed server certificate pinned by its clients, hashed per-principal
// tokens, and the login code that enrolls a new machine with the certificate
// fingerprint it should pin.
//
// There is deliberately no CA and no dependence on the system trust store. A
// peerfold server is not a public web site; a client that has been given the
// server's fingerprint can verify exactly the certificate it was enrolled with,
// and nothing else.
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ispengya/peerfold/internal/node"
)

// Roles a token may name.
const (
	RoleClient = "client"
	RoleViewer = "viewer"
)

// Server is a server's long-lived certificate and the fingerprint clients pin.
type Server struct {
	cert        tls.Certificate
	fingerprint string
}

// LoadOrCreateServer returns the certificate in dir, generating one on first
// use. The key is written 0600 and the certificate 0644; the directory itself is
// made 0700.
//
// A certificate is not rotated automatically. Clients pin it, so replacing it
// would disconnect every enrolled machine; an operator that needs a new one
// deletes the pair and re-enrolls clients deliberately.
func LoadOrCreateServer(dir string) (*Server, error) {
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err != nil {
			return nil, fmt.Errorf("identity: %s exists but %s does not: %w", certPath, keyPath, err)
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("identity: load certificate: %w", err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("identity: parse certificate: %w", err)
		}
		cert.Leaf = leaf
		return &Server{cert: cert, fingerprint: Fingerprint(leaf)}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("identity: inspect %s: %w", certPath, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("identity: create %s: %w", dir, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("identity: generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "peerfold"},
		DNSNames:              []string{"peerfold"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("identity: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("identity: encode key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("identity: write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("identity: write %s: %w", certPath, err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("identity: parse new certificate: %w", err)
	}
	return &Server{
		cert:        tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		fingerprint: Fingerprint(leaf),
	}, nil
}

// TLSConfig is the server side of a connection.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{s.cert},
		MinVersion:   tls.VersionTLS13,
	}
}

// Fingerprint is the lowercase SHA-256 of the certificate's DER form.
func (s *Server) Fingerprint() string { return s.fingerprint }

// Fingerprint is the SHA-256 of a certificate's DER form, in lowercase hex.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// NormalizeFingerprint accepts the forms an operator is likely to paste: with or
// without a "sha256:" prefix, and in either case.
func NormalizeFingerprint(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != 64 {
		return "", fmt.Errorf("identity: fingerprint must be 64 hex characters, got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("identity: fingerprint is not hex: %w", err)
	}
	return s, nil
}

// ClientTLSConfig returns a config that accepts exactly the certificate with
// the given fingerprint.
//
// Verification is a pin, not a chain: a self-signed certificate is its own
// authority, so normal chain verification would either need it installed
// system-wide or would accept any certificate. The pin is checked after the
// handshake and before any application bytes are sent.
func ClientTLSConfig(fingerprint string) (*tls.Config, error) {
	want, err := NormalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	wantBytes, _ := hex.DecodeString(want)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: "peerfold",
		// The pin below is the verification. Skip the default CA/hostname
		// check, which cannot apply to a self-signed certificate.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("identity: server presented no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], wantBytes) != 1 {
				return fmt.Errorf("identity: server fingerprint %s does not match the pinned %s",
					hex.EncodeToString(got[:]), want)
			}
			return nil
		},
		ClientSessionCache: tls.NewLRUClientSessionCache(32),
	}, nil
}

// ── tokens ────────────────────────────────────────────────────────────────

// TokenRecord is one principal's credential, as stored on the server. Only the
// hash is kept; the token itself is returned once, when the machine enrolls.
type TokenRecord struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Hash    string `json:"hash"`
	Write   bool   `json:"write,omitempty"`
	Created int64  `json:"created"`
}

type tokenFile struct {
	Tokens []TokenRecord `json:"tokens"`
}

// TokenHash is the SHA-256 of a token, in lowercase hex.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken returns a fresh 256-bit token in URL-safe base64.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("identity: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// SetToken records token as name's credential, replacing any earlier token for
// the same name. Only the hash is stored.
func SetToken(path, name, role, token string, write bool) error {
	if !node.ValidName(name) {
		return fmt.Errorf("identity: %q is not one path component", name)
	}
	if role != RoleClient && role != RoleViewer {
		return fmt.Errorf("identity: unknown role %q", role)
	}
	if token == "" {
		return errors.New("identity: token must not be empty")
	}
	tf, err := loadTokens(path)
	if err != nil {
		return err
	}
	kept := tf.Tokens[:0]
	for _, t := range tf.Tokens {
		if t.Name != name {
			kept = append(kept, t)
		}
	}
	tf.Tokens = append(kept, TokenRecord{
		Name:    name,
		Role:    role,
		Hash:    TokenHash(token),
		Write:   write,
		Created: time.Now().Unix(),
	})
	return saveTokens(path, tf)
}

// AddToken creates a token for name, replacing any earlier token for the same
// name, and returns the freshly generated token.
func AddToken(path, name, role string, write bool) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	if err := SetToken(path, name, role, token, write); err != nil {
		return "", err
	}
	return token, nil
}

// LookupToken returns the record for name. A missing file or a missing name is
// not an error: both simply mean there is no credential to match.
func LookupToken(path, name string) (TokenRecord, bool, error) {
	tf, err := loadTokens(path)
	if err != nil {
		return TokenRecord{}, false, err
	}
	for _, t := range tf.Tokens {
		if t.Name == name {
			return t, true, nil
		}
	}
	return TokenRecord{}, false, nil
}

// CheckToken reports whether token is the credential recorded for name and has
// the expected role. The comparison is constant time.
func CheckToken(path, name, role, token string) (TokenRecord, bool) {
	rec, ok, err := LookupToken(path, name)
	if err != nil || !ok || token == "" || rec.Role != role {
		return TokenRecord{}, false
	}
	want, err := hex.DecodeString(rec.Hash)
	if err != nil {
		return TokenRecord{}, false
	}
	got := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return TokenRecord{}, false
	}
	return rec, true
}

func loadTokens(path string) (tokenFile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return tokenFile{}, nil
	}
	if err != nil {
		return tokenFile{}, fmt.Errorf("identity: read tokens %s: %w", path, err)
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return tokenFile{}, fmt.Errorf("identity: parse tokens %s: %w", path, err)
	}
	return tf, nil
}

// saveTokens writes the file atomically and with owner-only permissions. An
// A token file is a credential store, so a partially written file must never
// become one a login happens to read.
func saveTokens(path string, tf tokenFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("identity: create %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	f, err := os.CreateTemp(dir, ".tokens-*.json")
	if err != nil {
		return fmt.Errorf("identity: create tokens temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("identity: replace %s: %w", path, err)
	}
	return nil
}

// ── login code ────────────────────────────────────────────────────────────

// LoadOrCreateEnrollSecret returns the server's enrollment secret, generating
// it on first use. The file is owner-only; a machine that can read it can
// enroll new peers.
//
// The secret is intentionally stable across restarts: the server prints the
// same login code until the operator deletes the file. Deleting it revokes
// every not-yet-enrolled machine, while already-issued per-name tokens keep
// working.
func LoadOrCreateEnrollSecret(dir string) (string, error) {
	path := filepath.Join(dir, "enroll.key")
	if b, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(b))
		if s == "" {
			return "", fmt.Errorf("identity: %s is empty", path)
		}
		return s, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("identity: read %s: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("identity: create %s: %w", dir, err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("identity: generate enrollment secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("identity: write %s: %w", path, err)
	}
	return secret, nil
}

// LoginCode is what the server prints at startup: the certificate a client
// should pin and the secret that enrolls it. It deliberately contains no
// address, principal or role; those belong to the machine being enrolled.
type LoginCode struct {
	Fingerprint string
	Secret      string
}

// String is PF1.<fingerprint>.<secret>. A dot is not part of either component,
// so the string splits unambiguously.
func (c LoginCode) String() string {
	return "PF1." + c.Fingerprint + "." + c.Secret
}

// ParseLoginCode validates a pasted login code.
func ParseLoginCode(s string) (LoginCode, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 || parts[0] != "PF1" {
		return LoginCode{}, errors.New("identity: login code must look like PF1.<fingerprint>.<secret>")
	}
	fp, err := NormalizeFingerprint(parts[1])
	if err != nil {
		return LoginCode{}, err
	}
	if parts[2] == "" {
		return LoginCode{}, errors.New("identity: login code has no secret")
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return LoginCode{}, fmt.Errorf("identity: login code secret is not base64url: %w", err)
	}
	return LoginCode{Fingerprint: fp, Secret: parts[2]}, nil
}
