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

func (f *fakeEgress) Outcome(requested, served string) {}

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
