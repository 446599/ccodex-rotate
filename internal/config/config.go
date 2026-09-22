// Package config loads and saves ccodex-rotate's settings.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the persistent configuration for ccodex-rotate.
type Config struct {
	// Listen is the address of the local Codex-facing reverse proxy.
	Listen string `json:"listen"`
	// UpstreamBase is the real Codex backend origin (no trailing slash).
	UpstreamBase string `json:"upstream_base"`
	// MixedPort is the local mihomo mixed (HTTP/SOCKS) port used for forwarding.
	MixedPort int `json:"mixed_port"`
	// CollectPort is a second mihomo inbound used only for credential
	// collection, bound to the COLLECT group, so a manual forward choice never
	// affects collection.
	CollectPort int `json:"collect_port"`
	// ControllerPort is the local mihomo external-controller port.
	ControllerPort int `json:"controller_port"`
	// ControllerSecret protects the mihomo controller.
	ControllerSecret string `json:"controller_secret"`
	// MihomoPath is the mihomo binary. Empty means auto-detect.
	MihomoPath string `json:"mihomo_path"`
	// Subscriptions are subscription URLs or local files (Clash/YAML/base64/URI list).
	Subscriptions []string `json:"subscriptions"`
	// Proxies are explicit proxy URIs (http/https/socks5) added to the pool.
	Proxies []string `json:"proxies"`
	// Nodes are custom node share links (ss/vmess/vless/trojan/hysteria2/...)
	// converted into a local Clash provider file.
	Nodes []string `json:"nodes"`
	// DownloadProxy is used to fetch subscriptions. Empty tries environment
	// proxies and then common local ports. Use "direct" to force a direct fetch.
	DownloadProxy string `json:"download_proxy"`
	// SubRefreshMinutes re-downloads subscriptions periodically (0 disables).
	SubRefreshMinutes int `json:"subscription_refresh_minutes"`

	// HealthURL is probed through every node; a node is healthy when it
	// returns HealthExpected. Unauthenticated Codex returns 401 when the
	// node is usable, and 403 when the exit is blocked.
	HealthURL string `json:"health_url"`
	// HealthExpected is the expected HTTP status expression, e.g. "401".
	HealthExpected string `json:"health_expected"`
	// HealthIntervalSec is how often mihomo re-checks each node.
	HealthIntervalSec int `json:"health_interval_seconds"`
	// TestIntervalSec is how often the auto url-test group picks the best node.
	TestIntervalSec int `json:"test_interval_seconds"`
	// ProbeEnabled uses the captured account auth to actively collect a
	// turn-state from nodes (this consumes a little quota).
	ProbeEnabled bool `json:"probe_enabled"`
	// ProbeModel is the model used for collection probes.
	ProbeModel string `json:"probe_model"`
	// ProbeTimeoutSec is the per-node probe timeout.
	ProbeTimeoutSec int `json:"probe_timeout_seconds"`
	// MaxProbesPerCollect caps how many nodes one collection round tries.
	// 0 means "try nodes one at a time until one yields the target state".
	// Bounded by default: a full 108-node sweep every 5 minutes burns
	// through the account's routing budget and can close astra windows.
	MaxProbesPerCollect int `json:"max_probes_per_collect"`
	// CollectLanes is how many independent collection lanes exist (each a
	// dedicated mihomo select group + inbound port), so parallel collection
	// can probe that many exits at once without sharing one group
	// selection. Lane i uses CollectPort+i. Default 10.
	CollectLanes int `json:"collect_lanes"`
	// LoopPauseSec is the pause between parallel-loop rounds, to stay under
	// upstream rate limits (tight loops earn HTTP 429s). Default 10.
	LoopPauseSec int `json:"loop_pause_seconds"`
	// CollectOnStart collects once as soon as account auth is available.
	CollectOnStart bool `json:"collect_on_start"`
	// AutoCollect starts collection automatically when a target-model request
	// arrives (i.e. after you send a message). Default true.
	AutoCollect bool `json:"auto_collect"`
	// CollectModels are additional models to collect a turn-state for, on top of
	// ProbeModel. Default includes Codex's review model.
	CollectModels []string `json:"collect_models"`
	// CollectSuccessIntervalSec is the delay after a successful collection.
	CollectSuccessIntervalSec int `json:"collect_success_interval_seconds"`
	// CollectRetryIntervalSec is the delay after a failed collection.
	CollectRetryIntervalSec int `json:"collect_retry_interval_seconds"`
	// HuntEnabled runs a lightweight rotating probe that catches "astra
	// windows": upstream routing rotates every few minutes, so the hunter
	// re-checks a few exits and moves forwarding onto one serving the
	// target model. Each probe is one tiny request.
	HuntEnabled bool `json:"hunt_enabled"`
	// HuntIntervalSec is how often the hunter runs.
	HuntIntervalSec int `json:"hunt_interval_seconds"`
	// HuntNodes caps how many exits one hunt round probes.
	HuntNodes int `json:"hunt_nodes"`
	// NotifyEnabled pops a desktop notification when an astra window opens.
	NotifyEnabled bool `json:"notify_enabled"`

	// Selection is "auto" (fastest healthy) or "manual".
	Selection string `json:"selection"`

	// MaxRetries is the number of failover attempts per request.
	MaxRetries int `json:"max_retries"`
	// MaxBodyMiB caps the buffered request body that can be replayed on retry.
	MaxBodyMiB int `json:"max_body_mib"`

	// InjectState re-injects a cached X-Codex-Turn-State (bound to one node).
	InjectState bool `json:"inject_state"`
	// StateTTLSeconds is how long a cached turn-state stays usable.
	StateTTLSeconds int `json:"state_ttl_seconds"`
	// StateLengths is the target state length(s): 292 for personal, 332 for
	// Team/Business. A node returning one of these is "good"; its value is
	// cached and injected.
	StateLengths []int `json:"state_lengths"`
	// InjectNodeAffinity, when true, only injects a state through the node that
	// produced it. Default false: 292 may be injected across nodes.
	InjectNodeAffinity bool `json:"inject_node_affinity"`
	// CookiePin, when true, harvests upstream Set-Cookie pairs alongside a
	// 292 turn-state and replays them as a Cookie header while the bundle
	// is fresh (see CredTTLSeconds). Default true.
	CookiePin bool `json:"cookie_pin_enabled"`
	// CookieRefreshAll, when true, lets every upstream response (even 312s)
	// refresh the cookie jar. Default false (frozen mode): only the exact
	// cookie set arriving with a 292 is replayable, because every response
	// mints a unique set and 312s must not overwrite the 292 set.
	CookieRefreshAll bool `json:"cookie_refresh_all"`
	// CredTTLSeconds is the freshness window of a harvested credential
	// bundle (292 value + cookies). Inside the window both are injected;
	// after it a newly harvested bundle replaces the old one, or an
	// expiry notification fires if none arrived. Default 240.
	CredTTLSeconds int `json:"cred_ttl_seconds"`
	// CredRefreshPauseSec is the pause after a successful 292 collection
	// before collecting again, keeping the bundle fresh. Default 30.
	CredRefreshPauseSec int `json:"cred_refresh_pause_seconds"`
	// ModelAliases maps a requested model name to its canonical name so
	// turn-state keys, probing and injection agree on one identifier.
	ModelAliases map[string]string `json:"model_aliases"`
	// ForceModel, when set, rewrites every request body's "model" to this value
	// before forwarding (e.g. force gpt-6-astra even if the client asks for
	// another model such as gpt-5.6-luna).
	ForceModel string `json:"force_model"`

	// TimeoutSec is the per-attempt upstream timeout.
	TimeoutSec int `json:"timeout_seconds"`

	// AutoConfigCodex patches ~/.codex/config.toml on setup.
	AutoConfigCodex bool `json:"auto_config_codex"`
	// RestoreOnExit reverts the Codex config on clean shutdown.
	RestoreOnExit bool `json:"restore_on_exit"`
	// CodexHome overrides the default ~/.codex directory.
	CodexHome string `json:"codex_home"`
}

