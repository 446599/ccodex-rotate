// Package sim contains an end-to-end simulation of ccodex-rotate against a mock
// upstream and a set of mock proxy nodes (blocked / dead / healthy).
package sim

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"ccodex-rotate/internal/config"
	"ccodex-rotate/internal/proxy"
)

type node struct {
	name     string
	behavior string // blocked | dead502 | deadconn | healthy
	proxy    *httptest.Server
	tr       *http.Transport
	attempts int
}

type simEgress struct {
	mu    sync.Mutex
	nodes []*node
	cur   int
}

func (e *simEgress) Transport() *http.Transport {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodes[e.cur].tr
}

func (e *simEgress) Current() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodes[e.cur].name
}

func (e *simEgress) Rotate(ctx context.Context, reason string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cur = (e.cur + 1) % len(e.nodes)
	return e.nodes[e.cur].name, true
}

func (e *simEgress) Success() {}

func (e *simEgress) Pin(ctx context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, n := range e.nodes {
		if n.name == name {
			e.cur = i
			return nil
		}
	}
	return nil
}

func (e *simEgress) order() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.nodes))
	for i, n := range e.nodes {
		out[i] = n.name
	}
	return out
}

// newMockUpstream returns an SSE upstream that records the X-Via node header.
func newMockUpstream() (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var vias []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		via := r.Header.Get("X-Via")
		mu.Lock()
		vias = append(vias, via)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, "data: via="+via+"\n\n")
		fl.Flush()
		io.WriteString(w, "data: done\n\n")
		fl.Flush()
	}))
	return up, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), vias...) }
}

// newMockNode builds one fake proxy node in front of the upstream.
func newMockNode(name, behavior, upstreamURL string) *node {
	n := &node{name: name, behavior: behavior}
	n.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch behavior {
		case "blocked":
			w.WriteHeader(http.StatusForbidden)
			return
		case "dead502":
			w.WriteHeader(http.StatusBadGateway)
			return
		case "deadconn":
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		// healthy: forward absolute-form request
		req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		req.Header = r.Header.Clone()
		req.Header.Set("X-Via", name)
		resp, err := (&http.Transport{}).RoundTrip(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	pu, _ := url.Parse(n.proxy.URL)
	n.tr = &http.Transport{
		Proxy:             http.ProxyURL(pu),
		DisableKeepAlives: true,
	}
	return n
}

func TestFailoverSkipsBlockedAndDeadNodes(t *testing.T) {
	up, vias := newMockUpstream()
	defer up.Close()

	nodes := []*node{
		newMockNode("blocked", "blocked", up.URL),
		newMockNode("dead502", "dead502", up.URL),
		newMockNode("deadconn", "deadconn", up.URL),
		newMockNode("healthy", "healthy", up.URL),
	}
	for _, n := range nodes {
		defer n.proxy.Close()
	}
	eg := &simEgress{nodes: nodes, cur: 0}

	cfg := config.Default()
	cfg.UpstreamBase = up.URL
	cfg.MaxRetries = 5
	srv := proxy.New(cfg, eg, nil)
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, err := http.Get(front.URL + "/backend-api/codex/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "via=healthy") || !strings.Contains(string(body), "done") {
		t.Fatalf("unexpected stream: %q", body)
	}
	got := vias()
	if len(got) != 1 || got[0] != "healthy" {
		t.Fatalf("upstream should only see the healthy node, got %v", got)
	}
	if eg.Current() != "healthy" {
		t.Fatalf("current node should be healthy, got %s", eg.Current())
	}
	t.Logf("node order: %v, upstream saw: %v", eg.order(), got)
}

func TestBodyReplayThroughSimulatedFailover(t *testing.T) {
	var mu sync.Mutex
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		w.Write(b)
	}))
	defer up.Close()

	nodes := []*node{
		newMockNode("blocked", "blocked", up.URL),
		newMockNode("healthy", "healthy", up.URL),
	}
	for _, n := range nodes {
		defer n.proxy.Close()
	}
	eg := &simEgress{nodes: nodes, cur: 0}
	cfg := config.Default()
	cfg.UpstreamBase = up.URL
	cfg.MaxRetries = 3
	srv := proxy.New(cfg, eg, nil)
	front := httptest.NewServer(srv)
	defer front.Close()

	payload := `{"model":"gpt-6-astra","input":"你好"}`
	resp, err := http.Post(front.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != payload {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != payload {
		t.Fatalf("upstream saw %v, want the payload once", got)
	}
}
