package mihomo

import (
	"context"
	"fmt"
	"io"
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
	m             *Manager
	tr            *http.Transport
	p             *pool.Pool
	mu            sync.Mutex
	current       string
	collecting    bool
	lastCollect   time.Time
	lastCollectOK bool
	nextCollect   time.Time
	collectTried  int
	collectTotal  int
	lastSeenLen   int
	lastSeenModel string
	collectLog    []CollectEvent
	stopCollect   context.CancelFunc
	probe         ProbeFunc
	manual        bool // forwarding exit manually pinned; collection ignores it
}

func (e *Egress) logEvent(model, msg string) {
	e.mu.Lock()
	e.collectLog = append(e.collectLog, CollectEvent{Time: time.Now(), Model: model, Msg: msg})
	if len(e.collectLog) > 100 {
		e.collectLog = e.collectLog[len(e.collectLog)-100:]
	}
	e.mu.Unlock()
}

// CollectLog returns the credential-collection log, newest first.
func (e *Egress) CollectLog() []CollectEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]CollectEvent, len(e.collectLog))
	for i, ev := range e.collectLog {
		out[len(e.collectLog)-1-i] = ev
	}
	return out
}

// ProbeFunc performs one collection attempt through the current node and
// reports the state length observed, whether the node was reachable at all,
// and the raw state value when it matched the target length.
type ProbeFunc func(ctx context.Context, client *http.Client, model string) (length int, reachable bool, value string, err error)

// SetProbe installs a collection probe (typically authenticated). When nil, a
// plain unauthenticated reachability probe is used.
func (e *Egress) SetProbe(f ProbeFunc) { e.probe = f }

// Client returns an HTTP client bound to the mihomo mixed port (forwarding).
func (e *Egress) Client(timeout time.Duration) *http.Client {
	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", e.m.cfg.MixedPort))
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:              http.ProxyURL(u),
			DisableKeepAlives:  true,
			DisableCompression: true,
		},
	}
}

// collectClient returns a client bound to the dedicated collection inbound
// (COLLECT group), independent of the forwarding exit.
func (e *Egress) collectClient(timeout time.Duration) *http.Client {
	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", e.m.cfg.CollectPort))
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:              http.ProxyURL(u),
			DisableKeepAlives:  true,
			DisableCompression: true,
		},
	}
}

// targetLengths returns the accepted state lengths.
func (e *Egress) targetLengths() map[int]bool {
	m := map[int]bool{}
	for _, n := range e.m.cfg.StateLengths {
		m[n] = true
	}
	return m
}

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
func (e *Egress) Transport() *http.Transport { return e.tr }

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
func (e *Egress) Current() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

// pickLocked chooses and selects a node.
func (e *Egress) pickLocked(ctx context.Context, exclude, reason string) (string, bool) {
	if exclude != "" && reason != "" {
		e.p.MarkFail(exclude, reason)
	}
	name, ok := e.p.Next(exclude)
	if !ok {
		return e.current, false
	}
	if err := e.m.Select(ctx, MainGroup, name); err != nil {
		return e.current, false
	}
	e.current = name
	return name, true
}

// Init loads nodes and selects an initial node.
func (e *Egress) Init(ctx context.Context) error {
	if err := e.RefreshNodes(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == "" {
		e.pickLocked(ctx, "", "")
	}
	return nil
}

// Rotate marks the current node as failed and switches to the next usable node.
// When the forwarding exit is manually pinned, rotation is disabled (the pin is
// respected); collection is unaffected either way.
func (e *Egress) Rotate(ctx context.Context, reason string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.manual {
		return e.current, false
	}
	cur := e.current
	return e.pickLocked(ctx, cur, reason)
}

// Success records the current node as working.
func (e *Egress) Success() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.p.MarkOK(e.current, 0)
}

// EnsureHealthy switches away when the current node is known-failed.
func (e *Egress) EnsureHealthy(ctx context.Context) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.manual {
		return e.current, false
	}
	var curState string
	for _, e2 := range e.p.Snapshot() {
		if e2.Name == e.current {
			curState = e2.State
			break
		}
	}
	if e.current != "" && curState != pool.Failed {
		return e.current, false
	}
	name, changed := e.pickLocked(ctx, e.current, "")
	return name, changed
}

// Reset returns the selector to the fastest member of the AUTO group.
func (e *Egress) Reset(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.m.Select(ctx, MainGroup, AutoGroup); err != nil {
		return err
	}
	e.manual = false
	e.current = ""
	proxies, err := e.m.Proxies(ctx)
	if err == nil {
		if a, ok := proxies[AutoGroup]; ok {
			e.current = a.Now
		}
	}
	return nil
}

// PinUser forces a specific forwarding node chosen from the panel.
func (e *Egress) PinUser(ctx context.Context, name string) error {
	return e.Pin(ctx, name)
}

// Pin binds the forwarding exit to name and marks it manual.
func (e *Egress) Pin(ctx context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.m.Select(ctx, MainGroup, name); err != nil {
		return err
	}
	e.current = name
	e.manual = true
	return nil
}

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
func (e *Egress) Snapshot() []pool.Entry { return e.p.Snapshot() }

