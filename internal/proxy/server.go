// Package proxy implements a streaming reverse proxy for the Codex backend with
// health-based node failover. It deliberately does not inject or collect any
// upstream "turn state": every request is forwarded verbatim through whichever
// egress node is currently healthy.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	// Outcome records whether the current node served the requested model.
	Outcome(requested, served string)
	// Pin binds the current node to name (used to keep turn-state on one exit).
	Pin(ctx context.Context, name string) error
}

// Record is one handled request, kept for the panel.
type Record struct {
	Time        time.Time `json:"time"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Status      int       `json:"status"`
	Node        string    `json:"node"`
	Model       string    `json:"model,omitempty"`
	ServedModel string    `json:"served_model,omitempty"`
	Quota       string    `json:"quota,omitempty"`
	Attempts    int       `json:"attempts"`
	Millis      int64     `json:"millis"`
}

// prefixCapture keeps the first N bytes written through it.
type prefixCapture struct {
	buf   []byte
	limit int
}

func (p *prefixCapture) Write(b []byte) (int, error) {
	if room := p.limit - len(p.buf); room > 0 {
		if len(b) <= room {
			p.buf = append(p.buf, b...)
		} else {
			p.buf = append(p.buf, b[:room]...)
		}
	}
	return len(b), nil
}

// Server is the local reverse proxy: transparent forwarding with
// health-based failover plus behavioral degradation challenges. It keeps no
// turn-state and injects nothing.
type Server struct {
	cfg    config.Config
	eg     Egress
	logf   func(string, ...any)
	recent *ring
	models *modelid.Resolver
	force  atomic.Value // string: forced model ("" = off)

	reqCount int64
	errCount int64
	mu       sync.Mutex

	authMu    sync.Mutex
	auth      string
	account   string
	lastModel string
}

// cookieJar isolates cookies by the same account + canonical model key as turn-state.
// New builds a Server.
func New(cfg config.Config, eg Egress, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{cfg: cfg, eg: eg, logf: logf, recent: newRing(50), models: modelid.New(cfg.ModelAliases)}
	s.force.Store(cfg.ForceModel)
	return s
}

// CanonicalModel maps a requested model name to its canonical identifier.
func (s *Server) CanonicalModel(name string) string {
	if s.models == nil {
		return name
	}
	return s.models.Canonical(name)
}

// ForceModel returns the currently forced model ("" = off).
func (s *Server) ForceModel() string {
	v, _ := s.force.Load().(string)
	return v
}

// SetForceModel sets (or clears) the forced model at runtime.
func (s *Server) SetForceModel(name string) { s.force.Store(name) }

// Challenge sends one ModelTrace-style attribution prompt through the
// current forwarding exit (what the user actually gets) and returns the
// model's text output. The endpoint requires streaming: text deltas are
// accumulated from the SSE stream.
func (s *Server) Challenge(ctx context.Context, model, prompt string) (string, error) {
	s.authMu.Lock()
	auth, account := s.auth, s.account
	s.authMu.Unlock()
	if auth == "" {
		return "", fmt.Errorf("no account auth observed yet")
	}
	if model == "" {
		model = s.cfg.ProbeModel
	}
	body := `{"model":` + strconv.Quote(model) +
		`,"instructions":"You are a helper. Complete the task directly.",` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` +
		strconv.Quote(prompt) + `}]}],` +
		`"stream":true,"store":false,"reasoning":{"effort":"medium"},` +
		`"tools":[],"parallel_tool_calls":false}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.UpstreamBase+"/backend-api/codex/responses", strings.NewReader(body))
	if err != nil {
		return "", err
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
	if s.eg == nil {
		return "", fmt.Errorf("no egress")
	}
	client := &http.Client{Timeout: 300 * time.Second, Transport: s.eg.Transport()}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return "", fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return accumulateStreamText(resp.Body)
}

// accumulateStreamText collects output text from a Responses SSE stream.
func accumulateStreamText(r io.Reader) (string, error) {
	var sb strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if d, ok := ev["delta"].(string); ok {
			sb.WriteString(d)
			continue
		}
		if t, ok := ev["text"].(string); ok {
			sb.WriteString(t)
		}
	}
	if err := sc.Err(); err != nil {
		return sb.String(), err
	}
	return sb.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
	// Optionally force the model so every request uses it.
	if fm := s.ForceModel(); fm != "" {
		if nb := rewriteModelField(body, fm); nb != nil {
			body = nb
			model = s.models.Canonical(fm)
		}
	}

	// Remember the account auth so behavioral challenges can reuse it.
	if a := r.Header.Get("Authorization"); a != "" {
		s.authMu.Lock()
		s.auth = a
		if account != "" {
			s.account = account
		}
		if model != "" {
			s.lastModel = model
		}
		s.authMu.Unlock()
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
		// Record the served model for degradation marking (passive only).
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
		cap := &prefixCapture{limit: 1 << 15}
		_, copyErr := copyStream(w, io.TeeReader(resp.Body, cap))
		resp.Body.Close()
		served := extractModel(cap.buf)
		// Learn whether this exit served the requested model (marks the
		// node degraded in the panel; never auto-switches).
		if r.Method == http.MethodPost {
			s.eg.Outcome(model, served)
		}
		s.record(Record{
			Time: time.Now(), Method: r.Method, Path: r.URL.Path,
			Status: resp.StatusCode, Node: s.eg.Current(), Model: model,
			ServedModel: served,
			Quota:       resp.Header.Get("x-codex-primary-used-percent"),
			Attempts:    attempts,
			Millis:      time.Since(start).Milliseconds(),
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

// rewriteModelField replaces the value of the first "model" field in a JSON body
// without touching anything else. Returns nil if no model field was found.
func rewriteModelField(b []byte, newModel string) []byte {
	const k = `"model"`
	i := bytes.Index(b, []byte(k))
	if i < 0 {
		return nil
	}
	rest := b[i+len(k):]
	j := 0
	for j < len(rest) && (rest[j] == ' ' || rest[j] == ':' || rest[j] == '\t' || rest[j] == '\n' || rest[j] == '\r') {
		j++
	}
	if j >= len(rest) || rest[j] != '"' {
		return nil
	}
	start := i + len(k) + j + 1
	endRel := bytes.IndexByte(b[start:], '"')
	if endRel < 0 {
		return nil
	}
	end := start + endRel
	out := make([]byte, 0, len(b)-((end)-(start))+len(newModel))
	out = append(out, b[:start]...)
	out = append(out, newModel...)
	out = append(out, b[end:]...)
	return out
}

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
