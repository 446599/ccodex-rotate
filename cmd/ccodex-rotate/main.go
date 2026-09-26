// Command ccodex-rotate is a small, local Codex reverse proxy with health-based
// proxy-node rotation. It intentionally avoids injecting or harvesting upstream
// turn-state, which keeps it simple and stable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"ccodex-rotate/internal/codexcfg"
	"ccodex-rotate/internal/config"
	"ccodex-rotate/internal/core"
	"ccodex-rotate/internal/mihomo"
	"ccodex-rotate/internal/mtrace"
	"ccodex-rotate/internal/nodes"
	"ccodex-rotate/internal/proxy"
	"ccodex-rotate/internal/subscription"
	"ccodex-rotate/internal/web"
)

const version = "0.6.0-beta.1"

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file path")
	codexHome := fs.String("codex-home", "", "override ~/.codex directory")
	_ = fs.Parse(os.Args[2:])

	switch cmd {
	case "init":
		runInit(*cfgPath)
	case "sub":
		runSub(*cfgPath, fs.Args())
	case "proxy":
		runProxy(*cfgPath, fs.Args())
	case "node":
		runNode(*cfgPath, fs.Args())
	case "core":
		runCore(*cfgPath, fs.Args())
	case "fetch-core":
		runFetchCore(*cfgPath)
	case "serve", "run":
		runServe(*cfgPath, *codexHome)
	case "check":
		runCheck(*cfgPath)
	case "status":
		runStatus(*cfgPath)
	case "nodes":
		runNodes(*cfgPath)
	case "scan", "collect":
		runCollect(*cfgPath)
	case "restore":
		runRestore(*cfgPath, *codexHome)
	case "path", "paths":
		fmt.Println("config:", *cfgPath)
		fmt.Println("data  :", config.DataDir())
		fmt.Println("codex :", codexcfg.Path(firstNonEmpty(codexHomeString(*codexHome), "")))
	case "version", "-v", "--version":
		fmt.Println("ccodex-rotate", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`ccodex-rotate ` + version + `

  ccodex-rotate init       create a default config if none exists
  ccodex-rotate sub add <url...>     add subscription link(s)
  ccodex-rotate sub list             list subscription links (redacted)
  ccodex-rotate sub rm <index|url>   remove a subscription
  ccodex-rotate sub clear            remove all subscriptions
  ccodex-rotate proxy add <uri...>   add an explicit proxy (http/https/socks5)
  ccodex-rotate proxy list           list explicit proxies
  ccodex-rotate proxy clear          remove all explicit proxies
  ccodex-rotate fetch-core        download a mihomo core for this platform
  ccodex-rotate core [path]       show detected core, or set mihomo_path
  ccodex-rotate serve      start mihomo + local proxy + panel (Ctrl+C to stop)
  ccodex-rotate check      validate config and generated mihomo config
  ccodex-rotate status     read live status from a running instance
  ccodex-rotate nodes      list nodes and health from a running instance
  ccodex-rotate node add|list|clear   manage custom nodes
  ccodex-rotate collect    trigger a ModelTrace attribution round now
  ccodex-rotate restore    restore the Codex config backup
  ccodex-rotate paths      print config/data/codex paths

Flags:
  --config PATH      config file (default ` + config.DefaultPath() + `)
  --codex-home PATH  override ~/.codex
`)
}

func runInit(cfgPath string) {
	if _, err := os.Stat(cfgPath); err == nil {
		log.Printf("config already exists: %s", cfgPath)
		return
	}
	if err := config.Save(cfgPath, config.Default()); err != nil {
		fatal(err)
	}
	log.Printf("created %s", cfgPath)
	log.Printf("next: ccodex-rotate sub add <your-subscription-url>")
}

func runSub(cfgPath string, args []string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	if len(args) == 0 {
		log.Printf("usage: sub add|list|rm|clear")
		return
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			fatal(fmt.Errorf("usage: sub add <url...>"))
		}
		for _, u := range args[1:] {
			u = strings.TrimSpace(u)
			if u == "" || contains(cfg.Subscriptions, u) {
				continue
			}
			cfg.Subscriptions = append(cfg.Subscriptions, u)
		}
	case "list":
		if len(cfg.Subscriptions) == 0 {
			log.Printf("no subscriptions")
			return
		}
		for i, u := range cfg.Subscriptions {
			fmt.Printf("  [%d] %s\n", i, subscription.Redact(u))
		}
		return
	case "rm", "remove":
		if len(args) < 2 {
			fatal(fmt.Errorf("usage: sub rm <index|url>"))
		}
		cfg.Subscriptions = removeSub(cfg.Subscriptions, args[1])
	case "clear":
		cfg.Subscriptions = nil
	default:
		fatal(fmt.Errorf("unknown sub command %q", args[0]))
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		fatal(err)
	}
	log.Printf("saved %s (%d subscription(s)); restart `serve` to apply", cfgPath, len(cfg.Subscriptions))
}