// Default returns a config with sensible defaults for a fresh install.
func Default() Config {
	return Config{
		Listen:                    "127.0.0.1:17850",
		UpstreamBase:              "https://chatgpt.com",
		MixedPort:                 17890,
		CollectPort:               17892,
		ControllerPort:            17891,
		ControllerSecret:          randomSecret(),
		Subscriptions:             nil,
		Proxies:                   nil,
		SubRefreshMinutes:         60,
		HealthURL:                 "https://chatgpt.com/backend-api/codex/models?client_version=0.0.0",
		HealthExpected:            "*",
		HealthIntervalSec:         300,
		TestIntervalSec:           60,
		ProbeEnabled:              true,
		ProbeModel:                "gpt-6-astra",
		CollectModels:             []string{"codex-auto-review"},
		ProbeTimeoutSec:           12,
		MaxProbesPerCollect:       25,
		CollectLanes:              10,
		LoopPauseSec:              10,
		CollectOnStart:            true,
		CollectSuccessIntervalSec: 1800,
		CollectRetryIntervalSec:   300,
		HuntEnabled:               true,
		HuntIntervalSec:           120,
		HuntNodes:                 4,
		NotifyEnabled:             true,
		InjectNodeAffinity:        false,
		CookiePin:                 true,
		CredTTLSeconds:            240,
		CredRefreshPauseSec:       30,
		Selection:                 "auto",
		MaxRetries:                3,
		MaxBodyMiB:                128,
		TimeoutSec:                120,
		InjectState:               true,
		StateTTLSeconds:           240,
		StateLengths:              []int{292, 332},
		AutoCollect:               true,
		AutoConfigCodex:           true,
		RestoreOnExit:             true,
	}
}

