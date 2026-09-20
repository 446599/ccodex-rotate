// Package subscription downloads subscription files on behalf of mihomo, which
// otherwise cannot bootstrap when the subscription host itself needs a proxy.
package subscription

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// candidates are tried when no proxy is configured in the environment.
var candidates = []string{
	"http://127.0.0.1:7897",
	"http://127.0.0.1:7890",
	"socks5://127.0.0.1:7897",
	"socks5://127.0.0.1:7890",
}

func envProxy() *url.URL {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if pu, err := http.ProxyFromEnvironment(req); err == nil && pu != nil {
		return pu
	}
	return nil
}

func dialOK(host string) bool {
	if !strings.Contains(host, ":") {
		return false
	}
	c, err := net.DialTimeout("tcp", host, 400*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// proxyChain returns the ordered list of proxies to try (nil = direct).
func proxyChain(explicit string) []*url.URL {
	switch strings.TrimSpace(explicit) {
	case "direct":
		return []*url.URL{nil}
	case "":
		// build from env + candidates + direct
	default:
		if u, err := url.Parse(explicit); err == nil {
			return []*url.URL{u, nil}
		}
	}
	var chain []*url.URL
	if p := envProxy(); p != nil {
		chain = append(chain, p)
	}
	for _, c := range candidates {
		u, err := url.Parse(c)
		if err != nil || !dialOK(u.Host) {
			continue
		}
		chain = append(chain, u)
	}
	chain = append(chain, nil) // direct as last resort
	return chain
}

func clientFor(proxy *url.URL) *http.Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives:     true,
	}
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
	}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func fetchOnce(ctx context.Context, rawURL string, proxy *url.URL) ([]byte, error) {
	client := clientFor(proxy)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash-verge/v1.7.7")
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	return body, nil
}

// Fetch downloads a subscription, trying every usable proxy in turn.
func Fetch(ctx context.Context, rawURL, explicitProxy string) ([]byte, error) {
	chain := proxyChain(explicitProxy)
	var lastErr error
	for round := 0; round < 2; round++ {
		for _, p := range chain {
			body, err := fetchOnce(ctx, rawURL, p)
			if err == nil {
				return body, nil
			}
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable proxy")
	}
	return nil, lastErr
}

// DownloadTo fetches a subscription and writes it to path atomically.
func DownloadTo(ctx context.Context, rawURL, explicitProxy, path string) error {
	body, err := Fetch(ctx, rawURL, explicitProxy)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IsRemote reports whether s is an http(s) URL.
func IsRemote(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// Redact hides secrets in a subscription URL for logging.
func Redact(s string) string {
	if u, err := url.Parse(s); err == nil {
		u.RawQuery = ""
		u.User = nil
		name := u.Path
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i:]
		}
		return u.Scheme + "://" + u.Host + "/…" + name
	}
	return "subscription"
}
