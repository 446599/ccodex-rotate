package pool

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSelectionPrefersOKAndSkipsFailed(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.SetNodes([]string{"a", "b", "c"})

	// No OK nodes yet: it should hand out unknown nodes round-robin.
	n1, ok := p.Next("")
	if !ok || n1 == "" {
		t.Fatal("expected a node")
	}
	p.MarkOK(n1, 10)

	n2, ok := p.Next(n1)
	if !ok || n2 == n1 {
		t.Fatalf("expected a different node, got %q", n2)
	}
	p.MarkFail(n2, "blocked")

	// With a and c usable, b must not be returned while cooling down.
	for i := 0; i < 5; i++ {
		n, ok := p.Next("")
		if !ok {
			t.Fatal("expected node")
		}
		if n == n2 {
			t.Fatalf("failed node %q was selected", n2)
		}
	}
}

func TestOKPreferredOverUnknown(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.SetNodes([]string{"a", "b", "c"})
	p.MarkOK("c", 5)
	n, _ := p.Next("")
	if n != "c" {
		t.Fatalf("expected OK node c, got %q", n)
	}
}

func TestBlockedCooldownExpires(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.SetNodes([]string{"only"})
	p.blockTTL = 10 * time.Millisecond
	p.MarkFail("only", "blocked")
	// While cooling down, Next still returns a fallback rather than nothing.
	if _, ok := p.Next(""); !ok {
		t.Fatal("expected a fallback node while cooling down")
	}
	time.Sleep(40 * time.Millisecond)
	_, _, _, failed, total := p.Counts()
	if total != 1 || failed != 0 {
		t.Fatalf("cooldown should have expired: failed=%d total=%d", failed, total)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := New(path)
	p.SetNodes([]string{"a", "b"})
	p.MarkOK("a", 12)
	p.MarkFail("b", "error")

	p2 := New(path)
	snap := p2.Snapshot()
	if len(snap) != 2 || snap[0].Name != "a" || snap[0].State != OK {
		t.Fatalf("persistence failed: %+v", snap)
	}
}
