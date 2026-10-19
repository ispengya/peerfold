package server_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ispengya/peerfold/internal/server"
)

func TestRegistryAggregateIsFirstComeOnly(t *testing.T) {
	var r server.Registry

	got, err := r.Register("a", true)
	if err != nil || !got {
		t.Fatalf(`Register("a", true) = (%v, %v), want (true, nil)`, got, err)
	}
	if owner, ok := r.AggregateOwner(); !ok || owner != "a" {
		t.Fatalf(`AggregateOwner() = (%q, %v), want ("a", true)`, owner, ok)
	}

	// A second client asking for aggregate is downgraded, not rejected: it
	// still connects and is still usable, it just does not supply the
	// aggregated tree.
	got, err = r.Register("b", true)
	if err != nil || got {
		t.Fatalf(`Register("b", true) = (%v, %v), want (false, nil)`, got, err)
	}
	if owner, ok := r.AggregateOwner(); !ok || owner != "a" {
		t.Fatalf(`AggregateOwner() after a downgrade = (%q, %v), want ("a", true)`, owner, ok)
	}
}

func TestRegistryRejectsDuplicateName(t *testing.T) {
	var r server.Registry
	if _, err := r.Register("a", true); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if _, err := r.Register("a", false); !errors.Is(err, server.ErrDuplicateName) {
		t.Fatalf("Register with a duplicate name = %v, want ErrDuplicateName", err)
	}
	// The rejected registration must not have disturbed the live set.
	live := r.Live()
	if len(live) != 1 {
		t.Fatalf("Live() = %v, want just [a]", live)
	}
	if _, ok := r.AggregateOwner(); !ok {
		t.Error("a rejected duplicate cleared the aggregate owner")
	}
}

func TestRegistryUnregisterFreesAggregate(t *testing.T) {
	var r server.Registry
	if _, err := r.Register("a", true); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.Unregister("a")
	if owner, ok := r.AggregateOwner(); ok {
		t.Fatalf("AggregateOwner() = %q after unregistering the owner, want none", owner)
	}
	if live := r.Live(); len(live) != 0 {
		t.Fatalf("Live() = %v, want empty", live)
	}

	// With the owner gone, the gate is free again.
	got, err := r.Register("b", true)
	if err != nil || !got {
		t.Fatalf(`Register("b", true) after the owner left = (%v, %v), want (true, nil)`, got, err)
	}
	if owner, _ := r.AggregateOwner(); owner != "b" {
		t.Fatalf("AggregateOwner() = %q, want b", owner)
	}
}

func TestRegistryUnregisterUnknown(t *testing.T) {
	var r server.Registry
	r.Unregister("nobody") // must not panic
	if live := r.Live(); len(live) != 0 {
		t.Fatalf("Live() = %v, want empty", live)
	}
}

// A downgraded client must not take over the gate when it leaves, and
// unregistering it must not clear the real owner.
func TestRegistryUnregisterNonOwnerKeepsOwner(t *testing.T) {
	var r server.Registry
	if _, err := r.Register("a", true); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if _, err := r.Register("b", true); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	r.Unregister("b")
	if owner, ok := r.AggregateOwner(); !ok || owner != "a" {
		t.Fatalf("AggregateOwner() = (%q, %v) after a non-owner left, want (a, true)", owner, ok)
	}
}

func TestRegistryLiveIsSorted(t *testing.T) {
	var r server.Registry
	for _, name := range []string{"win", "bsd", "mac"} {
		if _, err := r.Register(name, false); err != nil {
			t.Fatalf("Register(%q): %v", name, err)
		}
	}
	got := r.Live()
	want := []string{"bsd", "mac", "win"}
	if len(got) != len(want) {
		t.Fatalf("Live() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Live() = %v, want %v (ascending)", got, want)
		}
	}
}

func TestRegistryConcurrentRegister(t *testing.T) {
	var r server.Registry
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Exactly one of these asks for the aggregate role; whichever wins
			// is the only owner.
			if _, err := r.Register(fmt.Sprintf("c%02d", i), i == 0); err != nil {
				t.Errorf("Register(c%02d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	live := r.Live()
	if len(live) != n {
		t.Fatalf("Live() has %d entries, want %d", len(live), n)
	}
	if owner, ok := r.AggregateOwner(); !ok || owner != "c00" {
		t.Fatalf("AggregateOwner() = (%q, %v), want (c00, true)", owner, ok)
	}
}
