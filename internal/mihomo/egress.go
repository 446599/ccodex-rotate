package mihomo

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"ccodex-rotate/internal/pool"
)

// CollectEvent is one entry in the credential-collection log.
type CollectEvent struct {
	Time  time.Time `json:"time"`
	Model string    `json:"model,omitempty"`
	Msg   string    `json:"msg"`
}

// Egress is the rotating egress used by the reverse proxy. Every connection
// goes through the local mihomo mixed port; node health is discovered lazily
// and remembered, so failed/blocked nodes are skipped on later requests.
type Egress struct {
	m       *Manager
	tr      *http.Transport
	p       *pool.Pool
	mu      sync.Mutex
	current string
	manual  bool // forwarding exit manually pinned
}

// SetNotify installs a callback fired when an astra window is found (hunt
// hit or a collected astra-chain state). It is used for desktop/browser
// notifications. The callback must be non-blocking and must not call back
// into Egress.
// NewEgress builds an Egress for the manager, persisting node state to statePath.
func NewEgress(m *Manager, statePath string) (*Egress, error) {
	u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", m.cfg.MixedPort))
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy: http.ProxyURL(u),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: -1,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true,
		DisableCompression:    true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: headerTimeout(m.cfg.TimeoutSec),
		ExpectContinueTimeout: time.Second,
	}
	return &Egress{m: m, tr: tr, p: pool.New(statePath)}, nil
}

func headerTimeout(sec int) time.Duration {
	if sec <= 0 {
		sec = 120
	}
	return time.Duration(sec) * time.Second
}

// Transport returns the shared transport.
// Transport returns the shared transport.
func (e *Egress) Transport() *http.Transport { return e.tr }

// RefreshNodes reloads the node list from mihomo into the pool.
// RefreshNodes reloads the node list from mihomo into the pool.
func (e *Egress) RefreshNodes(ctx context.Context) error {
	names, err := e.m.NodeNames(ctx)
	if err != nil {
		return err
	}
	e.p.SetNodes(names)
	return nil
}

// Current returns the effective node in use.
// Current returns the effective node in use.
func (e *Egress) Current() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

// pick chooses and selects a node. The state lock is only held while reading
// pool state; the controller call happens WITHOUT the lock so a slow mihomo
// can never wedge the panel or collection.
// pick chooses and selects a node. The state lock is only held while reading
// pool state; the controller call happens WITHOUT the lock so a slow mihomo
// can never wedge the panel or collection.
func (e *Egress) pick(ctx context.Context, exclude, reason string) (string, bool) {
	if exclude != "" && reason != "" {
		e.p.MarkFail(exclude, reason)
	}
	e.mu.Lock()
	if e.manual {
		cur := e.current
		e.mu.Unlock()
		return cur, false
	}
	cur := e.current
	name, ok := e.p.Next(exclude)
	e.mu.Unlock()
	if !ok || name == cur {
		return cur, false
	}
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := e.m.Select(sctx, MainGroup, name); err != nil {
		return cur, false
	}
	e.mu.Lock()
	e.current = name
	e.mu.Unlock()
	return name, true
}

// Init loads nodes and selects an initial node.
// Init loads nodes and selects an initial node.
func (e *Egress) Init(ctx context.Context) error {
	if err := e.RefreshNodes(ctx); err != nil {
		return err
	}
	if e.Current() == "" {
		e.pick(ctx, "", "")
	}
	return nil
}

// Rotate marks the current node as failed and switches to the next usable node.
// When the forwarding exit is manually pinned, rotation is disabled (the pin is
// respected); collection is unaffected either way.
// Rotate marks the current node as failed and switches to the next usable node.
// When the forwarding exit is manually pinned, rotation is disabled (the pin is
// respected); collection is unaffected either way.
func (e *Egress) Rotate(ctx context.Context, reason string) (string, bool) {
	if e.Manual() != "" {
		return e.Current(), false
	}
	return e.pick(ctx, e.Current(), reason)
}

// Success records the current node as working.
// Success records the current node as working.
func (e *Egress) Success() {
	e.p.MarkOK(e.Current(), 0)
}

// EnsureHealthy switches away when the current node is known-failed.
// EnsureHealthy switches away when the current node is known-failed.
func (e *Egress) EnsureHealthy(ctx context.Context) (string, bool) {
	if e.Manual() != "" {
		return e.Current(), false
	}
	cur := e.Current()
	curState := ""
	for _, e2 := range e.p.Snapshot() {
		if e2.Name == cur {
			curState = e2.State
			break
		}
	}
	if cur != "" && curState != pool.Failed {
		return cur, false
	}
	return e.pick(ctx, cur, "")
}

// Reset returns the selector to the fastest member of the AUTO group.
// Reset returns the selector to the fastest member of the AUTO group.
func (e *Egress) Reset(ctx context.Context) error {
	e.p.ClearDegraded()
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := e.m.Select(sctx, MainGroup, AutoGroup); err != nil {
		return err
	}
	e.mu.Lock()
	e.manual = false
	e.current = ""
	e.mu.Unlock()
	if proxies, err := e.m.Proxies(sctx); err == nil {
		if a, ok := proxies[AutoGroup]; ok {
			e.mu.Lock()
			e.current = a.Now
			e.mu.Unlock()
		}
	}
	return nil
}

// PinUser forces a specific forwarding node chosen from the panel.
// PinUser forces a specific forwarding node chosen from the panel.
func (e *Egress) PinUser(ctx context.Context, name string) error {
	return e.Pin(ctx, name)
}

// Pin binds the forwarding exit to name and marks it manual.
// Pin binds the forwarding exit to name and marks it manual.
func (e *Egress) Pin(ctx context.Context, name string) error {
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := e.m.Select(sctx, MainGroup, name); err != nil {
		return err
	}
	e.mu.Lock()
	e.current = name
	e.manual = true
	e.mu.Unlock()
	return nil
}

// Manual reports the manually pinned forwarding exit ("" when automatic).
// Manual reports the manually pinned forwarding exit ("" when automatic).
func (e *Egress) Manual() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.manual {
		return ""
	}
	return e.current
}

// Snapshot returns the node health records.
// Snapshot returns the node health records.
func (e *Egress) Snapshot() []pool.Entry { return e.p.Snapshot() }

// Counts returns (ok, reachable, unknown, failed, total).
// Counts returns (ok, reachable, unknown, failed, total).
func (e *Egress) Counts() (int, int, int, int, int) { return e.p.Counts() }

func (e *Egress) Outcome(requested, served string) {
	if served == "" || requested == "" {
		return
	}
	cur := e.Current()
	if served == requested {
		e.p.MarkClean(cur)
		return
	}
	e.p.MarkDegraded(cur)
}

const versionish = "0.1"
