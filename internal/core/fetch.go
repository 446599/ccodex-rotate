// Package core downloads a mihomo (Clash.Meta) binary for the current platform.
package core

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ccodex-rotate/internal/subscription"
)

const apiLatest = "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest"

// Fetch downloads the latest mihomo core into dir and returns its path.
func Fetch(ctx context.Context, explicitProxy, dir string) (string, error) {
	proxy, err := subscription.ResolveProxy(explicitProxy)
	if err != nil {
		return "", err
	}
	hc := newHTTPClient(proxy)

	assetURL, err := latestAsset(ctx, hc)
	if err != nil {
		return "", err
	}
	body, err := download(ctx, hc, assetURL)
	if err != nil {
		return "", err
	}
	bin, err := extract(body, assetURL)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "mihomo"
	if runtime.GOOS == "windows" {
		name = "mihomo.exe"
	}
	out := filepath.Join(dir, name)
	if err := os.WriteFile(out, bin, 0o755); err != nil {
		return "", err
	}
	return out, nil
}

func newHTTPClient(proxy *url.URL) *http.Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Minute}
}

func latestAsset(ctx context.Context, hc *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiLatest, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ccodex-rotate")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github api: %s", resp.Status)
	}
	var payload struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("mihomo-%s-%s-", runtime.GOOS, runtime.GOARCH)
	var fallback string
	for _, a := range payload.Assets {
		if !strings.HasPrefix(a.Name, prefix) {
			continue
		}
		if strings.Contains(a.Name, "compatible") {
			if fallback == "" {
				fallback = a.URL
			}
			continue
		}
		return a.URL, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no mihomo asset for %s/%s in latest release", runtime.GOOS, runtime.GOARCH)
}

func download(ctx context.Context, hc *http.Client, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ccodex-rotate")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("download: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

func extract(body []byte, rawURL string) ([]byte, error) {
	switch {
	case strings.HasSuffix(rawURL, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			base := strings.ToLower(filepath.Base(f.Name))
			if strings.HasPrefix(base, "mihomo") {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, 128<<20))
			}
		}
		return nil, fmt.Errorf("mihomo binary not found in zip")
	case strings.HasSuffix(rawURL, ".gz"):
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		return io.ReadAll(io.LimitReader(gr, 128<<20))
	default:
		return body, nil
	}
}
