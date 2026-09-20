// Package codexcfg patches ~/.codex/config.toml to route Codex through
// ccodex-rotate, keeping a restorable backup.
package codexcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const ProviderName = "ccodex-rotate"

// Path resolves the Codex config path.
func Path(codexHome string) string {
	if codexHome == "" {
		if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
			codexHome = v
		} else if home, err := os.UserHomeDir(); err == nil {
			codexHome = filepath.Join(home, ".codex")
		}
	}
	return filepath.Join(codexHome, "config.toml")
}

// BackupPath is the sidecar backup created before the first patch.
func BackupPath(cfgPath string) string { return cfgPath + ".ccodex-rotate.bak" }

// Apply patches the config text to point at listen. It is idempotent.
func Apply(text, listen string) string {
	lines := strings.Split(text, "\n")
	var out []string
	setBase, setProv := false, false
	inTop := true
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			inTop = false
		}
		if inTop && !strings.HasPrefix(t, "#") && strings.Contains(t, "=") {
			key := strings.TrimSpace(strings.SplitN(t, "=", 2)[0])
			switch key {
			case "openai_base_url":
				out = append(out, fmt.Sprintf("openai_base_url = %q", listen))
				setBase = true
				continue
			case "model_provider":
				out = append(out, fmt.Sprintf("model_provider = %q", ProviderName))
				setProv = true
				continue
			}
		}
		out = append(out, ln)
	}
	var header []string
	if !setBase {
		header = append(header, fmt.Sprintf("openai_base_url = %q", listen))
	}
	if !setProv {
		header = append(header, fmt.Sprintf("model_provider = %q", ProviderName))
	}
	text = strings.Join(append(header, out...), "\n")
	if !strings.Contains(text, "[model_providers."+ProviderName+"]") {
		text = strings.TrimRight(text, "\n") + "\n\n[model_providers." + ProviderName + "]\n" +
			fmt.Sprintf("base_url = %q\n", listen) +
			fmt.Sprintf("name = %q\n", ProviderName) +
			"wire_api = \"responses\"\n" +
			"requires_openai_auth = true\n" +
			"request_max_retries = 0\n" +
			"stream_max_retries = 0\n"
	}
	return text
}

// Setup backs up and rewrites the Codex config.
func Setup(cfgPath, listen string) (string, error) {
	original, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", cfgPath, err)
	}
	backup := BackupPath(cfgPath)
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if err := os.WriteFile(backup, original, 0o600); err != nil {
			return "", fmt.Errorf("write backup: %w", err)
		}
	}
	patched := Apply(string(original), listen)
	if err := os.WriteFile(cfgPath, []byte(patched), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", cfgPath, err)
	}
	return backup, nil
}

// Restore copies the backup back over the config.
func Restore(cfgPath string) error {
	backup := BackupPath(cfgPath)
	data, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("no backup at %s: %w", backup, err)
	}
	return os.WriteFile(cfgPath, data, 0o600)
}

// IsWired reports whether the config already points at listen.
func IsWired(cfgPath, listen string) bool {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), listen)
}