func runProxy(cfgPath string, args []string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	if len(args) == 0 {
		log.Printf("usage: proxy add|list|clear")
		return
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			fatal(fmt.Errorf("usage: proxy add <uri...>"))
		}
		for _, u := range args[1:] {
			u = strings.TrimSpace(u)
			if u == "" || contains(cfg.Proxies, u) {
				continue
			}
			cfg.Proxies = append(cfg.Proxies, u)
		}
	case "list":
		for i, u := range cfg.Proxies {
			fmt.Printf("  [%d] %s\n", i, u)
		}
		return
	case "clear":
		cfg.Proxies = nil
	default:
		fatal(fmt.Errorf("unknown proxy command %q", args[0]))
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		fatal(err)
	}
	log.Printf("saved %s (%d explicit proxy(ies)); restart `serve` to apply", cfgPath, len(cfg.Proxies))
}

func runNode(cfgPath string, args []string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	if len(args) == 0 {
		log.Printf("usage: node add|list|clear")
		return
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			fatal(fmt.Errorf("usage: node add <ss://|vmess://|vless://|trojan://|hysteria2://|http(s)://|socks5://>"))
		}
		for _, u := range args[1:] {
			u = strings.TrimSpace(u)
			if u == "" || contains(cfg.Nodes, u) {
				continue
			}
			if _, err := nodes.Parse(u); err != nil {
				log.Printf("skip %v", err)
				continue
			}
			cfg.Nodes = append(cfg.Nodes, u)
		}
	case "list":
		for i, u := range cfg.Nodes {
			fmt.Printf("  [%d] %s\n", i, redactNode(u))
		}
		return
	case "clear":
		cfg.Nodes = nil
	default:
		fatal(fmt.Errorf("unknown node command %q", args[0]))
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		fatal(err)
	}
	log.Printf("saved %s (%d custom node(s)); restart `serve` to apply", cfgPath, len(cfg.Nodes))
}

func redactNode(u string) string {
	if i := strings.Index(u, "://"); i >= 0 && len(u) > i+16 {
		return u[:i+16] + "…"
	}
	return u
}

func runCore(cfgPath string, args []string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	if len(args) >= 1 {
		cfg.MihomoPath = strings.TrimSpace(args[0])
		if err := config.Save(cfgPath, cfg); err != nil {
			fatal(err)
		}
		log.Printf("mihomo_path set to %s", cfg.MihomoPath)
		return
	}
	if cfg.MihomoPath != "" {
		log.Printf("configured mihomo_path: %s", cfg.MihomoPath)
	}
	if p, err := mihomo.FindBinary(cfg); err == nil {
		log.Printf("detected mihomo: %s", p)
	} else {
		log.Printf("not found; run `ccodex-rotate fetch-core` or `ccodex-rotate core <path>`")
	}
}

