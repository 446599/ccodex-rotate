package mihomo

import (
	"fmt"
	"net/url"
	"strings"

	"ccodex-rotate/internal/config"
)

// provider names are fixed so the manager can address them.
const (
	AutoGroup    = "AUTO"
	MainGroup    = "CODEX"   // forwarding (may be manually pinned)
	CollectGroup = "COLLECT" // credential collection (always auto)
)

// Provider is a locally cached subscription file loaded by mihomo.
type Provider struct {
	Name string
	Path string
}

// GenerateConfig renders a minimal mihomo configuration that:
//   - loads every locally cached subscription as a file proxy-provider,
//   - health-checks every node against the (unauthenticated) Codex endpoint,
//     keeping only nodes that answer with the expected status (401 = reachable,
//     403 = blocked exit),
//   - exposes an AUTO url-test group and a CODEX selector that also allows
//     pinning a single node.
func GenerateConfig(cfg config.Config, providers []Provider) (string, error) {
	if len(providers) == 0 && len(cfg.Proxies) == 0 {
		return "", fmt.Errorf("no subscriptions or proxies configured")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "mixed-port: %d\n", cfg.MixedPort)
	b.WriteString("allow-lan: false\n")
	b.WriteString("bind-address: 127.0.0.1\n")
	b.WriteString("mode: rule\n")
	b.WriteString("log-level: warning\n")
	b.WriteString("ipv6: false\n")
	fmt.Fprintf(&b, "external-controller: 127.0.0.1:%d\n", cfg.ControllerPort)
	fmt.Fprintf(&b, "secret: %s\n", yq(cfg.ControllerSecret))
	b.WriteString("external-controller-cors:\n  allow-origins:\n    - \"*\"\n  allow-private-network: true\n")
	b.WriteString("unified-delay: true\n")
	b.WriteString("profile:\n  store-selected: true\n  store-fake-ip: false\n")
	// DNS: resolve proxy-server domains over national DoH to avoid DNS pollution
	// (otherwise node servers resolve to hijacked IPs and every dial times out).
	b.WriteString("dns:\n")
	b.WriteString("  enable: true\n")
	b.WriteString("  ipv6: false\n")
	b.WriteString("  enhanced-mode: normal\n")
	b.WriteString("  default-nameserver:\n    - 223.5.5.5\n    - 119.29.29.29\n")
	b.WriteString("  nameserver:\n    - https://223.5.5.5/dns-query\n    - https://doh.pub/dns-query\n    - https://dns.alidns.com/dns-query\n")
	b.WriteString("  proxy-server-nameserver:\n    - https://223.5.5.5/dns-query\n    - https://doh.pub/dns-query\n    - https://dns.alidns.com/dns-query\n")

	var providerNames []string
	if len(providers) > 0 {
		b.WriteString("proxy-providers:\n")
		for _, p := range providers {
			providerNames = append(providerNames, p.Name)
			fmt.Fprintf(&b, "  %s:\n", yq(p.Name))
			b.WriteString("    type: file\n")
			fmt.Fprintf(&b, "    path: %s\n", yq(p.Path))
			b.WriteString("    health-check:\n")
			b.WriteString("      enable: true\n")
			fmt.Fprintf(&b, "      url: %s\n", yq(cfg.HealthURL))
			fmt.Fprintf(&b, "      interval: %d\n", cfg.HealthIntervalSec)
			fmt.Fprintf(&b, "      expected-status: %s\n", yq(cfg.HealthExpected))
			b.WriteString("      timeout: 5000\n")
		}
	}

	var explicit []string
	if len(cfg.Proxies) > 0 {
		b.WriteString("proxies:\n")
		for i, uri := range cfg.Proxies {
			name, line, err := proxyEntry(fmt.Sprintf("node%d", i+1), uri)
			if err != nil {
				return "", err
			}
			explicit = append(explicit, name)
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("proxy-groups:\n")
	fmt.Fprintf(&b, "  - name: %s\n", yq(AutoGroup))
	b.WriteString("    type: url-test\n")
	b.WriteString("    lazy: false\n")
	fmt.Fprintf(&b, "    url: %s\n", yq(cfg.HealthURL))
	fmt.Fprintf(&b, "    expected-status: %s\n", yq(cfg.HealthExpected))
	fmt.Fprintf(&b, "    interval: %d\n", cfg.TestIntervalSec)
	b.WriteString("    tolerance: 50\n")
	writeGroupMembers(&b, explicit, providerNames)

	fmt.Fprintf(&b, "  - name: %s\n", yq(MainGroup))
	b.WriteString("    type: select\n")
	b.WriteString("    proxies:\n")
	fmt.Fprintf(&b, "      - %s\n", yq(AutoGroup))
	for _, p := range explicit {
		fmt.Fprintf(&b, "      - %s\n", yq(p))
	}
	if len(providerNames) > 0 {
		b.WriteString("    use:\n")
		for _, p := range providerNames {
			fmt.Fprintf(&b, "      - %s\n", yq(p))
		}
	}

	// COLLECT group + a dedicated inbound: credential collection is routed
	// through it, so a manual forwarding choice never affects collection.
	fmt.Fprintf(&b, "  - name: %s\n", yq(CollectGroup))
	b.WriteString("    type: select\n")
	if len(explicit) > 0 {
		b.WriteString("    proxies:\n")
		for _, p := range explicit {
			fmt.Fprintf(&b, "      - %s\n", yq(p))
		}
	}
	if len(providerNames) > 0 {
		b.WriteString("    use:\n")
		for _, p := range providerNames {
			fmt.Fprintf(&b, "      - %s\n", yq(p))
		}
	}

	b.WriteString("listeners:\n")
	b.WriteString("  - name: collect-in\n")
	b.WriteString("    type: mixed\n")
	fmt.Fprintf(&b, "    port: %d\n", cfg.CollectPort)
	b.WriteString("    listen: 127.0.0.1\n")
	fmt.Fprintf(&b, "    proxy: %s\n", yq(CollectGroup))

	b.WriteString("rules:\n")
	fmt.Fprintf(&b, "  - MATCH,%s\n", MainGroup)
	return b.String(), nil
}

func writeGroupMembers(b *strings.Builder, explicit, providers []string) {
	if len(explicit) > 0 {
		b.WriteString("    proxies:\n")
		for _, p := range explicit {
			fmt.Fprintf(b, "      - %s\n", yq(p))
		}
	}
	if len(providers) > 0 {
		b.WriteString("    use:\n")
		for _, p := range providers {
			fmt.Fprintf(b, "      - %s\n", yq(p))
		}
	}
}

// proxyEntry converts a single proxy URI into a mihomo proxy entry.
func proxyEntry(name, raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("invalid proxy %q", raw)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			return "", "", fmt.Errorf("unsupported proxy scheme %q in %q", u.Scheme, raw)
		}
	}
	typ := u.Scheme
	tls := "false"
	switch u.Scheme {
	case "http":
		typ = "http"
	case "https":
		typ = "http"
		tls = "true"
	case "socks5", "socks5h":
		typ = "socks5"
	default:
		return "", "", fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	line := fmt.Sprintf("{name: %s, type: %s, server: %s, port: %s, tls: %s",
		yq(name), typ, yq(host), port, tls)
	if u.User != nil {
		user := u.User.Username()
		pass, _ := u.User.Password()
		line += fmt.Sprintf(", username: %s, password: %s", yq(user), yq(pass))
	}
	if typ == "socks5" {
		line += ", udp: true"
	}
	line += "}"
	return name, line, nil
}

// yq single-quotes a YAML scalar.
func yq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
