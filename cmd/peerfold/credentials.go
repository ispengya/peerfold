package main

import (
	"crypto/tls"
	"errors"
	"fmt"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/identity"
	"github.com/ispengya/peerfold/internal/viewer"
)

// hasCredentials reports whether a config carries a login code or an enrolled
// token.
func hasCredentials(cfg config.Config) bool {
	return cfg.Token != "" || cfg.TLSFingerprint != ""
}

// viewerDialConfig turns a config's login code or enrolled token into a viewer
// connection config. An empty credential set is the explicit plaintext mode:
// the server, if secure, will refuse the handshake, which is the correct
// failure rather than a silent downgrade.
func viewerDialConfig(cfg config.Config) (viewer.DialConfig, error) {
	if !hasCredentials(cfg) {
		return viewer.DialConfig{}, nil
	}
	tlsCfg, err := credentialsTLS(cfg)
	if err != nil {
		return viewer.DialConfig{}, err
	}
	return viewer.DialConfig{TLS: tlsCfg, Token: cfg.Token}, nil
}

// credentialsTLS validates credentials that are present and returns the pinned
// client config. An incomplete pair is an error, never a partial credential.
func credentialsTLS(cfg config.Config) (*tls.Config, error) {
	if cfg.Token == "" || cfg.TLSFingerprint == "" {
		return nil, errors.New("incomplete credentials: both tls_fingerprint and token are required; paste the server login code again")
	}
	tlsCfg, err := identity.ClientTLSConfig(cfg.TLSFingerprint)
	if err != nil {
		return nil, fmt.Errorf("pinned server fingerprint: %w", err)
	}
	return tlsCfg, nil
}

// credentialName is the principal this config authenticates as. It is the name
// the token is bound to, not necessarily the local client name.
func credentialName(cfg config.Config) string {
	if cfg.AuthName != "" {
		return cfg.AuthName
	}
	return cfg.ClientName
}

// applyLoginCode stores a server-printed login code in a config. The code
// carries the certificate pin and the enrollment secret; the name and role are
// the machine's own and are sent when it enrolls.
func applyLoginCode(cfg *config.Config, code identity.LoginCode, name string) {
	cfg.TLSFingerprint = code.Fingerprint
	cfg.Token = code.Secret
	if name != "" {
		cfg.AuthName = name
	}
}