func runFetchCore(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	dir := filepath.Join(config.DataDir(), "mihomo")
	log.Printf("downloading mihomo for this platform ...")
	path, err := core.Fetch(context.Background(), cfg.DownloadProxy, dir)
	if err != nil {
		fatal(fmt.Errorf("%w (or set mihomo_path manually: `ccodex-rotate core <path>`)", err))
	}
	cfg.MihomoPath = path
	if err := config.Save(cfgPath, cfg); err != nil {
		fatal(err)
	}
	log.Printf("mihomo downloaded: %s", path)
	log.Printf("mihomo_path saved; run `ccodex-rotate serve`")
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func removeSub(list []string, key string) []string {
	out := list[:0:0]
	for i, u := range list {
		if key == fmt.Sprint(i) || u == key {
			continue
		}
		out = append(out, u)
	}
	return out
}

func runServe(cfgPath, codexHome string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	if !cfg.HasSources() {
		log.Printf("no subscriptions/proxies yet; starting anyway. Open the panel and add a subscription:")
		log.Printf("  http://%s/panel  (or run: ccodex-rotate sub add \"https://...\")", cfg.PanelAddress())
	}
	dataDir := config.DataDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := mihomo.New(cfg, dataDir)
	if err := mgr.Start(ctx); err != nil {
		fatal(err)
	}
	defer mgr.Stop()
	if v, err := mgr.Version(ctx); err == nil {
		log.Printf("mihomo %s running (mixed :%d, controller :%d)", v, cfg.MixedPort, cfg.ControllerPort)
	}

	eg, err := mihomo.NewEgress(mgr, filepath.Join(dataDir, "node-state.json"))
	fatal(err)
	if err := eg.Init(ctx); err != nil {
		log.Printf("egress init: %v", err)
	}
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if node, changed := eg.EnsureHealthy(ctx); changed {
					log.Printf("egress switched to usable node: %s", node)
				}
			}
		}
	}()
	if cfg.SubRefreshMinutes > 0 {
		go func() {
			t := time.NewTicker(time.Duration(cfg.SubRefreshMinutes) * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := mgr.RefreshProviders(ctx); err != nil {
						log.Printf("subscription refresh: %v", err)
					} else {
						log.Printf("subscription refreshed")
					}
				}
			}
		}()
	}
	srv := proxy.New(cfg, eg, log.Printf)

	// Behavioral degradation watchdog (ModelTrace attribution): off by
	// default, each round costs three full generations.
	var panel *web.Panel
	expFn := func() string {
		if m := srv.CanonicalModel(cfg.TraceModel); m != "" {
			return m
		}
		if m := srv.CanonicalModel(cfg.ProbeModel); m != "" {
			return m
		}
		if cfg.TraceModel != "" {
			return cfg.TraceModel
		}
		return cfg.ProbeModel
	}
	var traceMu sync.Mutex
	lastTraceNotify := time.Time{}
	mon := mtrace.NewMonitor(
		func(pctx context.Context, prompt string) (string, error) {
			return srv.Challenge(pctx, expFn(), prompt)
		},
		func(msg string) {
			traceMu.Lock()
			cooled := time.Since(lastTraceNotify) < 30*time.Minute
			if !cooled {
				lastTraceNotify = time.Now()
			}
			traceMu.Unlock()
			if cooled {
				return
			}
			if cfg.NotifyEnabled {
				desktopNotify("ccodex-rotate", msg)
			}
			panel.Broadcast(msg)
		},
	)
	mon.SetEnabled(cfg.TraceEnabled)
	mon.SetInterval(time.Duration(cfg.TraceIntervalSec) * time.Second)
	go mon.RunLoop(ctx, expFn)

	panel = &web.Panel{
		Listen: cfg.Listen, Upstream: cfg.UpstreamBase,
		Version:    version,
		Password:   cfg.PanelAuthPassword(),
		ProbeModel: cfg.ProbeModel,
		Trace:      mon,
		TraceModel: cfg.TraceModel,
		Mgr:        mgr, Eg: eg, Proxy: srv,
	}
	panel.SourcesCounts = func() (int, int, int) {
		c, err := config.Load(cfgPath)
		if err != nil {
			return 0, 0, 0
		}
		return len(c.Subscriptions), len(c.Nodes), len(c.Proxies)
	}
	panel.SourcesAdd = func(kind string, lines []string) (int, error) {
		c, err := config.Load(cfgPath)
		if err != nil {
			return 0, err
		}
		added := 0
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			switch kind {
			case "sub":
				if !contains(c.Subscriptions, l) {
					c.Subscriptions = append(c.Subscriptions, l)
					added++
				}
			case "node":
				if _, err := nodes.Parse(l); err != nil {
					continue
				}
				if !contains(c.Nodes, l) {
					c.Nodes = append(c.Nodes, l)
					added++
				}
			}
		}
		if err := config.Save(cfgPath, c); err != nil {
			return added, err
		}
		mgr.UpdateConfig(c)
		if err := mgr.RefreshProviders(ctx); err != nil {
			log.Printf("refresh providers: %v", err)
		}
		if err := eg.RefreshNodes(ctx); err != nil {
			log.Printf("refresh nodes: %v", err)
		}
		return added, nil
	}
	panel.SourcesClear = func(kind string) error {
		c, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		switch kind {
		case "sub":
			c.Subscriptions = nil
		case "node":
			c.Nodes = nil
		}
		if err := config.Save(cfgPath, c); err != nil {
			return err
		}
		mgr.UpdateConfig(c)
		if err := mgr.RefreshProviders(ctx); err != nil {
			log.Printf("refresh providers: %v", err)
		}
		return eg.RefreshNodes(ctx)
	}
	root := http.NewServeMux()
	root.Handle("/backend-api/codex", srv)
	root.Handle("/backend-api/codex/", srv)
	root.Handle("/healthz", srv)
	panelHandler := panel.Handler()
	var panelSrv *http.Server
	if cfg.PanelListen == "" {
		root.Handle("/", panelHandler)
	} else {
		panelSrv = &http.Server{Addr: cfg.PanelListen, Handler: panelHandler, ReadHeaderTimeout: 10 * time.Second}
	}

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: root, ReadHeaderTimeout: 10 * time.Second}

	wired := false
	cfgFile := codexcfg.Path(firstNonEmpty(cfg.CodexHome, codexHome))
	if cfg.AutoConfigCodex {
		base := "http://" + cfg.Listen + "/backend-api/codex"
		if _, err := codexcfg.Setup(cfgFile, base); err != nil {
			log.Printf("codex config not patched: %v", err)
		} else {
			wired = true
			log.Printf("codex wired to %s (backup: %s)", base, codexcfg.BackupPath(cfgFile))
		}
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(err)
		}
	}()
	if panelSrv != nil {
		go func() {
			if err := panelSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatal(err)
			}
		}()
	}
	log.Printf("proxy: http://%s/backend-api/codex   panel: http://%s/panel", cfg.Listen, cfg.PanelAddress())
	log.Printf("press Ctrl+C to stop")
	go openBrowser("http://" + cfg.PanelAddress() + "/panel")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down...")
	sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	_ = httpSrv.Shutdown(sctx)
	if panelSrv != nil {
		_ = panelSrv.Shutdown(sctx)
	}
	mgr.Stop()
	if wired && cfg.RestoreOnExit {
		if err := codexcfg.Restore(cfgFile); err == nil {
			log.Printf("codex config restored")
		}
	}
}

