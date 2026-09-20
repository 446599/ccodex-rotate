// Package turnstate caches the upstream X-Codex-Turn-State value so it can be
// re-injected on later requests. Values are bound to the node that produced
// them, so a state is only ever used through the same exit (no cross-exit
// reuse, which is what caused upstream 502s).
package turnstate

import (
	"sync"
	"time"
)

// Entry is one cached turn-state value.
type Entry struct {
	Value   string    `json:"value"`
	Node    string    `json:"node"`
	Model   string    `json:"model"`
	Account string    `json:"account"`
	Source  string    `json:"source"` // "traffic" (real session) or "probe"
	Length  int       `json:"length"`
	Created time.Time `json:"created"`
	Hits    int       `json:"hits"`
}

// Store is a concurrency-safe cache keyed by account+model.
type Store struct {
	mu      sync.Mutex
	entries map[string]*Entry
	ttl     time.Duration
}

// New creates a store with the given TTL.
func New(ttl time.Duration) *Store {
	return &Store{entries: map[string]*Entry{}, ttl: ttl}
}

func key(account, model string) string { return account + "\x00" + model }

// Get returns a live entry for account+model.
func (s *Store) Get(account, model string) (*Entry, bool) {
	if model == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key(account, model)]
	if !ok {
		return nil, false
	}
	if s.ttl > 0 && time.Since(e.Created) > s.ttl {
		delete(s.entries, key(account, model))
		return nil, false
	}
	return e, true
}

// Put stores a freshly observed value for account+model on node. A state learned
// from the live session ("traffic") is never replaced by a probe-derived one, so
// background collection cannot break an ongoing session.
func (s *Store) Put(account, model, node, value, source string) {
	if value == "" || model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(account, model)
	if old, ok := s.entries[k]; ok && old.Source == "traffic" && source == "probe" {
		return
	}
	s.entries[k] = &Entry{
		Value:   value,
		Node:    node,
		Model:   model,
		Account: account,
		Source:  source,
		Length:  len(value),
		Created: time.Now(),
	}
}

// Hit increments the injection counter of the current entry.
func (s *Store) Hit(account, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key(account, model)]; ok {
		e.Hits++
	}
}

// Snapshot returns all live entries.
func (s *Store) Snapshot() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]Entry, 0, len(s.entries))
	for k, e := range s.entries {
		if s.ttl > 0 && now.Sub(e.Created) > s.ttl {
			delete(s.entries, k)
			continue
		}
		out = append(out, *e)
	}
	return out
}
