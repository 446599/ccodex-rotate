package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelAuthenticationAndControls(t *testing.T) {
	handler := (&Panel{Password: "test-password"}).Handler()
	for _, path := range []string{"/panel", "/panel/assets/panel.js", "/api/status", "/api/events", "/api/collect", "/api/sources/add", "/backend-api/codex/responses"} {
		for _, password := range []string{"", "wrong"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if password != "" {
				req.SetBasicAuth("admin", password)
			}
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			if out.Code != http.StatusUnauthorized || out.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("%s: missing authentication challenge (%d)", path, out.Code)
			}
		}
	}
	for _, test := range []struct {
		method, path, origin, site string
		want                       int
	}{
		{"GET", "/panel", "", "", 200},
		{"GET", "/api/collect", "", "", 405},
		{"POST", "/api/sources/add", "https://evil.example", "", 403},
		{"POST", "/api/sources/add", "null", "", 403},
		{"POST", "/api/sources/add", "", "cross-site", 403},
		{"POST", "/api/sources/add", "https://example.com", "same-origin", 400},
		{"POST", "/api/sources/add", "", "", 400},
		{"POST", "/backend-api/codex/responses", "", "", 404},
	} {
		req := httptest.NewRequest(test.method, "http://example.com"+test.path, strings.NewReader("{"))
		req.SetBasicAuth("admin", "test-password")
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Sec-Fetch-Site", test.site)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != test.want {
			t.Fatalf("%s %s origin=%q: got %d want %d", test.method, test.path, test.origin, out.Code, test.want)
		}
		if out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("panel response must not be cached")
		}
	}
	open := (&Panel{}).Handler()
	out := httptest.NewRecorder()
	open.ServeHTTP(out, httptest.NewRequest("GET", "/panel", nil))
	if out.Code != 200 {
		t.Fatal("empty password must retain local unauthenticated mode")
	}
}
