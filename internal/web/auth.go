package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
)

// protectPanel applies authentication to every panel route, including assets,
// event streams and unknown paths. It never protects an inference listener.
func protectPanel(next http.Handler, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if password != "" {
			user, supplied, ok := r.BasicAuth()
			gotUser, wantUser := sha256.Sum256([]byte(user)), sha256.Sum256([]byte("admin"))
			gotPass, wantPass := sha256.Sum256([]byte(supplied)), sha256.Sum256([]byte(password))
			valid := subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) & subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
			if !ok || valid != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="ccodex-rotate", charset="UTF-8"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		// Cookies and cached Basic credentials must not turn cross-site requests
		// or GET links into control operations. CLI requests can omit Origin.
		switch r.URL.Path {
		case "/api/status", "/api/nodes", "/api/events":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
		case "/api/rotate", "/api/collect", "/api/collect-parallel", "/api/collect/stop", "/api/hunt", "/api/scan", "/api/injection", "/api/force-model", "/api/sources/add", "/api/sources/clear", "/api/pin", "/api/reset", "/api/recheck":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-origin request denied", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host || u.User != nil {
					http.Error(w, "cross-origin request denied", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
