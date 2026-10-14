package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"peerfold/internal/config"
)

// TestClientSavesConfigBeforeDialing pins the ordering that makes "one command
// to join" work: a server that cannot be reached must not also discard the
// shares the user just declared, because the whole point of running the command
// is to record them.
func TestClientSavesConfigBeforeDialing(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	var out, errOut bytes.Buffer
	code := run([]string{
		"client",
		"--server", "127.0.0.1:1",
		"--share", "Documents=" + tmp,
		"--config", cfgPath,
		"--name", "probe",
	}, &out, &errOut)

	if code == 0 {
		t.Fatalf("run(client to a dead port) = 0, want non-zero; stdout = %q", out.String())
	}

	// The connection failed, but the configuration did not.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load(%s) after a failed dial: %v", cfgPath, err)
	}
	if cfg.Server != "127.0.0.1:1" {
		t.Errorf("Server = %q, want %q", cfg.Server, "127.0.0.1:1")
	}
	if cfg.ClientName != "probe" {
		t.Errorf("ClientName = %q, want %q", cfg.ClientName, "probe")
	}
	if got := cfg.Shares["Documents"]; got != tmp {
		t.Errorf("Shares[Documents] = %q, want %q", got, tmp)
	}
}

// TestClientMergesIntoAnExistingConfig pins that a second run keeps the shares
// the first run recorded: the flags overlay the config, they do not replace it.
func TestClientMergesIntoAnExistingConfig(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	runOne := func(share string) {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := run([]string{
			"client",
			"--server", "127.0.0.1:1",
			"--share", share,
			"--config", cfgPath,
			"--name", "probe",
		}, &out, &errOut); code == 0 {
			t.Fatalf("dial to a dead port = 0, want non-zero")
		}
	}

	runOne("Alpha=" + first)
	runOne("Beta=" + second)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Shares["Alpha"]; got != first {
		t.Errorf("Shares[Alpha] = %q, want %q — the first run's share was dropped", got, first)
	}
	if got := cfg.Shares["Beta"]; got != second {
		t.Errorf("Shares[Beta] = %q, want %q", got, second)
	}
}

// TestClientUnsetFlagsKeepTheConfig pins that a flag left off is not treated as
// "set to empty": the config is the memory of how the client was told to join,
// so a later run that only adds a share must not blank the name or the server.
func TestClientUnsetFlagsKeepTheConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	var out, errOut bytes.Buffer
	if code := run([]string{
		"client",
		"--server", "127.0.0.1:1",
		"--share", "Documents=" + t.TempDir(),
		"--config", cfgPath,
		"--name", "kept",
	}, &out, &errOut); code == 0 {
		t.Fatalf("seed run = 0, want non-zero")
	}

	// Only a new share this time: --server and --name are absent.
	if code := run([]string{
		"client",
		"--share", "Other=" + t.TempDir(),
		"--config", cfgPath,
	}, &out, &errOut); code == 0 {
		t.Fatalf("second run = 0, want non-zero")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server != "127.0.0.1:1" {
		t.Errorf("Server = %q, want it kept at %q", cfg.Server, "127.0.0.1:1")
	}
	if cfg.ClientName != "kept" {
		t.Errorf("ClientName = %q, want it kept at %q", cfg.ClientName, "kept")
	}
	if _, ok := cfg.Shares["Other"]; !ok {
		t.Errorf("Shares = %v, want the new share added", cfg.Shares)
	}
}

func TestClientRejectsMalformedShare(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"client", "--share", "no-equals-sign", "--server", "127.0.0.1:1"}, &out, &errOut); code == 0 {
		t.Fatalf("run(client --share no-equals-sign) = 0, want non-zero")
	}
}

// TestClientRequiresAServer pins that "joined nothing" is an error rather than
// a silent no-op that leaves the process running with no server.
func TestClientRequiresAServer(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"client", "--config", filepath.Join(t.TempDir(), "config.json")}, &out, &errOut); code == 0 {
		t.Fatalf("run(client with no server) = 0, want non-zero")
	}
}

// TestClientNameDefaultsToHostname pins the documented default, so a user who
// does not name their machine still gets a usable registry entry.
func TestClientNameDefaultsToHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no host name available: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	var out, errOut bytes.Buffer
	if code := run([]string{
		"client",
		"--server", "127.0.0.1:1",
		"--share", "Documents=" + t.TempDir(),
		"--config", cfgPath,
	}, &out, &errOut); code == 0 {
		t.Fatalf("dial to a dead port = 0, want non-zero")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClientName != host {
		t.Errorf("ClientName = %q, want the host name %q", cfg.ClientName, host)
	}
}

// TestClientUnknownFlagExitsTwo pins that a usage error is distinguishable from
// a runtime failure, because a script that retries on 1 would retry a typo for
// ever.
func TestClientUnknownFlagExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"client", "--nonsense"}, &out, &errOut); code != 2 {
		t.Fatalf("run(client --nonsense) = %d, want 2", code)
	}
}

// TestClientDoesNotPersistAnInvalidShare pins that a bad --share is rejected
// before anything is written.
//
// The config merge is add-only, so a share saved by a command that failed would
// be permanent: every later run would fail on an entry the user never sees and
// has no way to remove. "The command failed" is not enough — the config has to
// be untouched.
func TestClientDoesNotPersistAnInvalidShare(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	before := config.Default()
	before.Server = "127.0.0.1:1"
	if err := config.Save(cfgPath, before); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cases := []struct {
		name  string
		share string
	}{
		{"a path that does not exist", "Documents=" + filepath.Join(t.TempDir(), "nope")},
		{"a name that is not a single component", "a/b=" + t.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run([]string{"client", "--config", cfgPath, "--share", tc.share}, &out, &errOut)
			if code == 0 {
				t.Fatalf("run(client --share %q) = 0, want non-zero", tc.share)
			}
			if errOut.Len() == 0 {
				t.Error("stderr is empty, want the reason for the failure")
			}

			after, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(after.Shares) != 0 {
				t.Fatalf("the rejected share was written to the config anyway: %v", after.Shares)
			}
		})
	}
}
