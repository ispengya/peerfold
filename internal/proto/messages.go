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
	// KindReadOK is a client's answer to a read request. It is a kind of its own
	// because request and answer travel in opposite directions on the same
	// connection and are otherwise the same shape: an answer carries no body, but
	// neither does a request whose body failed to encode.
	KindReadOK = "read_ok"
	KindWrite  = "write"
	// KindWriteOK is a client's answer to a write request. Every write — put,
	// create, mkdir, remove, rename — is answered with this one kind, because
	// the answer carries nothing but "it worked" or the reason it did not, and
	// the request already said which operation it was. It is a kind of its own
	// for the same reason as KindReadOK: request and answer travel in opposite
	// directions on one connection and are otherwise identical in shape.
	KindWriteOK = "write_ok"
	KindMkdir   = "mkdir"
	KindRename  = "rename"
	KindRemove  = "remove"

	// KindViewers asks the server which mounts are currently attached to it.
	// It is answered with KindViewersOK, whose body lists them. The question is
	// how `peerfold status` sees a mount running on another machine: the mount
	// is a viewer, and no local pid file can describe a process elsewhere.
	KindViewers   = "viewers"
	KindViewersOK = "viewers_ok"

	// KindClients asks the server which suppliers are connected. It is the
	// other half of the status view: a mount can be started on one machine,
	// a client on another, and status should describe both from anywhere.
	KindClients   = "clients"
	KindClientsOK = "clients_ok"

	// KindBye is a peer telling the server it is about to leave. The socket
	// closing would say the same thing eventually, but a client that is
	// stopping gracefully wants the withdrawal to happen while it can still
	// wait for confirmation, so the next start does not race the server still
	// believing the old name is connected.
	KindBye   = "bye"
	KindByeOK = "bye_ok"
)

// Write-body modes. A whole-file write-back either replaces what is there or
// refuses to touch a name that is already taken, and those are the only two
// things a "put this file" request can mean.
const (
	// WriteModePut replaces the target, creating it if it is absent. This is
	// what `peerfold put` sends.
	WriteModePut = "put"
	// WriteModeCreate refuses if the name is taken. This is `peerfold put
	// --exclusive`, for a caller that must not clobber a file it did not know
	// was there.
	WriteModeCreate = "create"
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

	// MountPoint is set by a mount viewer: the directory it exposes on its
	// own machine. It is what distinguishes a mount from the short-lived
	// viewer a command like `peerfold ls` opens, and an ordinary viewer leaves
	// it empty. Older servers ignore it, which is why it is omitempty.
	MountPoint string `json:"mount_point,omitempty"`
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

// TreeBatchBody carries one batch of a client's scanned tree. A large tree is
// several of these, and the server indexes each batch as it arrives rather than
// waiting for the end, so the aggregated tree grows while the upload runs.
type TreeBatchBody struct {
	Nodes []NodeJSON `json:"nodes"`
}

// TreeDoneBody closes a tree upload and lists the share names the client
// declared. It is the last message of an upload, and the server's reply to it
// is the upload's acknowledgement.
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
	// N is the largest number of bytes wanted, or negative for "to the end of
	// the file". It is an int rather than an int64 because it reaches the
	// owner's filesystem layer as a count, not an offset.
	N int `json:"n"`
}

// WriteBody announces a whole-file write-back to the owning client. Content
// follows as a data stream on the same ID, exactly as a read's content does, so
// a put does not have to fit in one frame.
//
// Path is share-relative on the owner ("Documents/a.txt"), and the owner splits
// it into a share name and a path within that share the same way it does for a
// read.
//
// Mode is one of the WriteMode values, and it is a single word rather than a
// boolean because there is no sensible default: a caller has to say whether it
// means "make this the file's content" or "make this file, and only if it is
// not already there".
type WriteBody struct {
	Client string `json:"client"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
}

// MkdirBody asks the owning client to create a directory.
type MkdirBody struct {
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

// EventBody is one index update pushed from a client. A node whose Kind is the
// deletion marker (255; node.KindDeleted, added in Task 3) marks a path that no
// longer exists.
//
// Seq is a per-connection monotonic counter, stamped by the sender as each batch
// is enqueued. The mount layer uses it to notice that events were lost: the
// transport is allowed to drop an event when a consumer is slow (viewer.emit does
// exactly that, deliberately), and a drop is not an error anywhere else in the
// system, so without a counter a stale tree is invisible.
//
// It is omitempty so that a sender which does not number its events produces the
// same bytes as before this field existed. Zero means "not numbered" rather than
// "sequence zero", so an older peer's events are not read as a gap of one.
type EventBody struct {
	Seq   uint64     `json:"seq,omitempty"`
	Nodes []NodeJSON `json:"nodes"`
}

// ViewerInfo describes one mount connected to the server.
//
// It is the server's account of the connection, not the mount's: Name and
// MountPoint are what the mount declared in its hello, while RemoteAddr is
// what the server observed and Since is when it accepted the connection. That
// split means status can report a mount even when the machine running it is
// unreachable by any other route.
type ViewerInfo struct {
	Name       string `json:"name"`
	MountPoint string `json:"mount_point"`
	RemoteAddr string `json:"remote_addr"`
	// Since is Unix seconds. A JSON time would need a time zone and a parser
	// on every peer for no benefit: the only thing anyone does with it is
	// subtract it from now.
	Since int64 `json:"since"`
}

// ViewersOKBody is the answer to KindViewers.
type ViewersOKBody struct {
	Viewers []ViewerInfo `json:"viewers"`
}

// ClientInfo describes one supplier connected to the server.
//
// Like ViewerInfo it is the server's account: Name comes from the client's
// hello, RemoteAddr is what the server observed, and Aggregate is the server's
// ruling rather than the client's request.
type ClientInfo struct {
	Name       string `json:"name"`
	RemoteAddr string `json:"remote_addr"`
	Since      int64  `json:"since"`
	Aggregate  bool   `json:"aggregate,omitempty"`
}

// ClientsOKBody is the answer to KindClients.
type ClientsOKBody struct {
	Clients []ClientInfo `json:"clients"`
}
