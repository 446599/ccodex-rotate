// Package nodes parses proxy "share links" (ss://, vmess://, vless://,
// trojan://, hysteria2://, http(s)://, socks5://) into mihomo proxy maps and
// writes them as a Clash provider file that the generated config loads.
package nodes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Proxy is a mihomo proxy definition.
type Proxy map[string]any

// Parse converts one share link into a mihomo proxy.
func Parse(uri string) (Proxy, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, fmt.Errorf("empty link")
	}
	lower := strings.ToLower(uri)
	switch {
	case strings.HasPrefix(lower, "ss://"):
		return parseSS(uri)
	case strings.HasPrefix(lower, "vmess://"):
		return parseVmess(uri)
	case strings.HasPrefix(lower, "vless://"):
		return parseVless(uri)
	case strings.HasPrefix(lower, "trojan://"):
		return parseTrojan(uri)
	case strings.HasPrefix(lower, "hysteria2://"), strings.HasPrefix(lower, "hy2://"):
		return parseHysteria2(uri)
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"),
		strings.HasPrefix(lower, "socks5://"), strings.HasPrefix(lower, "socks5h://"):
		return parseSimpleProxy(uri)
	default:
		return nil, fmt.Errorf("unsupported scheme in %q", redact(uri))
	}
}

// ParseAll parses every link, returning the successes and the first errors.
func ParseAll(uris []string) ([]Proxy, []error) {
	var out []Proxy
	var errs []error
	seen := map[string]bool{}
	for _, u := range uris {
		p, err := Parse(u)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		name, _ := p["name"].(string)
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, p)
	}
	return out, errs
}

