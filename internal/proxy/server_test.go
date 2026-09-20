package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ccodex-rotate/internal/config"
)

type fakeEgress struct {
	mu      sync.Mutex
	cur     string
	rotates int
	tr      *http.Transport
}

func (f *fakeEgress) Transport() *http.Transport { return f.tr }
func (f *fakeEgress) Current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}
func (f *fakeEgress) Rotate(ctx context.Context, reason string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotates++
	f.cur = "node2"
	return f.cur, true
}

func (f *fakeEgress) Success() {}

func (f *fakeEgress) Pin(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cur = name
	return nil
}

func newTestServer(t *testing.T, upstream *httptest.Server, retries int) (*Server, *fakeEgress) {
	t.Helper()
	cfg := config.Default()
	cfg.UpstreamBase = upstream.URL
	cfg.MaxRetries = retries
	fe := &fakeEgress{cur: "node1", tr: &http.Transport{}}
	return New(cfg, fe, nil), fe
}

func TestStreamingPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, "data: a\n\n")
		fl.Flush()
		io.WriteString(w, "data: b\n\n")
		fl.Flush()
	}))
	defer up.Close()

	srv, _ := newTestServer(t, up, 3)
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, err := http.Get(front.URL + "/backend-api/codex/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "data: a") || !strings.Contains(string(body), "data: b") {
		t.Fatalf("stream body not forwarded: %q", body)
	}
}

func TestFailoverOn403(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	srv, fe := newTestServer(t, up, 3)
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, err := http.Get(front.URL + "/backend-api/codex/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("expected retry to succeed, got %d %q", resp.StatusCode, body)
	}
	if fe.rotates < 1 {
		t.Fatal("expected a rotation")
	}
}

func TestFailoverOn502(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte("recovered"))
	}))
	defer up.Close()

	srv, fe := newTestServer(t, up, 3)
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, gerr := http.Get(front.URL + "/backend-api/codex/models")
	if gerr != nil {
		t.Fatal(gerr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "recovered" {
		t.Fatalf("expected recovery, got %d %q", resp.StatusCode, body)
	}
	if fe.rotates < 2 {
		t.Fatalf("expected 2 rotations, got %d", fe.rotates)
	}
}

func TestBodyReplayedAcrossFailover(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(b)
	}))
	defer up.Close()

	srv, _ := newTestServer(t, up, 3)
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, err := http.Post(front.URL+"/backend-api/codex/responses", "application/json",
		strings.NewReader(`{"hello":"world"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if string(body) != `{"hello":"world"}` {
		t.Fatalf("body not replayed, got %q", body)
	}
}

func TestNonRetriableStatusPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad"))
	}))
	defer up.Close()

	srv, fe := newTestServer(t, up, 3)
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, gerr := http.Get(front.URL + "/backend-api/codex/models")
	if gerr != nil {
		t.Fatal(gerr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if fe.rotates != 0 {
		t.Fatalf("400 must not rotate, rotates=%d", fe.rotates)
	}
}

func TestTurnStateCaptureAndReinject(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Codex-Turn-State"))
		mu.Unlock()
		w.Header().Set("X-Codex-Turn-State", "STATE-292")
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.UpstreamBase = up.URL
	cfg.StateLengths = nil // accept any length for this test
	fe := &fakeEgress{cur: "node1", tr: &http.Transport{}}
	srv := New(cfg, fe, nil)
	front := httptest.NewServer(srv)
	defer front.Close()

	body := `{"model":"gpt-6-astra","input":"hi"}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(front.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(seen))
	}
	if seen[0] != "" {
		t.Fatalf("first request should not inject, got %q", seen[0])
	}
	if seen[1] != "STATE-292" {
		t.Fatalf("second request should inject the learned state, got %q", seen[1])
	}
}

