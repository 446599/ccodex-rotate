// Package mihomo launches and controls a private mihomo (Clash.Meta) core.
package mihomo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ccodex-rotate/internal/config"
	"ccodex-rotate/internal/nodes"
	"ccodex-rotate/internal/subscription"
)

// Proxy mirrors the subset of mihomo's /proxies payload we care about.
type Proxy struct {
	Name    string       `json:"name"`
	Type    string       `json:"type"`
	Now     string       `json:"now"`
	All     []string     `json:"all"`
	History []DelayEntry `json:"history"`
}

// DelayEntry is one health-check result.
type DelayEntry struct {
	Time  string `json:"time"`
	Delay int    `json:"delay"`
}

// Node is a flattened, user-facing proxy node with health.
type Node struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Delay int    `json:"delay"`
	Alive bool   `json:"alive"`
}

// Manager owns a mihomo subprocess and talks to its controller.
type Manager struct {
	cfg       config.Config
	dir       string
	base      string
	client    *http.Client
	cmd       *exec.Cmd
	providers []Provider
	warnings  []string
	mu        sync.Mutex
	lastErr   error
	stopping  bool
}

// New builds a Manager. It does not start the process.
func New(cfg config.Config, dataDir string) *Manager {
	return &Manager{
		cfg:  cfg,
		dir:  filepath.Join(dataDir, "mihomo"),
		base: fmt.Sprintf("http://127.0.0.1:%d", cfg.ControllerPort),
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Dir returns the mihomo working directory.
func (m *Manager) Dir() string { return m.dir }

// ConfigPath is the generated mihomo config path.
func (m *Manager) ConfigPath() string { return filepath.Join(m.dir, "config.yaml") }

// FindBinary locates a usable mihomo binary on macOS or Windows.
func FindBinary(cfg config.Config) (string, error) {
	if cfg.MihomoPath != "" {
		if _, err := os.Stat(cfg.MihomoPath); err == nil {
			return cfg.MihomoPath, nil
		}
	}
	var candidates []string
	candidates = append(candidates,
		// macOS
		"/Applications/Clash Verge.app/Contents/MacOS/verge-mihomo",
		"/Applications/ClashX Meta.app/Contents/Resources/mihomo",
		"/Applications/ClashX Pro.app/Contents/Resources/mihomo",
		"/opt/homebrew/bin/mihomo",
		"/usr/local/bin/mihomo",
		"/opt/homebrew/bin/clash-meta",
	)
	candidates = append(candidates, windowsCandidates()...)

	// Next to this executable and in the data dir (fetch-core writes here).
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "mihomo"), filepath.Join(dir, "mihomo.exe"),
			filepath.Join(dir, "verge-mihomo"), filepath.Join(dir, "verge-mihomo.exe"),
		)
	}
	home, _ := os.UserHomeDir()
	dataDir := config.DataDir()
	for _, d := range []string{
		filepath.Join(dataDir, "mihomo"),
		filepath.Join(home, ".ccodex-rotate", "mihomo"),
	} {
		candidates = append(candidates,
			filepath.Join(d, "mihomo"), filepath.Join(d, "mihomo.exe"),
			filepath.Join(d, "verge-mihomo"), filepath.Join(d, "verge-mihomo.exe"),
			filepath.Join(d, "clash-meta"), filepath.Join(d, "clash-meta.exe"),
		)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	for _, name := range []string{"mihomo", "mihomo.exe", "clash-meta", "clash-meta.exe", "clash", "clash.exe", "verge-mihomo", "verge-mihomo.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("mihomo binary not found; run `ccodex-rotate fetch-core` to download it, or set mihomo_path in config")
}

// windowsCandidates returns common Windows core locations from the environment.
func windowsCandidates() []string {
	var dirs []string
	addDir := func(root, name string) {
		if root == "" {
			return
		}
		base := filepath.Join(root, name)
		dirs = append(dirs, base, filepath.Join(base, "resources"), filepath.Join(base, "resources", "sidecar"))
	}
	for _, root := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("PROGRAMFILES"), os.Getenv("ProgramFiles(x86)")} {
		addDir(root, filepath.Join("Programs", "Clash Verge"))
		addDir(root, "Clash Verge")
		addDir(root, filepath.Join("Programs", "Mihomo Party"))
		addDir(root, "Mihomo Party")
		addDir(root, "Clash Verge Rev")
	}
	for _, sub := range []string{
		filepath.Join("scoop", "apps", "mihomo", "current"),
		filepath.Join("scoop", "apps", "clash-meta", "current"),
	} {
		if home := os.Getenv("USERPROFILE"); home != "" {
			dirs = append(dirs, filepath.Join(home, sub))
		}
	}
	var out []string
	for _, d := range dirs {
		for _, exe := range []string{"verge-mihomo.exe", "mihomo.exe", "clash-meta.exe", "clash.exe"} {
			out = append(out, filepath.Join(d, exe))
		}
	}
	return out
}

