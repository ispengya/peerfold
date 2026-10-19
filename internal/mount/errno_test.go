package mount

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/viewer"
)

func TestErrnoForKnownTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"enoent: no such file or directory", -ENOENT},
		{"enotdir: not a directory", -ENOTDIR},
		{"eisdir: is a directory", -EISDIR},
		{"enotempty: client \"win\" is not empty", -ENOTEMPTY},
		{"eexist: name is taken", -EEXIST},
		{"eacces: permission denied", -EACCES},
		{"exdev: cross-client rename", -EXDEV},
		{"eio: connection is gone", -EIO},
	}
	for _, c := range cases {
		if got := errnoFor(errors.New(c.text)); got != c.want {
			t.Errorf("errnoFor(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// An unrecognised error must never be guessed at. Guessing wrong between
// ENOENT and ENOTEMPTY is the difference between "the file is gone" and "this
// directory still has contents", and the second one is what protects data.
func TestErrnoForUnknownIsEIO(t *testing.T) {
	for _, text := range []string{
		"",
		"some other failure",
		"the system cannot find the file specified", // localized, no token
		"no such file or directory",                 // bare text, no token
		"enoent",                                    // token with no separator
		"ENOENT: shouty token",                      // tokens are case-sensitive
	} {
		if got := errnoFor(errors.New(text)); got != -EIO {
			t.Errorf("errnoFor(%q) = %d, want -EIO (%d)", text, got, -EIO)
		}
	}
}

func TestErrnoForNilIsZero(t *testing.T) {
	if got := errnoFor(nil); got != 0 {
		t.Errorf("errnoFor(nil) = %d, want 0", got)
	}
}

// The prefix must be a whole token, not a prefix of a longer word, or a
// message that happens to begin with those letters would be misclassified.
func TestErrnoForRequiresTokenBoundary(t *testing.T) {
	for _, text := range []string{"enoentish: not a token", "enoentx: not a token"} {
		if got := errnoFor(errors.New(text)); got != -EIO {
			t.Errorf("errnoFor(%q) = %d, want -EIO", text, got)
		}
	}
}

func TestWithTokenRoundTrips(t *testing.T) {
	err := withToken(tokenNotFound, errors.New("no such file or directory"))
	if got := errnoFor(err); got != -ENOENT {
		t.Errorf("errnoFor(withToken(...)) = %d, want -ENOENT", got)
	}
	if err.Error() != "enoent: no such file or directory" {
		t.Errorf("text = %q, want the token prefixed to the original", err.Error())
	}
}

// Every condition the mount layer must distinguish has to be enumerated here.
// A missing entry degrades to EIO, which is safe but wrong, and safe-and-wrong
// is exactly the kind of bug no other test will notice.
func TestEveryClassifiedErrorCarriesAToken(t *testing.T) {
	cases := []struct {
		what string
		err  error
		want int
	}{
		{"remove non-empty directory", withToken(tokenNotEmpty, errors.New(`client: "win" is not empty`)), -ENOTEMPTY},
		{"read missing file", withToken(tokenNotFound, errors.New("no such file")), -ENOENT},
		{"mkdir over a file", withToken(tokenExists, errors.New("name is taken")), -EEXIST},
		{"list under a file", withToken(tokenNotDir, errors.New("not a directory")), -ENOTDIR},
		{"rename across clients", withToken(tokenCrossClient, errors.New("cross-client rename")), -EXDEV},
	}
	for _, c := range cases {
		if got := errnoFor(c.err); got != c.want {
			t.Errorf("%s: errnoFor = %d, want %d", c.what, got, c.want)
		}
	}
}

// A cross-client rename is refused in the viewer, before any frame is sent, so
// it cannot carry a token: there is no wire for one to travel on. It is
// recognised by identity instead, which works only because the viewer wraps the
// sentinel rather than formatting it. This pins that arrangement, so a change
// to the viewer's error that dropped the wrapping would be caught here rather
// than showing up as an EIO in a mount.
func TestErrnoForRecognisesTheViewerRefusalByIdentity(t *testing.T) {
	err := fmt.Errorf("%w %q to %q: %q and %q", viewer.ErrCrossClient,
		"win/Documents/a.txt", "linux/Documents/a.txt", "win", "linux")
	if got := errnoFor(err); got != -EXDEV {
		t.Errorf("errnoFor(viewer cross-client refusal) = %d, want -EXDEV (%d)", got, -EXDEV)
	}
	// And the message must still be readable, since it is what a user sees.
	if !strings.Contains(err.Error(), "win/Documents/a.txt") {
		t.Errorf("the refusal lost its paths: %q", err.Error())
	}
}

// The token is a prefix, not a replacement: the original error has to stay
// reachable, or a caller that was using errors.Is on it would silently start
// getting false.
func TestTokenKeepsTheOriginalErrorReachable(t *testing.T) {
	base := fs.ErrNotExist
	err := withToken(tokenNotFound, base)
	if !errors.Is(err, base) {
		t.Errorf("errors.Is(withToken(tokenNotFound, fs.ErrNotExist), fs.ErrNotExist) = false; the token must not break the chain")
	}
	// Wrapping it again, as the layers above do, must not break it either.
	wrapped := fmt.Errorf("client: reading %q: %w", "a.txt", err)
	if !errors.Is(wrapped, base) {
		t.Error("a second wrap broke the chain to the original error")
	}
	if got := errnoFor(wrapped); got != -ENOENT {
		t.Errorf("errnoFor(double-wrapped) = %d, want -ENOENT", got)
	}
}

// tokenOf must find the token wherever the producers' wrapping put it, and
// must not read one out of text that merely contains a colon.
func TestTokenOf(t *testing.T) {
	cases := []struct {
		text  string
		token string
		errno int
		ok    bool
	}{
		// Position zero: a bare owner error on the wire.
		{"enoent: missing", "enoent", -ENOENT, true},
		// The shape the client actually sends: its own context first, then the
		// owner's token-prefixed error. This is the case that motivated
		// scanning rather than splitting on the first colon — splitting would
		// read "client" and lose the enoent underneath it.
		{`client: reading "a.txt": enoent: open a.txt: The system cannot find the file specified`,
			"enoent", -ENOENT, true},
		{`client: writing "b.txt": enotempty: remove b: The directory is not empty.`,
			"enotempty", -ENOTEMPTY, true},
		// After one level of wrapping, where the token follows a space.
		{"server: forward: exdev: cross-client rename", "exdev", -EXDEV, true},

		// No token at all.
		{"enoent:missing", "", 0, false},  // no separator
		{": leading colon", "", 0, false}, // empty token
		{"no colon at all", "", 0, false},
		{"", "", 0, false},
		{"client: something else", "", 0, false},
		{"the system cannot find the file specified", "", 0, false},
		// Near misses: the separator is required and the word must be whole.
		{"enoentish: not a token", "", 0, false},
		{"theenoent: not a token", "", 0, false},
		{"ENOENT: shouty", "", 0, false},
	}
	for _, c := range cases {
		token, errno, ok := tokenOf(c.text)
		if ok != c.ok || token != c.token || errno != c.errno {
			t.Errorf("tokenOf(%q) = %q, %d, %v; want %q, %d, %v", c.text, token, errno, ok, c.token, c.errno, c.ok)
		}
	}
}

// The regression that motivated matching whole tokens: an error that has
// collected context on its way up must still classify correctly when it
// reaches the wire. This is the shape the client actually sends.
func TestErrnoForSurvivesContextPrefixes(t *testing.T) {
	// Built the way the client builds it: a token-prefixed owner error, then
	// the client's own context wrapped around it.
	owner := withToken(tokenNotFound, errors.New("open a.txt: no such file or directory"))
	wire := fmt.Errorf("client: reading %q: %w", "a.txt", owner)
	if got := errnoFor(wire); got != -ENOENT {
		t.Errorf("errnoFor(context-prefixed) = %d, want -ENOENT; a prefixed error must not degrade to EIO", got)
	}
	// And the same for every token, not just the one that was reported.
	for _, c := range []struct {
		token string
		want  int
	}{
		{tokenNotFound, -ENOENT},
		{tokenNotDir, -ENOTDIR},
		{tokenIsDir, -EISDIR},
		{tokenNotEmpty, -ENOTEMPTY},
		{tokenExists, -EEXIST},
		{tokenPermission, -EACCES},
		{tokenCrossClient, -EXDEV},
		{tokenOffline, -EIO},
	} {
		prefixed := fmt.Errorf("client: some context: %w", withToken(c.token, errors.New("the owner's own wording")))
		if got := errnoFor(prefixed); got != c.want {
			t.Errorf("errnoFor(context-prefixed %s) = %d, want %d", c.token, got, c.want)
		}
	}
}
