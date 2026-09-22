package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCookieIsolationAcrossForwardingProbeAndDegradation(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var degrade atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request body")
			return
		}
		mu.Lock()
		seen = append(seen, body.Model+":"+r.Header.Get("Cookie"))
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: body.Model})
		w.Header().Set("X-Codex-Turn-State", strings.Repeat("x", 292))
		served := body.Model
		if degrade.Load() && served == "gpt-6-astra" {
			served = "gpt-5.6-luna"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"model":"`+served+`"}}`+"\n\n")
	}))
	defer up.Close()
	srv, _ := newTestServer(t, up, 0)
	forward := func(model string) {
		req := httptest.NewRequest("POST", "http://local/backend-api/codex/responses", strings.NewReader(`{"model":"`+model+`"}`))
		req.Header.Set("Authorization", "Bearer synthetic")
		req.Header.Set("chatgpt-account-id", "account")
		out := httptest.NewRecorder()
		srv.ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatalf("forward failed: %d", out.Code)
		}
	}
	forward("gpt-6-astra")
	if _, _, _, _, err := srv.Probe(context.Background(), up.Client(), "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	forward("gpt-5.6-sol")
	forward("gpt-6-astra")
	degrade.Store(true)
	forward("gpt-6-astra")
	if srv.HasValidState("gpt-6-astra") {
		t.Fatal("degraded astra state not dropped")
	}
	if !srv.HasValidState("gpt-5.6-sol") {
		t.Fatal("astra invalidation deleted sol state")
	}
	forward("gpt-5.6-sol")
	forward("gpt-6-astra")
	want := []string{"gpt-6-astra:", "gpt-5.6-sol:", "gpt-5.6-sol:SID=gpt-5.6-sol", "gpt-6-astra:SID=gpt-6-astra", "gpt-6-astra:SID=gpt-6-astra", "gpt-5.6-sol:SID=gpt-5.6-sol", "gpt-6-astra:"}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(want) {
		t.Fatalf("got %d requests want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("request %d got %q want %q", i, seen[i], want[i])
		}
	}
}
