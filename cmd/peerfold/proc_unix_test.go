//go:build !windows

package main

import "testing"

// TestQuoteDesktopArg pins the XDG Exec= quoting used by setAutoStart.
func TestQuoteDesktopArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"server", "server"},
		{"--daemon", "--daemon"},
		{"", `""`},
		{"a b", `"a b"`},
		{`C:\path\to`, `"C:\\path\\to"`},
		{`say "hi"`, `"say \"hi\""`},
	}
	for _, c := range cases {
		if got := quoteDesktopArg(c.in); got != c.want {
			t.Errorf("quoteDesktopArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSetRemoveAutoStartRoundTrip pins the XDG autostart helpers against the
// real ~/.config/autostart directory. It is allowed to write there because the
// path is derived from UserHomeDir, which tests already use for pid files.
func TestSetRemoveAutoStartRoundTrip(t *testing.T) {
	const component = "client-autostarttest"
	defer removeAutoStart(component)

	if err := setAutoStart(component, "client --daemon"); err != nil {
		t.Fatalf("setAutoStart: %v", err)
	}
	if !hasAutoStart(component) {
		t.Fatal("hasAutoStart = false after setAutoStart")
	}
	if err := removeAutoStart(component); err != nil {
		t.Fatalf("removeAutoStart: %v", err)
	}
	if hasAutoStart(component) {
		t.Fatal("hasAutoStart = true after removeAutoStart")
	}
}