// DataDir returns the directory holding config, generated mihomo files and logs.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("CCODEX_ROTATE_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ccodex-rotate"
	}
	return filepath.Join(home, ".ccodex-rotate")
}

// DefaultPath is the config file path inside DataDir.
func DefaultPath() string { return filepath.Join(DataDir(), "config.json") }

// Load reads the config, applying defaults for missing fields.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.normalize(path); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Save writes the config atomically.
func Save(path string, cfg Config) error {
	if err := cfg.normalize(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) normalize(path string) error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:17850"
	}
	if c.UpstreamBase == "" {
		c.UpstreamBase = "https://chatgpt.com"
	}
	c.UpstreamBase = strings.TrimRight(c.UpstreamBase, "/")
	if c.MixedPort == 0 {
		c.MixedPort = 17890
	}
	if c.CollectPort == 0 {
		c.CollectPort = 17892
	}
	if c.ControllerPort == 0 {
		c.ControllerPort = 17891
	}
	if c.ControllerSecret == "" {
		c.ControllerSecret = randomSecret()
	}
	if c.HealthURL == "" {
		c.HealthURL = c.UpstreamBase + "/backend-api/codex/models?client_version=0.0.0"
	}
	if c.HealthExpected == "" {
		c.HealthExpected = "401"
	}
	if c.HealthIntervalSec <= 0 {
		c.HealthIntervalSec = 300
	}
	if c.TestIntervalSec <= 0 {
		c.TestIntervalSec = 60
	}
	if c.Selection == "" {
		c.Selection = "auto"
	}
	if c.Selection != "auto" && c.Selection != "manual" {
		return fmt.Errorf("selection must be auto or manual, got %q", c.Selection)
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.MaxBodyMiB <= 0 {
		c.MaxBodyMiB = 128
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = 120
	}
	if c.SubRefreshMinutes < 0 {
		c.SubRefreshMinutes = 0
	}
	if c.StateTTLSeconds <= 0 {
		c.StateTTLSeconds = 240
	}
	if c.CredTTLSeconds <= 0 {
		c.CredTTLSeconds = 240
	}
	if c.CredRefreshPauseSec <= 0 {
		c.CredRefreshPauseSec = 30
	}
	if c.ProbeModel == "" {
		c.ProbeModel = "gpt-6-astra"
	}
	if c.ProbeTimeoutSec <= 0 {
		c.ProbeTimeoutSec = 12
	}
	if c.MaxProbesPerCollect < 0 {
		c.MaxProbesPerCollect = 0
	}
	if c.CollectLanes <= 0 {
		c.CollectLanes = 10
	}
	if c.CollectLanes > 32 {
		c.CollectLanes = 32
	}
	if c.LoopPauseSec <= 0 {
		c.LoopPauseSec = 10
	}
	if c.CollectSuccessIntervalSec <= 0 {
		c.CollectSuccessIntervalSec = 1800
	}
	if c.CollectRetryIntervalSec <= 0 {
		c.CollectRetryIntervalSec = 300
	}
	if c.HuntIntervalSec <= 0 {
		c.HuntIntervalSec = 120
	}
	if c.HuntNodes <= 0 {
		c.HuntNodes = 4
	}
	_ = path
	return nil
}

// HasSources reports whether any egress source is configured.
func (c Config) HasSources() bool { return len(c.Subscriptions) > 0 || len(c.Proxies) > 0 }

func randomSecret() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "ccodex-rotate"
	}
	return hex.EncodeToString(b)
}
