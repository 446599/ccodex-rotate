package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"ccodex-rotate/internal/config"
	"ccodex-rotate/internal/web"
)

func TestCLIUsesPanelAddressAndPassword(t *testing.T) {
	server := httptest.NewServer((&web.Panel{Password: "environment-password"}).Handler())
	defer server.Close()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:1"
	cfg.PanelListen = strings.TrimPrefix(server.URL, "http://")
	cfg.PanelPassword = "config-password"
	t.Setenv("CCODEX_ROTATE_PANEL_PASSWORD", "environment-password")
	body, err := getJSON(cfg, "/panel")
	if err != nil || !strings.Contains(string(body), "<html") {
		t.Fatalf("CLI panel request failed: %v", err)
	}
	t.Setenv("CCODEX_ROTATE_PANEL_PASSWORD", "")
	if _, err := getJSON(cfg, "/panel"); err == nil {
		t.Fatal("CLI must report incorrect password")
	}
}
