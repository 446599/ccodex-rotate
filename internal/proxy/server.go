// Package proxy implements a streaming reverse proxy for the Codex backend with
// health-based node failover. It deliberately does not inject or collect any
// upstream "turn state": every request is forwarded verbatim through whichever
// egress node is currently healthy.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccodex-rotate/internal/config"
	"ccodex-rotate/internal/modelid"
	"ccodex-rotate/internal/turnstate"
)

// Egress abstracts the rotating outbound path.
type Egress interface {
	// Transport returns the HTTP transport bound to the current node.
	Transport() *http.Transport
	// Current returns the node currently in use.
	Current() string
	// Rotate marks the current node failed and switches to another usable one.
	Rotate(ctx context.Context, reason string) (string, bool)
	// Success records that the current node worked.
	Success()
	// Pin binds the current node to name (used to keep turn-state on one exit).
	Pin(ctx context.Context, name string) error
}

// Record is one handled request, kept for the panel.
type Record struct {
	Time     time.Time `json:"time"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Status   int       `json:"status"`
	Node     string    `json:"node"`
	Model    string    `json:"model,omitempty"`
	Injected bool      `json:"injected"`
	Attempts int       `json:"attempts"`
	Millis   int64     `json:"millis"`
}

// Server is the local reverse proxy.
type Server struct {
	cfg     config.Config
	eg      Egress
	logf    func(string, ...any)
	recent  *ring
	state   *turnstate.Store
	lengths map[int]bool
	models  *modelid.Resolver
	inject  atomic.Bool

	reqCount int64
	errCount int64
	mu       sync.Mutex

	authMu    sync.Mutex
	auth      string
	account   string
	lastModel string
	onAuth    func()
	authOnce  sync.Once

	needMu     sync.Mutex
	lastNeed   map[string]time.Time
	onNeed     func(model string)
	collectSet map[string]bool
}

// SetOnAuth registers a callback fired once, the first time account auth is
// observed. It is used to kick off a full node collection scan immediately.
func (s *Server) SetOnAuth(f func()) { s.onAuth = f }

// SetOnNeedState registers a callback fired when a request arrives for a model
// that has no cached turn-state yet, so collection can start right away.
func (s *Server) SetOnNeedState(f func(model string)) { s.onNeed = f }

// SetCollectModels restricts on-demand collection to the given canonical
// models. When empty, only cfg.ProbeModel triggers collection.
func (s *Server) SetCollectModels(models []string) {
	set := map[string]bool{}
	base := append([]string{s.cfg.ProbeModel}, models...)
	for _, m := range base {
		if c := s.models.Canonical(m); c != "" {
			set[c] = true
		}
	}
	s.collectSet = set
}

func (s *Server) triggerNeedState(model string) {
	if s.onNeed == nil {
		return
	}
	if len(s.collectSet) > 0 && !s.collectSet[model] {
		return
	}
	s.needMu.Lock()
	if s.lastNeed == nil {
		s.lastNeed = map[string]time.Time{}
	}
	if t, ok := s.lastNeed[model]; ok && time.Since(t) < 30*time.Second {
		s.needMu.Unlock()
		return
	}
	s.lastNeed[model] = time.Now()
	s.needMu.Unlock()
	s.onNeed(model)
}

// New builds a Server.
func New(cfg config.Config, eg Egress, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{cfg: cfg, eg: eg, logf: logf, recent: newRing(50), models: modelid.New(cfg.ModelAliases)}
	// The state store is always created so injection can be toggled at runtime.
	s.state = turnstate.New(time.Duration(cfg.StateTTLSeconds) * time.Second)
	if len(cfg.StateLengths) > 0 {
		s.lengths = map[int]bool{}
		for _, n := range cfg.StateLengths {
			s.lengths[n] = true
		}
	}
	s.inject.Store(cfg.InjectState)
	return s
}

// SetInjection enables or disables credential injection at runtime.
func (s *Server) SetInjection(on bool) { s.inject.Store(on) }

// InjectionEnabled reports whether credential injection is on.
func (s *Server) InjectionEnabled() bool { return s.inject.Load() }

// StateTTLSeconds is the credential validity window.
func (s *Server) StateTTLSeconds() int { return s.cfg.StateTTLSeconds }

// StateSnapshot returns the cached turn-state entries for the panel.
func (s *Server) StateSnapshot() []turnstate.Entry {
	if s.state == nil {
		return nil
	}
	return s.state.Snapshot()
}

// HasValidState reports whether a usable turn-state is cached for model.
func (s *Server) HasValidState(model string) bool {
	if s.state == nil {
		return false
	}
	m := s.models.Canonical(model)
	if m == "" {
		return false
	}
	s.authMu.Lock()
	account := s.account
	s.authMu.Unlock()
	_, ok := s.state.Get(account, m)
	return ok
}

// HasValidStateForProbe reports whether a usable (target-length) turn-state is
// already cached for the collection model, so collection can be skipped.
func (s *Server) HasValidStateForProbe() bool { return s.HasValidState(s.cfg.ProbeModel) }

// HasAuth reports whether account credentials have been observed yet. Collection
// must not start before a real request supplies them.
func (s *Server) HasAuth() bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.auth != ""
}

// Probe performs one state-collection attempt through the current node. It uses
// the account auth captured from real traffic; without it, it just checks
// reachability (length 0). When a value of an accepted length is returned it is
// cached (bound to the current node) for later injection.
func (s *Server) Probe(ctx context.Context, client *http.Client, probeModel string) (int, bool, string, error) {
	s.authMu.Lock()
	auth, account := s.auth, s.account
	s.authMu.Unlock()
	model := s.models.Canonical(probeModel)
	if model == "" {
		model = s.models.Canonical(s.cfg.ProbeModel)
	}
	if model == "" {
		model = s.cfg.ProbeModel
	}

	if auth == "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.HealthURL, nil)
		if err != nil {
			return 0, false, "", err
		}
		req.Header.Set("User-Agent", "ccodex-rotate/probe")
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

	body := probeBody(model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.UpstreamBase+"/backend-api/codex/responses", strings.NewReader(body))
	if err != nil {
		return 0, false, "", err
	}
	req.Header.Set("Authorization", auth)
	if account != "" {
		req.Header.Set("chatgpt-account-id", account)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, "", err
	}
	value := resp.Header.Get("X-Codex-Turn-State")
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		return 0, false, "", nil
	}
	if value == "" {
		return 0, true, "", nil
	}
	length := len(value)
	if s.state != nil && s.lengthAllowed(length) {
		node := ""
		if s.eg != nil {
			node = s.eg.Current()
		}
		s.state.Put(account, model, node, value)
	}
	return length, true, value, nil
}

func probeBody(model string) string {
	return `{"model":"` + model + `","instructions":"You are a helper.",` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"stream":true,"store":false,"reasoning":{"effort":"low"},` +
		`"tools":[],"parallel_tool_calls":false}`
}

func (s *Server) lengthAllowed(n int) bool {
	if s.lengths == nil {
		return true
	}
	return s.lengths[n]
}

// Stats returns counters and the recent request log.
func (s *Server) Stats() (int64, int64, []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqCount, s.errCount, s.recent.snapshot()
}

func (s *Server) record(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqCount++
	if r.Status >= 400 {
		s.errCount++
	}
	s.recent.add(r)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.URL.Path == "/" || r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ccodex-rotate ok\nnode: %s\n", s.eg.Current())
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/backend-api/codex") {
		http.NotFound(w, r)
		return
	}

	body, replayable, err := readBody(r, int64(s.cfg.MaxBodyMiB)<<20)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	target := s.cfg.UpstreamBase + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	account := strings.TrimSpace(r.Header.Get("chatgpt-account-id"))
	model := s.models.Canonical(extractModel(body))

	// Remember the account auth so node scans can collect a turn-state.
	if a := r.Header.Get("Authorization"); a != "" {
		s.authMu.Lock()
		first := s.auth == ""
		s.auth = a
		if account != "" {
			s.account = account
		}
		if model != "" {
			s.lastModel = model
		}
		s.authMu.Unlock()
		if first && s.onAuth != nil {
			s.authOnce.Do(s.onAuth)
		}
		// If this model has no cached 292 yet, start collecting immediately.
		if s.state != nil && model != "" {
			if _, ok := s.state.Get(account, model); !ok {
				s.triggerNeedState(model)
			}
		}
	}

	maxAttempts := s.cfg.MaxRetries + 1
	if !replayable {
		maxAttempts = 1
	}
	// No total deadline: Codex responses stream for a long time and a hard
	// request timeout truncates them ("stream closed before response.completed").
	// The client's context cancels on disconnect; the transport's
	// ResponseHeaderTimeout bounds failover attempts instead.
	ctx := r.Context()

	var lastErr error
	attempts := 0
	injectedAny := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		attempts = attempt + 1
		req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		copyHeaders(req.Header, r.Header)
		req.Host = "" // let net/http set from URL
		req.ContentLength = int64(len(body))

		// Inject a cached turn-state (can be toggled off at runtime).
		if s.inject.Load() && s.state != nil && model != "" {
			if e, ok := s.state.Get(account, model); ok && s.lengthAllowed(e.Length) {
				inject := true
				if s.cfg.InjectNodeAffinity && e.Node != "" && e.Node != s.eg.Current() {
					inject = s.eg.Pin(ctx, e.Node) == nil
				}
				if inject {
					req.Header.Set("X-Codex-Turn-State", e.Value)
					s.state.Hit(account, model)
					injectedAny = true
				}
			}
		}

		tr := s.eg.Transport()
		resp, err := tr.RoundTrip(req)
		if err != nil {
			lastErr = err
			if attempt+1 < maxAttempts && retriableErr(err) {
				failedOn := s.eg.Current()
				node, _ := s.eg.Rotate(ctx, "error")
				s.logf("retry %s %s: %v on %s -> %s", r.Method, r.URL.Path, err, failedOn, node)
				continue
			}
			break
		}
		// Learn the turn-state the upstream just issued, but only keep a value
		// of an accepted length (otherwise a wrong-length value would suppress
		// both collection and injection).
		if s.state != nil && model != "" {
			if v := resp.Header.Get("X-Codex-Turn-State"); v != "" && s.lengthAllowed(len(v)) {
				s.state.Put(account, model, s.eg.Current(), v)
			}
		}
		if retriableStatus(resp.StatusCode) && attempt+1 < maxAttempts {
			reason := "error"
			if resp.StatusCode == http.StatusForbidden {
				reason = "blocked"
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			s.logf("retry %s %s after upstream %d on %s", r.Method, r.URL.Path, resp.StatusCode, s.eg.Current())
			s.eg.Rotate(ctx, reason)
			continue
		}
		// Deliver response.
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		s.eg.Success()
		_, copyErr := copyStream(w, resp.Body)
		resp.Body.Close()
		s.record(Record{
			Time: time.Now(), Method: r.Method, Path: r.URL.Path,
			Status: resp.StatusCode, Node: s.eg.Current(), Model: model,
			Injected: injectedAny, Attempts: attempts,
			Millis: time.Since(start).Milliseconds(),
		})
		if copyErr != nil {
			s.logf("stream %s: %v", r.URL.Path, copyErr)
			// A mid-stream break is usually the exit dropping the connection;
			// mark it so later requests avoid it (unless manually pinned, which
			// Rotate respects).
			if r.Context().Err() == nil {
				if node, changed := s.eg.Rotate(ctx, "stream_error"); changed {
					s.logf("egress switched after stream error: %s", node)
				}
			}
		}
		return
	}

	status := http.StatusBadGateway
	msg := "upstream connection failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	s.record(Record{
		Time: time.Now(), Method: r.Method, Path: r.URL.Path,
		Status: status, Node: s.eg.Current(), Attempts: attempts,
		Millis: time.Since(start).Milliseconds(),
	})
	s.logf("fail %s %s: %s", r.Method, r.URL.Path, msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"ccodex_rotate_error"}}`, msg)
}

// readBody buffers the request body so it can be replayed on failover.
func readBody(r *http.Request, cap int64) ([]byte, bool, error) {
	if r.Body == nil {
		return nil, true, nil
	}
	defer r.Body.Close()
	if cap <= 0 {
		cap = 1 << 30
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r.Body, cap+1))
	if err != nil {
		return nil, false, err
	}
	if n > cap {
		return nil, false, fmt.Errorf("body exceeds %d bytes", cap)
	}
	return buf.Bytes(), true, nil
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHop(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isHop(k string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

func retriableStatus(code int) bool {
	switch code {
	case http.StatusForbidden, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retriableErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// Could be mid-flight; avoid replaying a possibly billed request.
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"connection refused", "connection reset", "broken pipe",
		"proxyconnect", "no route to host", "network is unreachable",
		"eof", "cannot assign requested address", "connect: ",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// copyStream flushes each chunk so Server-Sent Events arrive immediately.
func copyStream(w http.ResponseWriter, r io.Reader) (int64, error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

// ring is a tiny fixed-size circular buffer for recent requests.
type ring struct {
	mu   sync.Mutex
	data []Record
	max  int
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) add(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, rec)
	if len(r.data) > r.max {
		r.data = r.data[len(r.data)-r.max:]
	}
}

func (r *ring) snapshot() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, len(r.data))
	copy(out, r.data)
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// parseInt is a small helper shared by callers parsing config strings.
func parseInt(s string) int { n, _ := strconv.Atoi(s); return n }

// extractModel pulls the "model" field out of a JSON request body without a
// full parse (bodies may be large or compressed).
func extractModel(b []byte) string {
	const k = `"model"`
	i := bytes.Index(b, []byte(k))
	if i < 0 {
		return ""
	}
	rest := b[i+len(k):]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == ':' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}
