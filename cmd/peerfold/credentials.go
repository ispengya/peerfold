package main

import (
	"crypto/tls"
	"errors"
	"fmt"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/identity"
	"github.com/ispengya/peerfold/internal/viewer"
)

// hasCredentials reports whether a config carries an invite at all.
func hasCredentials(cfg config.Config) bool {
	return cfg.Token != "" || cfg.TLSFingerprint != ""
}

// viewerDialConfig turns a config's invite into a viewer connection config. An
// empty invite is the explicit plaintext mode: the server, if secure, will
// refuse the handshake, which is the correct failure rather than a silent
// downgrade.
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

// credentialsTLS validates an invite that is present and returns its pinned
// client config. An incomplete invite is an error, never a partial credential.
func credentialsTLS(cfg config.Config) (*tls.Config, error) {
	if cfg.Token == "" || cfg.TLSFingerprint == "" {
		return nil, errors.New("incomplete invite: both tls_fingerprint and token are required; paste the invite again")
	}
	tlsCfg, err := identity.ClientTLSConfig(cfg.TLSFingerprint)
	if err != nil {
		return nil, fmt.Errorf("invite fingerprint: %w", err)
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

// applyInvite stores an invite in a config. The invite fixes the server, the
// pinned certificate, the principal and the token; a client name the server
// will index under is the same principal.
func applyInvite(cfg *config.Config, inv identity.Invite) {
	cfg.Server = inv.Address
	cfg.TLSFingerprint = inv.Fingerprint
	cfg.AuthName = inv.Name
	cfg.Token = inv.Token
	cfg.ClientName = inv.Name
}