// Start writes the config and launches mihomo, then waits for the controller.
func (m *Manager) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(m.dir, "providers"), 0o700); err != nil {
		return err
	}
	providers, err := m.PrepareProviders(ctx)
	if err != nil {
		return err
	}
	m.providers = providers
	text, err := GenerateConfig(m.cfg, providers)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.ConfigPath(), []byte(text), 0o600); err != nil {
		return err
	}
	bin, err := FindBinary(m.cfg)
	if err != nil {
		return err
	}

	logPath := filepath.Join(m.dir, "mihomo.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "-d", m.dir, "-f", m.ConfigPath())
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start mihomo: %w", err)
	}
	m.mu.Lock()
	m.cmd = cmd
	m.stopping = false
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		logFile.Close()
		m.mu.Lock()
		if !m.stopping {
			m.lastErr = fmt.Errorf("mihomo exited: %v", err)
		}
		m.mu.Unlock()
	}()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.Version(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return fmt.Errorf("mihomo controller did not become ready; see %s", logPath)
}

// Stop terminates the mihomo process.
func (m *Manager) Stop() {
	m.mu.Lock()
	cmd := m.cmd
	m.stopping = true
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// Err returns the last background error, if any.
func (m *Manager) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func (m *Manager) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.ControllerSecret)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("controller %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// Version probes the controller.
func (m *Manager) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := m.get(ctx, "/version", &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// Proxies returns the raw proxy map.
func (m *Manager) Proxies(ctx context.Context) (map[string]Proxy, error) {
	var payload struct {
		Proxies map[string]Proxy `json:"proxies"`
	}
	if err := m.get(ctx, "/proxies", &payload); err != nil {
		return nil, err
	}
	return payload.Proxies, nil
}

var groupTypes = map[string]bool{
	"Selector": true, "URLTest": true, "Fallback": true,
	"LoadBalance": true, "Relay": true, "Compatible": true, "Direct": true,
	"Reject": true, "RejectDrop": true, "Pass": true, "PassRule": true, "Dns": true,
}

var builtinNames = map[string]bool{
	"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true,
	"COMPATIBLE": true, "PASS-RULE": true, "GLOBAL": true, "DNS": true,
}

// NodeNames returns every real node name, subscription order first.
func (m *Manager) NodeNames(ctx context.Context) ([]string, error) {
	proxies, err := m.Proxies(ctx)
	if err != nil {
		return nil, err
	}
	prov, _ := m.providerProxies(ctx)
	seen := map[string]bool{}
	var names []string
	add := func(name, typ string) {
		if name == "" || seen[name] || groupTypes[typ] || builtinNames[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, p := range prov {
		add(p.Name, p.Type)
	}
	for _, p := range proxies {
		add(p.Name, p.Type)
	}
	return names, nil
}

// providerProxies returns the nodes contributed by proxy-providers.
func (m *Manager) providerProxies(ctx context.Context) ([]Proxy, error) {
	var payload struct {
		Providers map[string]struct {
			Proxies []Proxy `json:"proxies"`
		} `json:"providers"`
	}
	if err := m.get(ctx, "/providers/proxies", &payload); err != nil {
		return nil, err
	}
	var out []Proxy
	for _, p := range payload.Providers {
		out = append(out, p.Proxies...)
	}
	return out, nil
}

// Nodes lists every real proxy node (provider nodes + explicit proxies) with
// its latest health. Alive nodes come first, sorted by delay.
func (m *Manager) Nodes(ctx context.Context) ([]Node, error) {
	proxies, err := m.Proxies(ctx)
	if err != nil {
		return nil, err
	}
	prov, err := m.providerProxies(ctx)
	if err != nil {
		// Providers may be absent (explicit-only setups); not fatal.
		prov = nil
	}
	seen := map[string]bool{}
	var nodes []Node
	add := func(name, typ string, hist []DelayEntry) {
		if name == "" || seen[name] {
			return
		}
		if groupTypes[typ] || builtinNames[name] {
			return
		}
		seen[name] = true
		n := Node{Name: name, Type: typ}
		if len(hist) > 0 {
			n.Delay = hist[len(hist)-1].Delay
		}
		n.Alive = n.Delay > 0
		nodes = append(nodes, n)
	}
	for _, p := range proxies {
		add(p.Name, p.Type, p.History)
	}
	for _, p := range prov {
		add(p.Name, p.Type, p.History)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Alive != nodes[j].Alive {
			return nodes[i].Alive
		}
		return nodes[i].Delay < nodes[j].Delay
	})
	return nodes, nil
}

// Select pins the given group to a node (or a sub-group such as AUTO).
func (m *Manager) Select(ctx context.Context, group, name string) error {
	body := strings.NewReader(fmt.Sprintf(`{"name":%q}`, name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.base+"/proxies/"+group, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.ControllerSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("select %s=%s: %s: %s", group, name, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Current returns the node currently used by the CODEX group.
func (m *Manager) Current(ctx context.Context) (string, error) {
	proxies, err := m.Proxies(ctx)
	if err != nil {
		return "", err
	}
	p, ok := proxies[MainGroup]
	if !ok {
		return "", nil
	}
	return p.Now, nil
}

// HealthyNames returns alive node names sorted by delay. If onlyAlive is false,
// dead nodes are appended sorted by name.
func (m *Manager) HealthyNames(ctx context.Context) (alive []string, dead []string, err error) {
	nodes, err := m.Nodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n.Name)
		} else {
			dead = append(dead, n.Name)
		}
	}
	return alive, dead, nil
}

// Recheck asks mihomo to re-test the auto group immediately.
func (m *Manager) Recheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.base+"/group/"+MainGroup+"/delay?url="+m.cfg.HealthURL+"&timeout=5000", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.ControllerSecret)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ExpectedProviders returns the provider names/paths without downloading.
func ExpectedProviders(cfg config.Config, dir string) []Provider {
	var out []Provider
	for i, s := range cfg.Subscriptions {
		name := fmt.Sprintf("sub%d", i+1)
		path := filepath.Join(dir, "mihomo", "providers", name+".yaml")
		if !subscription.IsRemote(s) {
			if abs, err := filepath.Abs(s); err == nil {
				path = abs
			}
		}
		out = append(out, Provider{Name: name, Path: path})
	}
	if len(cfg.Nodes) > 0 {
		out = append(out, Provider{Name: "custom", Path: filepath.Join(dir, "mihomo", "providers", "custom.yaml")})
	}
	return out
}

// PrepareProviders downloads remote subscriptions into local files, writes any
// custom node links into a local provider, and returns the provider list.
func (m *Manager) PrepareProviders(ctx context.Context) ([]Provider, error) {
	var out []Provider
	for i, s := range m.cfg.Subscriptions {
		name := fmt.Sprintf("sub%d", i+1)
		if subscription.IsRemote(s) {
			dst := filepath.Join(m.dir, "providers", name+".yaml")
			dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			err := subscription.DownloadTo(dctx, s, m.cfg.DownloadProxy, dst)
			cancel()
			if err != nil {
				if st, statErr := os.Stat(dst); statErr == nil && st.Size() > 0 {
					m.warn(fmt.Sprintf("subscription %d download failed (%v); using cached copy", i+1, err))
				} else {
					// No usable copy: skip this source instead of killing
					// startup; a single dead subscription must not take down
					// the proxy when other sources are fine.
					m.warn(fmt.Sprintf("subscription %d download failed (%v); skipping (no cache)", i+1, err))
					continue
				}
			}
			out = append(out, Provider{Name: name, Path: dst})
			continue
		}
		abs, err := filepath.Abs(s)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("subscription file %s: %w", s, err)
		}
		out = append(out, Provider{Name: name, Path: abs})
	}
	if len(m.cfg.Nodes) > 0 {
		proxies, errs := nodes.ParseAll(m.cfg.Nodes)
		for _, e := range errs {
			m.warn(e.Error())
		}
		if len(proxies) > 0 {
			dst := filepath.Join(m.dir, "providers", "custom.yaml")
			if err := nodes.WriteProvider(dst, proxies); err != nil {
				return nil, fmt.Errorf("write custom nodes: %w", err)
			}
			out = append(out, Provider{Name: "custom", Path: dst})
		}
	}
	if len(out) == 0 && (len(m.cfg.Subscriptions) > 0 || len(m.cfg.Nodes) > 0) {
		return nil, fmt.Errorf("no usable providers: all subscriptions failed and no custom nodes")
	}
	// No sources configured at all (first run): start anyway with DIRECT
	// fallback so the panel opens and the user can add subscriptions.
	return out, nil
}

func (m *Manager) warn(msg string) {
	m.mu.Lock()
	m.warnings = append(m.warnings, msg)
	m.mu.Unlock()
}

// Warnings returns accumulated non-fatal warnings.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.warnings...)
}

// UpdateConfig replaces the manager's config (e.g. after sources changed).
func (m *Manager) UpdateConfig(cfg config.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// RefreshProviders re-downloads subscriptions, rewrites the generated config
// (so newly added subscriptions/nodes take effect) and reloads mihomo.
func (m *Manager) RefreshProviders(ctx context.Context) error {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if len(cfg.Subscriptions) == 0 && len(cfg.Nodes) == 0 && len(cfg.Proxies) == 0 {
		return nil
	}
	providers, err := m.PrepareProviders(ctx)
	if err != nil {
		return err
	}
	m.providers = providers
	text, err := GenerateConfig(cfg, providers)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.ConfigPath(), []byte(text), 0o600); err != nil {
		return err
	}
	return m.Reload(ctx)
}

// Reload asks mihomo to reload its configuration (and thus the providers).
func (m *Manager) Reload(ctx context.Context) error {
	body := strings.NewReader(fmt.Sprintf(`{"path":%q}`, m.ConfigPath()))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.base+"/configs?force=true", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.ControllerSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("reload: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// waitPort is used by tests to confirm the mixed port is listening.
func waitPort(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
