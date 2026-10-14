// Package server is the hub of the aggregation: it listens for clients, grants
// exactly one of them the aggregate role, and owns the index the CLI queries.
package server

import (
	"errors"
	"sort"
	"sync"
)

// ErrDuplicateName reports that a client name is already connected. Names are
// the index's namespace, so two live clients may not share one.
var ErrDuplicateName = errors.New("server: client name already connected")

// ErrNoSuchClient reports that a name is not currently connected.
var ErrNoSuchClient = errors.New("server: no such client")

// ErrEmptyName reports a registration attempt with no name.
var ErrEmptyName = errors.New("server: client name must not be empty")

// ErrClientOffline reports that a request named a client that is not connected,
// so there is no one to answer it. It is distinct from a failure the owner did
// answer: this one never reached a machine at all.
var ErrClientOffline = errors.New("server: client is not connected")

// ErrNotFound reports that a request named a path the owner does not have.
var ErrNotFound = errors.New("server: not found")

// Registry tracks which clients are connected and which one supplies the
// aggregated tree. The zero value is ready to use.
//
// v1 allows exactly one aggregating client. A later client that also asks to
// aggregate is not rejected: it is downgraded and told so in its hello reply,
// because refusing it outright would make adding a second machine look like a
// failure rather than a configuration the server simply resolves.
type Registry struct {
	mu        sync.Mutex
	live      map[string]bool
	aggregate string
}

// Register records name as connected and reports whether it may aggregate.
//
// It returns ErrDuplicateName if the name is already connected, and
// ErrEmptyName if name is empty — an empty name would occupy the aggregate
// gate while AggregateOwner reported no owner, since "" is the value that
// means "no owner".
//
// When aggregate is requested but another name already holds the role, the
// result is (false, nil): the caller is connected as a plain supplier.
func (r *Registry) Register(name string, aggregate bool) (bool, error) {
	if name == "" {
		return false, ErrEmptyName
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.live[name]; dup {
		return false, ErrDuplicateName
	}
	if r.live == nil {
		r.live = make(map[string]bool)
	}
	r.live[name] = true
	if aggregate && r.aggregate == "" {
		r.aggregate = name
		return true, nil
	}
	return false, nil
}

// Unregister drops name. Unregistering an unknown name is a no-op, so a
// connection teardown path can call it unconditionally.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, name)
	if r.aggregate == name {
		r.aggregate = ""
	}
}

// Live returns the connected client names, ascending.
func (r *Registry) Live() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.live))
	for name := range r.live {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// AggregateOwner returns the name holding the aggregate role.
func (r *Registry) AggregateOwner() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aggregate, r.aggregate != ""
}
