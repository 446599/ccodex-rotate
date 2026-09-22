package mihomo

import (
	"strings"
	"testing"

	"ccodex-rotate/internal/config"
)

func baseCfg() config.Config {
	c := config.Default()
	c.Subscriptions = []string{"https://example.com/sub"}
	return c
}

func TestGenerateConfigIncludesHealthAndGroups(t *testing.T) {
	c := baseCfg()
	c.Proxies = []string{"socks5://127.0.0.1:7897"}
	providers := []Provider{{Name: "sub1", Path: "/data/providers/sub1.yaml"}}
	out, err := GenerateConfig(c, providers)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"proxy-providers:",
		"type: file",
		"path: '/data/providers/sub1.yaml'",
		"health-check:",
		"expected-status: '*'",
		"proxies:",
		"type: socks5",
		"server: '127.0.0.1'",
		"port: 7897",
		"type: url-test",
		"name: 'AUTO'",
		"name: 'CODEX'",
		"name: 'COLLECT'",
		"listeners:",
		"proxy: 'COLLECT'",
		"- MATCH,CODEX",
		"allow-lan: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %q\n---\n%s", want, out)
		}
	}
}

func TestGenerateConfigNoSourcesFallsBackToDirect(t *testing.T) {
	c := config.Default()
	out, err := GenerateConfig(c, nil)
	if err != nil {
		t.Fatalf("empty config should still generate (DIRECT fallback): %v", err)
	}
	if !strings.Contains(out, "DIRECT") {
		t.Fatalf("expected DIRECT fallback:\n%s", out)
	}
	if !strings.Contains(out, "- MATCH,CODEX") {
		t.Fatalf("expected rules:\n%s", out)
	}
}

func TestGenerateConfigExplicitOnly(t *testing.T) {
	c := config.Default()
	c.Proxies = []string{"http://127.0.0.1:7897"}
	out, err := GenerateConfig(c, nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(out, "proxy-providers:") {
		t.Errorf("no providers expected:\n%s", out)
	}
	if !strings.Contains(out, "name: 'AUTO'") {
		t.Errorf("auto group should still exist:\n%s", out)
	}
}

func TestProxyEntrySchemes(t *testing.T) {
	c := config.Default()
	c.Proxies = []string{"http://user:pass@host:8080", "https://h:443", "socks5://h:1080"}
	out, err := GenerateConfig(c, nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(out, "username: 'user'") || !strings.Contains(out, "password: 'pass'") {
		t.Errorf("http auth not rendered:\n%s", out)
	}
	if !strings.Contains(out, "tls: true") {
		t.Errorf("https should set tls: true:\n%s", out)
	}
}

func TestGroupDefaultSelection(t *testing.T) {
	c := baseCfg()
	providers := []Provider{{Name: "sub1", Path: "/x/sub1.yaml"}}
	out, _ := GenerateConfig(c, providers)
	idx := strings.Index(out, "- name: 'CODEX'")
	if idx < 0 {
		t.Fatal("no CODEX group")
	}
	tail := out[idx:]
	if !strings.Contains(tail, "proxies:\n      - 'AUTO'") {
		t.Errorf("AUTO should be the first CODEX member:\n%s", tail)
	}
}

func TestParallelLaneGroupsAndListeners(t *testing.T) {
	c := baseCfg()
	c.CollectLanes = 3
	providers := []Provider{{Name: "sub1", Path: "/x/sub1.yaml"}}
	out, err := GenerateConfig(c, providers)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, want := range []string{
		"- name: 'COLLECT-1'",
		"- name: 'COLLECT-3'",
		"- name: collect-in-1",
		"- name: collect-in-3",
		"port: 17893",
		"port: 17895",
		"proxy: 'COLLECT-1'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in generated config", want)
		}
	}
	if strings.Contains(out, "COLLECT-4") {
		t.Errorf("only 3 lanes expected")
	}
	// Lane groups must live in proxy-groups, not under listeners.
	lg := strings.Index(out, "- name: 'COLLECT-1'")
	li := strings.Index(out, "listeners:")
	if lg < 0 || li < 0 || lg > li {
		t.Errorf("lane group must precede listeners section")
	}
}