func TestProbeCollectsTargetLengthState(t *testing.T) {
	state292 := strings.Repeat("A", 292)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Turn-State", state292)
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.UpstreamBase = up.URL
	cfg.StateLengths = []int{292}
	fe := &fakeEgress{cur: "node1", tr: &http.Transport{}}
	srv := New(cfg, fe, nil)
	srv.authMu.Lock()
	srv.auth, srv.account, srv.lastModel = "Bearer test", "acct", "gpt-6-astra"
	srv.authMu.Unlock()

	length, reachable, value, err := srv.Probe(context.Background(), &http.Client{})
	if err != nil || !reachable {
		t.Fatalf("probe failed: len=%d reachable=%v err=%v", length, reachable, err)
	}
	if length != 292 || value != state292 {
		t.Fatalf("expected 292-length state, got len=%d", length)
	}
	snap := srv.StateSnapshot()
	if len(snap) != 1 || snap[0].Length != 292 || snap[0].Node != "node1" {
		t.Fatalf("state not cached correctly: %+v", snap)
	}
}

func TestProbeRejectsWrongLength(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Turn-State", strings.Repeat("B", 312)) // 11 blocks
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.UpstreamBase = up.URL
	cfg.StateLengths = []int{292}
	fe := &fakeEgress{cur: "node1", tr: &http.Transport{}}
	srv := New(cfg, fe, nil)
	srv.authMu.Lock()
	srv.auth, srv.account, srv.lastModel = "Bearer test", "acct", "gpt-6-astra"
	srv.authMu.Unlock()

	length, reachable, _, _ := srv.Probe(context.Background(), &http.Client{})
	if length != 312 || !reachable {
		t.Fatalf("expected 312 observed, got len=%d reachable=%v", length, reachable)
	}
	if got := srv.StateSnapshot(); len(got) != 0 {
		t.Fatalf("312 state must not be cached for injection, got %+v", got)
	}
}

func TestModelAliasSharesStateKey(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Turn-State", "STATE-ANY")
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.UpstreamBase = up.URL
	cfg.StateLengths = nil
	fe := &fakeEgress{cur: "node1", tr: &http.Transport{}}
	srv := New(cfg, fe, nil)
	front := httptest.NewServer(srv)
	defer front.Close()

	// "gpt-5.4-high" must be stored under the canonical "gpt-5.4".
	body := `{"model":"gpt-5.4-high","input":"hi"}`
	resp, err := http.Post(front.URL+"/backend-api/codex/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	snap := srv.StateSnapshot()
	if len(snap) != 1 || snap[0].Model != "gpt-5.4" {
		t.Fatalf("state should be keyed by canonical model, got %+v", snap)
	}
}

func TestOnlyTargetModelsTriggerCollection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.UpstreamBase = up.URL
	cfg.ProbeModel = "gpt-6-astra"
	fe := &fakeEgress{cur: "node1", tr: &http.Transport{}}
	srv := New(cfg, fe, nil)

	var mu sync.Mutex
	var triggered []string
	srv.SetOnNeedState(func(model string) {
		mu.Lock()
		triggered = append(triggered, model)
		mu.Unlock()
	})
	srv.SetCollectModels(nil) // only ProbeModel should trigger

	front := httptest.NewServer(srv)
	defer front.Close()

	post := func(model string) {
		body := `{"model":"` + model + `","input":"hi"}`
		req, _ := http.NewRequest("POST", front.URL+"/backend-api/codex/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer x")
		req.Header.Set("chatgpt-account-id", "acct")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("gpt-6-astra")
	post("codex-auto-review") // auxiliary model must not trigger

	mu.Lock()
	defer mu.Unlock()
	if len(triggered) != 1 || triggered[0] != "gpt-6-astra" {
		t.Fatalf("only the target model should trigger collection, got %v", triggered)
	}
}

func TestExhaustedRetriesReturnsLastStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer up.Close()

	srv, fe := newTestServer(t, up, 2)
	front := httptest.NewServer(srv)
	defer front.Close()

	resp, gerr := http.Get(front.URL + "/backend-api/codex/models")
	if gerr != nil {
		t.Fatal(gerr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want last upstream status 403, got %d", resp.StatusCode)
	}
	if fe.rotates != 2 {
		t.Fatalf("want 2 rotations, got %d", fe.rotates)
	}
}
