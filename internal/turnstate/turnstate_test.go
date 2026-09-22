package turnstate

import (
	"testing"
	"time"
)

func TestPutFullStoresCookies(t *testing.T) {
	s := New(time.Hour)
	s.PutFull("a", "m", "n1", "value292", []string{"__cflb=x", "__oailb=y"})
	e, ok := s.Get("a", "m")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.CookieCount != 2 || len(e.Cookies) != 2 {
		t.Fatalf("cookies not stored: %+v", e)
	}
	if e.Cookies[0] != "__cflb=x" {
		t.Fatalf("wrong cookie: %q", e.Cookies[0])
	}
}

func TestFreshWindow(t *testing.T) {
	s := New(time.Hour)
	s.PutFull("a", "m", "n1", "value292", []string{"c=v"})
	if _, ok := s.Fresh("a", "m", 240*time.Second); !ok {
		t.Fatal("fresh bundle should be fresh")
	}
	// Simulate an aged bundle by backdating Created.
	s.mu.Lock()
	s.entries[key("a", "m")].Created = time.Now().Add(-300 * time.Second)
	s.mu.Unlock()
	if _, ok := s.Fresh("a", "m", 240*time.Second); ok {
		t.Fatal("aged bundle must not be fresh")
	}
	// Aged bundle still visible via Get (turn-state injection continues).
	if _, ok := s.Get("a", "m"); !ok {
		t.Fatal("aged bundle should still be returned by Get")
	}
}

func TestPutRotatesBundle(t *testing.T) {
	s := New(time.Hour)
	s.PutFull("a", "m", "n1", "old", []string{"c=1"})
	s.PutFull("a", "m", "n2", "new", []string{"c=2"})
	e, ok := s.Get("a", "m")
	if !ok || e.Value != "new" || e.Node != "n2" {
		t.Fatalf("bundle not rotated: %+v", e)
	}
	if len(e.Cookies) != 1 || e.Cookies[0] != "c=2" {
		t.Fatalf("cookies not rotated: %+v", e.Cookies)
	}
}

func TestDropVoidsBundle(t *testing.T) {
	s := New(time.Hour)
	s.PutFull("a", "m", "n1", "value292", []string{"c=v"})
	if !s.Exists("a", "m") {
		t.Fatal("expected entry to exist")
	}
	s.Drop("a", "m")
	if s.Exists("a", "m") {
		t.Fatal("dropped entry must not exist")
	}
	if _, ok := s.Get("a", "m"); ok {
		t.Fatal("dropped entry must not be returned")
	}
	// Dropping a missing entry is a no-op.
	s.Drop("a", "m")
}
