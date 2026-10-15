package viewer

import "testing"

// The split is the one place the viewer interprets a path, and getting it wrong
// is invisible until a real server rejects the request: an early version put the
// whole path in the server-side path field for a bare client name, so every
// "list this client" call was answered with "no such client".

func TestSplitPath(t *testing.T) {
	cases := []struct {
		in         string
		wantClient string
		wantPath   string
	}{
		// The top level names no client.
		{"", "", ""},
		// A bare name is a client, and the server-side path is empty.
		{"win", "win", ""},
		// One separator splits it; the rest belongs to the client's namespace.
		{"win/Documents", "win", "Documents"},
		{"win/Documents/sub", "win", "Documents/sub"},
		// A separator at the front names an empty client, which the server
		// then rejects: better than silently listing the top level.
		{"/Documents", "", "Documents"},
		// A trailing separator addresses the client's root.
		{"win/", "win", ""},
		// Backslash is a redundant spelling of the boundary, which a Windows
		// user will type.
		{`win\Documents`, "win", "Documents"},
	}
	for _, tc := range cases {
		gotClient, gotPath := splitPath(tc.in)
		if gotClient != tc.wantClient || gotPath != tc.wantPath {
			t.Errorf("splitPath(%q) = (%q, %q), want (%q, %q)",
				tc.in, gotClient, gotPath, tc.wantClient, tc.wantPath)
		}
	}
}