// WriteProvider writes proxies as a Clash YAML provider file.
func WriteProvider(path string, proxies []Proxy) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("proxies:\n")
	for _, p := range proxies {
		b.WriteString("  - ")
		b.WriteString(flow(p))
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// flow renders a map/slice/scalar in YAML flow style.
func flow(v any) string {
	switch t := v.(type) {
	case map[string]any:
		return flowMap(t)
	case Proxy:
		return flowMap(map[string]any(t))
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, flow(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case string:
		return quote(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case nil:
		return "null"
	default:
		return quote(fmt.Sprint(t))
	}
}

func flowMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, quote(k)+": "+flow(m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func parseSS(uri string) (Proxy, error) {
	rest := uri[len("ss://"):]
	name := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		name, _ = url.QueryUnescape(rest[i+1:])
		rest = rest[:i]
	}
	if i := strings.Index(rest, "?"); i >= 0 {
		rest = rest[:i] // plugins not supported
	}
	var method, password, host, port string
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo, hostport := rest[:at], rest[at+1:]
		dec, err := b64decode(userinfo)
		if err != nil {
			return nil, fmt.Errorf("ss userinfo: %w", err)
		}
		method, password = split2(dec, ":")
		host, port = splitHostPort(hostport)
	} else {
		dec, err := b64decode(rest)
		if err != nil {
			return nil, fmt.Errorf("ss body: %w", err)
		}
		at := strings.LastIndex(dec, "@")
		if at < 0 {
			return nil, fmt.Errorf("ss body missing @")
		}
		method, password = split2(dec[:at], ":")
		host, port = splitHostPort(dec[at+1:])
	}
	if host == "" || port == "" {
		return nil, fmt.Errorf("ss missing host/port")
	}
	if name == "" {
		name = host + ":" + port
	}
	return Proxy{"name": name, "type": "ss", "server": host, "port": port, "cipher": method, "password": password, "udp": true}, nil
}

func parseVmess(uri string) (Proxy, error) {
	body := uri[len("vmess://"):]
	if i := strings.Index(body, "#"); i >= 0 {
		body = body[:i]
	}
	dec, err := b64decode(body)
	if err != nil {
		return nil, fmt.Errorf("vmess body: %w", err)
	}
	var j struct {
		PS   string `json:"ps"`
		Add  string `json:"add"`
		Port any    `json:"port"`
		ID   string `json:"id"`
		Aid  any    `json:"aid"`
		Scy  string `json:"scy"`
		Net  string `json:"net"`
		Type string `json:"type"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
	}
	if err := json.Unmarshal([]byte(dec), &j); err != nil {
		return nil, fmt.Errorf("vmess json: %w", err)
	}
	if j.Add == "" || j.ID == "" {
		return nil, fmt.Errorf("vmess missing add/id")
	}
	name := j.PS
	if name == "" {
		name = j.Add
	}
	p := Proxy{
		"name": name, "type": "vmess", "server": j.Add, "port": toString(j.Port),
		"uuid": j.ID, "alterId": toInt(j.Aid), "cipher": orDefault(j.Scy, "auto"), "udp": true,
	}
	if j.TLS == "tls" {
		p["tls"] = true
		if j.SNI != "" {
			p["servername"] = j.SNI
		} else if j.Host != "" {
			p["servername"] = j.Host
		}
	}
	if j.Net == "ws" {
		opts := map[string]any{"path": orDefault(j.Path, "/")}
		if j.Host != "" {
			opts["headers"] = map[string]any{"Host": j.Host}
		}
		p["network"] = "ws"
		p["ws-opts"] = opts
	} else if j.Net != "" && j.Net != "tcp" {
		p["network"] = j.Net
	}
	return p, nil
}

func parseVless(uri string) (Proxy, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("vless parse: %v", err)
	}
	id := ""
	if u.User != nil {
		id = u.User.Username()
	}
	q := u.Query()
	name := fragName(u, u.Hostname())
	p := Proxy{"name": name, "type": "vless", "server": u.Hostname(), "port": u.Port(), "uuid": id, "udp": true}
	applyTLS(p, q)
	applyTransport(p, q)
	if f := q.Get("flow"); f != "" {
		p["flow"] = f
	}
	return p, nil
}

func parseTrojan(uri string) (Proxy, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("trojan parse: %v", err)
	}
	pass := ""
	if u.User != nil {
		pass = u.User.Username()
	}
	q := u.Query()
	name := fragName(u, u.Hostname())
	p := Proxy{"name": name, "type": "trojan", "server": u.Hostname(), "port": u.Port(), "password": pass, "udp": true}
	if q.Get("security") == "tls" || q.Get("tls") == "1" {
		p["sni"] = q.Get("sni")
		if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" {
			p["skip-cert-verify"] = true
		}
	}
	applyTransport(p, q)
	return p, nil
}

func parseHysteria2(uri string) (Proxy, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("hysteria2 parse: %v", err)
	}
	pass := ""
	if u.User != nil {
		pass = u.User.Username()
		if p, ok := u.User.Password(); ok && p != "" {
			pass = pass + ":" + p
		}
	}
	q := u.Query()
	name := fragName(u, u.Hostname())
	p := Proxy{"name": name, "type": "hysteria2", "server": u.Hostname(), "port": u.Port(), "password": pass}
	if s := q.Get("sni"); s != "" {
		p["sni"] = s
	}
	if q.Get("insecure") == "1" {
		p["skip-cert-verify"] = true
	}
	if obfs := q.Get("obfs"); obfs != "" {
		p["obfs"] = obfs
		p["obfs-password"] = q.Get("obfs-password")
	}
	return p, nil
}

func parseSimpleProxy(uri string) (Proxy, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("proxy parse: %v", err)
	}
	typ := u.Scheme
	tls := false
	switch u.Scheme {
	case "http":
		typ = "http"
	case "https":
		typ = "http"
		tls = true
	case "socks5", "socks5h":
		typ = "socks5"
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	name := u.Hostname() + ":" + port
	p := Proxy{"name": name, "type": typ, "server": u.Hostname(), "port": port}
	if tls {
		p["tls"] = true
	}
	if u.User != nil {
		p["username"] = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			p["password"] = pw
		}
	}
	if typ == "socks5" {
		p["udp"] = true
	}
	return p, nil
}

func applyTLS(p Proxy, q url.Values) {
	sec := q.Get("security")
	if sec == "tls" || sec == "reality" || q.Get("tls") == "1" {
		p["tls"] = true
		if s := q.Get("sni"); s != "" {
			p["servername"] = s
		}
	}
	if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" {
		p["skip-cert-verify"] = true
	}
}

func applyTransport(p Proxy, q url.Values) {
	switch q.Get("type") {
	case "ws":
		opts := map[string]any{"path": orDefault(q.Get("path"), "/")}
		if h := q.Get("host"); h != "" {
			opts["headers"] = map[string]any{"Host": h}
		}
		p["network"] = "ws"
		p["ws-opts"] = opts
	case "grpc":
		p["network"] = "grpc"
		p["grpc-opts"] = map[string]any{"grpc-service-name": q.Get("serviceName")}
	case "h2", "http":
		p["network"] = "h2"
		p["h2-opts"] = map[string]any{"path": orDefault(q.Get("path"), "/"), "host": []any{q.Get("host")}}
	}
}

func fragName(u *url.URL, fallback string) string {
	if u.Fragment != "" {
		if n, err := url.QueryUnescape(u.Fragment); err == nil && n != "" {
			return n
		}
		return u.Fragment
	}
	return fallback
}

func b64decode(s string) (string, error) {
	s = strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(pad(s)); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("invalid base64")
}

func pad(s string) string {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return s
}

func split2(s, sep string) (string, string) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func splitHostPort(s string) (string, string) {
	// handle [ipv6]:port
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "]"); i >= 0 {
			host := s[1:i]
			rest := strings.TrimPrefix(s[i+1:], ":")
			return host, rest
		}
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.Itoa(int(t))
	case int:
		return strconv.Itoa(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func toInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func redact(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}
