// Package pool tracks per-node health discovered lazily at request time, so the
// tool never depends on mihomo's flaky provider health-check. Failed nodes are
// cooled down and skipped; working nodes are preferred.
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// State of a node.
const (
	Unknown   = "unknown"
	Reachable = "reachable"
	OK        = "ok"
	Failed    = "failed"
)

// Entry is the persisted health record of one node.
type Entry struct {
	Name      string    `json:"name"`
	Delay     int       `json:"delay"`
	State     string    `json:"state"`
	FailUntil time.Time `json:"fail_until"`
	LastOK    time.Time `json:"last_ok"`
	Reason    string    `json:"reason,omitempty"`
}

// Pool holds node state.
type Pool struct {
	mu       sync.Mutex
	entries  map[string]*Entry
	order    []string
	path     string
	failTTL  time.Duration
	blockTTL time.Duration
}

// New creates a pool persisting to path.
func New(path string) *Pool {
	p := &Pool{
		entries:  map[string]*Entry{},
		path:     path,
		failTTL:  90 * time.Second,
		blockTTL: 300 * time.Second,
	}
	p.load()
	return p
}

func (p *Pool) load() {
	b, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var data struct {
		Entries []*Entry `json:"entries"`
	}
	if json.Unmarshal(b, &data) != nil {
		return
	}
	for _, e := range data.Entries {
		p.entries[e.Name] = e
	}
	for name := range p.entries {
		p.order = append(p.order, name)
	}
	sort.Strings(p.order)
}

func (p *Pool) saveLocked() {
	entries := make([]*Entry, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	b, _ := json.MarshalIndent(map[string]any{"entries": entries}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p.path)
	}
}

// SetNodes updates the node list, preserving existing records.
func (p *Pool) SetNodes(names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	p.order = p.order[:0]
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		p.order = append(p.order, n)
		if _, ok := p.entries[n]; !ok {
			p.entries[n] = &Entry{Name: n, State: Unknown}
		}
	}
	p.saveLocked()
}

// MarkOK records a successful use.
func (p *Pool) MarkOK(name string, delay int) {
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.ensure(name)
	e.State = OK
	e.Delay = delay
	e.LastOK = time.Now()
	e.FailUntil = time.Time{}
	e.Reason = ""
	p.saveLocked()
}

// MarkReachable records a node that answers but does not yield the target state.
func (p *Pool) MarkReachable(name string) {
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.ensure(name)
	if e.State != OK {
		e.State = Reachable
	}
	e.FailUntil = time.Time{}
	e.Reason = ""
	p.saveLocked()
}

// MarkFail records a failure and starts a cooldown.
func (p *Pool) MarkFail(name, reason string) {
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.ensure(name)
	e.State = Failed
	e.Reason = reason
	ttl := p.failTTL
	if reason == "blocked" {
		ttl = p.blockTTL
	}
	e.FailUntil = time.Now().Add(ttl)
	p.saveLocked()
}

func (p *Pool) ensure(name string) *Entry {
	if e, ok := p.entries[name]; ok {
		return e
	}
	e := &Entry{Name: name, State: Unknown}
	p.entries[name] = e
	p.order = append(p.order, name)
	return e
}

func (p *Pool) usable(e *Entry) bool {
	if e.State == Failed && time.Now().Before(e.FailUntil) {
		return false
	}
	return true
}

// Next picks the best node to use, excluding exclude. It prefers known-good
// nodes, then untested ones, and only reuses cooled-down failures as a last
// resort. It rotates round-robin within each tier.
func (p *Pool) Next(exclude string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.order) == 0 {
		return "", false
	}
	start := 0
	for i, n := range p.order {
		if n == exclude {
			start = (i + 1) % len(p.order)
			break
		}
	}
	pick := func(tier func(*Entry) bool) string {
		for i := 0; i < len(p.order); i++ {
			n := p.order[(start+i)%len(p.order)]
			if n == exclude {
				continue
			}
			if tier(p.entries[n]) {
				return n
			}
		}
		return ""
	}
	if n := pick(func(e *Entry) bool { return e.State == OK }); n != "" {
		return n, true
	}
	if n := pick(func(e *Entry) bool { return e.State == Reachable && p.usable(e) }); n != "" {
		return n, true
	}
	if n := pick(func(e *Entry) bool { return e.State == Unknown && p.usable(e) }); n != "" {
		return n, true
	}
	if n := pick(func(e *Entry) bool { return p.usable(e) }); n != "" {
		return n, true
	}
	// everything is cooling down: clear cooldowns and try anything.
	for _, n := range p.order {
		if n != exclude {
			return n, true
		}
	}
	return "", false
}

// Snapshot returns a copy of all entries, best first.
func (p *Pool) Snapshot() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Entry, 0, len(p.order))
	now := time.Now()
	for _, n := range p.order {
		e := *p.entries[n]
		if e.State == Failed && !now.Before(e.FailUntil) {
			e.State = Unknown
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// Counts returns (ok, reachable, unknown, failed, total).
func (p *Pool) Counts() (int, int, int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ok, reach, un, bad := 0, 0, 0, 0
	now := time.Now()
	for _, e := range p.entries {
		switch {
		case e.State == OK:
			ok++
		case e.State == Reachable:
			reach++
		case e.State == Failed && now.Before(e.FailUntil):
			bad++
		default:
			un++
		}
	}
	return ok, reach, un, bad, len(p.order)
}

func rank(e Entry) int {
	switch e.State {
	case OK:
		return 0
	case Reachable:
		return 1
	case Unknown:
		return 2
	default:
		return 3
	}
}