// Counts returns (ok, reachable, unknown, failed, total).
func (e *Egress) Counts() (int, int, int, int, int) { return e.p.Counts() }

// Collect gathers a turn-state by probing nodes one at a time and stops as soon
// as one returns an accepted length (e.g. 292). It never probes all nodes at
// once, to avoid triggering upstream risk-control. The probe caches the value,
// bound to the node that produced it. Returns true on success.
func (e *Egress) Collect(ctx context.Context, model string) bool {
	e.mu.Lock()
	if e.collecting {
		e.mu.Unlock()
		return false
	}
	e.collecting = true
	probe := e.probe
	cctx, cancel := context.WithCancel(ctx)
	e.stopCollect = cancel
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		e.collecting = false
		e.stopCollect = nil
		e.mu.Unlock()
	}()
	ctx = cctx

	if err := e.RefreshNodes(ctx); err != nil {
		e.finishCollect(false)
		return false
	}
	if _, err := e.m.NodeNames(ctx); err != nil {
		e.finishCollect(false)
		return false
	}
	// Candidates in pool order (ok > reachable > unknown), skipping nodes that
	// are still cooling down after a recent failure so the round is not wasted
	// on dead servers.
	var candidates, cooling []string
	for _, ent := range e.p.Snapshot() {
		if ent.State == pool.Failed {
			cooling = append(cooling, ent.Name)
			continue
		}
		candidates = append(candidates, ent.Name)
	}
	if len(candidates) == 0 {
		candidates = cooling
	}
	names := candidates
	e.logEvent(model, fmt.Sprintf("开始采集：候选 %d 个（跳过 %d 个冷却节点）", len(names), len(cooling)))

	e.mu.Lock()
	e.collectTotal = len(names)
	e.collectTried = 0
	e.mu.Unlock()

	timeout := time.Duration(e.m.cfg.ProbeTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	// Collection uses the dedicated COLLECT inbound, never the forwarding exit.
	client := e.collectClient(timeout)
	targets := e.targetLengths()
	if probe == nil {
		probe = e.reachabilityProbe
	}
	max := e.m.cfg.MaxProbesPerCollect
	tried := 0
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		if err := e.m.Select(ctx, CollectGroup, name); err != nil {
			continue
		}
		length, reachable, _, err := probe(ctx, client, model)
		tried++
		e.mu.Lock()
		e.collectTried = tried
		if length > 0 {
			e.lastSeenLen = length
			e.lastSeenModel = model
		}
		e.mu.Unlock()
		switch {
		case err == nil && length > 0 && (len(targets) == 0 || targets[length]):
			e.p.MarkOK(name, 0)
			e.logEvent(model, fmt.Sprintf("采到 %d 字符 @ %s", length, name))
			e.finishCollect(true)
			return true
		case err != nil:
			e.p.MarkFail(name, "error")
		case reachable:
			e.p.MarkReachable(name)
		default:
			e.p.MarkFail(name, "blocked")
		}
		if max > 0 && tried >= max {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	e.finishCollect(false)
	if ctx.Err() != nil {
		e.logEvent(model, "采集已停止")
	} else {
		e.logEvent(model, "本轮未采到合格凭据")
	}
	return false
}

// StopCollect interrupts an in-progress collection round.
func (e *Egress) StopCollect() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopCollect == nil {
		return false
	}
	e.stopCollect()
	return true
}

// preferOK orders candidates so nodes already known to yield the target state
// are tried first.
func (e *Egress) preferOK(names []string) []string {
	ok := map[string]bool{}
	for _, ent := range e.p.Snapshot() {
		if ent.State == pool.OK {
			ok[ent.Name] = true
		}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if ok[n] {
			out = append(out, n)
		}
	}
	for _, n := range names {
		if !ok[n] {
			out = append(out, n)
		}
	}
	return out
}

func (e *Egress) finishCollect(ok bool) {
	interval := e.m.cfg.CollectRetryIntervalSec
	if ok {
		interval = e.m.cfg.CollectSuccessIntervalSec
	}
	e.mu.Lock()
	e.lastCollect = time.Now()
	e.lastCollectOK = ok
	e.nextCollect = e.lastCollect.Add(time.Duration(interval) * time.Second)
	e.mu.Unlock()
}

// CollectInfo returns (lastCollect, lastOK, nextCollect, collecting).
func (e *Egress) CollectInfo() (time.Time, bool, time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastCollect, e.lastCollectOK, e.nextCollect, e.collecting
}

// CollectProgress returns how many nodes have been tried out of the total in
// the current collection round.
func (e *Egress) CollectProgress() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.collectTried, e.collectTotal
}

// LastSeen returns the most recent turn-state length observed (even when it was
// not an accepted length) and the model it was observed for.
func (e *Egress) LastSeen() (string, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastSeenModel, e.lastSeenLen
}

// reachabilityProbe is the unauthenticated fallback probe.
func (e *Egress) reachabilityProbe(ctx context.Context, client *http.Client, model string) (int, bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.m.cfg.HealthURL, nil)
	if err != nil {
		return 0, false, "", err
	}
	req.Header.Set("User-Agent", "ccodex-rotate/"+versionish)
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, "", err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		return 0, false, "", nil
	}
	return 0, true, "", nil
}

const versionish = "0.1"