func runCheck(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	if !cfg.HasSources() {
		log.Printf("WARN: no subscriptions/proxies yet; add one with `sub add` or in the panel")
	}
	bin, err := mihomo.FindBinary(cfg)
	if err != nil {
		log.Printf("WARN: %v", err)
	} else {
		log.Printf("mihomo binary: %s", bin)
	}
	text, err := mihomo.GenerateConfig(cfg, mihomo.ExpectedProviders(cfg, config.DataDir()))
	fatal(err)
	log.Printf("generated mihomo config (%d bytes) is valid YAML-to-be", len(text))
	log.Printf("health url: %s (expected %s)", cfg.HealthURL, cfg.HealthExpected)
	log.Printf("listen %s -> upstream %s via mixed :%d", cfg.Listen, cfg.UpstreamBase, cfg.MixedPort)
	cfgFile := codexcfg.Path(cfg.CodexHome)
	base := "http://" + cfg.Listen + "/backend-api/codex"
	if codexcfg.IsWired(cfgFile, base) {
		log.Printf("codex config points at this proxy: %s", cfgFile)
	} else {
		log.Printf("codex config not wired yet (is served only while `serve` runs)")
	}
}

func runStatus(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	body, err := getJSON(cfg, "/api/status")
	fatal(err)
	fmt.Println(pretty(body))
}

func runNodes(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	body, err := getJSON(cfg, "/api/nodes")
	fatal(err)
	var payload struct {
		Current string `json:"current"`
		Nodes   []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Delay int    `json:"delay"`
			Alive bool   `json:"alive"`
		} `json:"nodes"`
	}
	fatal(json.Unmarshal(body, &payload))
	fmt.Printf("current: %s\n", payload.Current)
	for _, n := range payload.Nodes {
		state := "down"
		if n.Alive {
			state = fmt.Sprintf("%dms", n.Delay)
		}
		fmt.Printf("  %-40s %-10s %s\n", n.Name, n.Type, state)
	}
}

func runCollect(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	_, err = panelRequest(cfg, http.MethodPost, "/api/trace-now")
	fatal(err)
	log.Printf("attribution round started; watch it with `ccodex-rotate status`")
}

func runRestore(cfgPath, codexHome string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	p := codexcfg.Path(firstNonEmpty(cfg.CodexHome, codexHome))
	if err := codexcfg.Restore(p); err != nil {
		fatal(err)
	}
	log.Printf("restored %s", p)
}

func getJSON(cfg config.Config, path string) ([]byte, error) {
	return panelRequest(cfg, http.MethodGet, path)
}

func panelRequest(cfg config.Config, method, path string) ([]byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(method, "http://"+cfg.PanelAddress()+path, nil)
	if err != nil {
		return nil, err
	}
	if password := cfg.PanelAuthPassword(); password != "" {
		req.SetBasicAuth("admin", password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("is `serve` running? %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("panel returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func pretty(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(b)
	}
	return string(out)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func codexHomeString(s string) string { return s }

func fatal(err error) {
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

// desktopNotify pops a best-effort OS notification in the background.
func desktopNotify(title, body string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.CommandContext(ctx, "osascript", "-e",
				`display notification "`+osascriptEscape(body)+`" with title "`+osascriptEscape(title)+`" sound name "Glass"`)
		case "windows":
			ps := `(New-Object -ComObject WScript.Shell).Popup('` + psEscape(body) + `', 8, '` + psEscape(title) + `', 64)`
			cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", ps)
		default:
			cmd = exec.CommandContext(ctx, "notify-send", title, body)
		}
		_ = cmd.Run()
	}()
}

func osascriptEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func psEscape(s string) string { return strings.ReplaceAll(s, `'`, `''`) }

// openBrowser opens a URL in the default browser on macOS/Windows/Linux.
func openBrowser(url string) {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	args = append(args, url)
	_ = exec.Command(name, args...).Start()
}
