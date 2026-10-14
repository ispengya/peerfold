package proto

import "encoding/json"

// Envelope is the payload of a control frame: a request, a response, or an
// error, correlated by ID. Body is left as raw JSON so each kind can decode
// into its own struct.
type Envelope struct {
	ID   uint64          `json:"id"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body,omitempty"`
	Err  string          `json:"err,omitempty"`
	EOF  bool            `json:"eof,omitempty"`
}

// Control message kinds. Every kind is a plain string so it can travel in
// Envelope.Kind unchanged.
const (
	KindHello     = "hello"
	KindHelloOK   = "hello_ok"
	KindTreeBatch = "tree_batch"
	KindTreeDone  = "tree_done"
	KindEvent     = "event"
	KindList      = "list"
	KindListOK    = "list_ok"
	KindRead      = "read"
	KindWrite     = "write"
	KindMkdir     = "mkdir"
	KindCreate    = "create"
	KindRename    = "rename"
	KindRemove    = "remove"
)

// Roles a client declares in its hello. A viewer dials in to observe the
// aggregated tree without offering shares of its own.
const (
	RoleClient = "client"
	RoleViewer = "viewer"
)

// HelloBody is the first message a client sends.
type HelloBody struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Shares    []string `json:"shares"`
	Aggregate bool     `json:"aggregate"`
}

// HelloOKBody answers a hello. Aggregate reports the server's ruling: a second
// client asking to aggregate is downgraded to a plain supplier rather than
// rejected.
type HelloOKBody struct {
	Name      string `json:"name"`
	Aggregate bool   `json:"aggregate"`
}

// NodeJSON mirrors node.Node for the wire, so the two encode identically and
// an index update can be carried without a translation layer. Its field types
// must stay in step with node.Node.
type NodeJSON struct {
	Path  string `json:"path"`
	Kind  uint8  `json:"kind"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	Mtime int64  `json:"mtime"`
}

// TreeDoneBody closes a tree upload and lists the share names the client
// declared.
type TreeDoneBody struct {
	Shares []string `json:"shares"`
}

// ListBody asks for one directory of the aggregated tree.
type ListBody struct {
	Client string `json:"client"`
	Path   string `json:"path"`
}

// ListOKBody is the directory listing itself.
type ListOKBody struct {
	Nodes []NodeJSON `json:"nodes"`
}

// ReadBody asks the owning client for a byte range of a file.
type ReadBody struct {
	Client string `json:"client"`
	Path   string `json:"path"`
	Off    int64  `json:"off"`
	N      int64  `json:"n"`
}

// WriteBody announces a whole-file write-back to the owning client. Content
// follows as a data stream on the same ID.
type WriteBody struct {
	Client   string `json:"client"`
	Path     string `json:"path"`
	Truncate bool   `json:"truncate"`
}

// MkdirBody asks the owning client to create a directory.
type MkdirBody struct {
	Client string `json:"client"`
	Path   string `json:"path"`
}

// CreateBody asks the owning client to create an empty file.
type CreateBody struct {
	Client string `json:"client"`
	Path   string `json:"path"`
}

// RenameBody asks the owning client to rename within its own tree.
type RenameBody struct {
	Client  string `json:"client"`
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
}

// RemoveBody asks the owning client to remove a path.
type RemoveBody struct {
	Client string `json:"client"`
	Path   string `json:"path"`
}

// EventBody is one index update pushed from a client. A node whose Kind is
// node.NodeKindDeleted marks a path that no longer exists.
type EventBody struct {
	Nodes []NodeJSON `json:"nodes"`
}
