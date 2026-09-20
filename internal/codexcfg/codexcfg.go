// Package codexcfg patches ~/.codex/config.toml to route Codex through
// ccodex-rotate, keeping a restorable backup. It edits only the keys it owns so
// unrelated settings are preserved, and it verifies the real provider rather
// than guessing from a substring.
package codexcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const ProviderName = "ccodex-rotate"

const providerTable = "model_providers." + ProviderName

// providerKeys are the keys this tool owns inside its provider table.
var providerKeys = map[string]bool{
	"base_url":             true,
	"name":                 true,
	"wire_api":             true,
	"requires_openai_auth": true,
	"request_max_retries":  true,
	"stream_max_retries":   true,
}

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

func quote(s string) string { return strconv.Quote(s) }

func keyOf(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
		return ""
	}
	if i := strings.Index(line, "="); i >= 0 {
		return strings.TrimSpace(line[:i])
	}
	return ""
}

func providerBody(listen string) []string {
	return []string{
		"base_url = " + quote(listen),
		`name = "` + ProviderName + `"`,
		`wire_api = "responses"`,
		`requires_openai_auth = true`,
		`request_max_retries = 0`,
		`stream_max_retries = 0`,
	}
}

// Apply patches the config text to point at listen. It is idempotent and:
//   - sets top-level openai_base_url / model_provider,
//   - creates OR updates the managed provider table so its base_url is always
//     correct (an existing stale table is fixed, not skipped),
//   - leaves every other key and table untouched.
func Apply(text, listen string) string {
	lines := strings.Split(text, "\n")
	var out []string
	section := ""
	haveProvider := false
	setBase, setProv := false, false

	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			name := strings.TrimSpace(t[1 : len(t)-1])
			section = name
			if name == providerTable {
				haveProvider = true
				out = append(out, ln)
				out = append(out, providerBody(listen)...)
				continue
			}
			out = append(out, ln)
			continue
		}
		if section == providerTable {
			if providerKeys[keyOf(ln)] {
				continue // drop old managed keys; canonical ones were just written
			}
			out = append(out, ln)
			continue
		}
		if section == "" {
			switch keyOf(ln) {
			case "openai_base_url":
				out = append(out, "openai_base_url = "+quote(listen))
				setBase = true
				continue
			case "model_provider":
				out = append(out, `model_provider = `+quote(ProviderName))
				setProv = true
				continue
			}
		}
		out = append(out, ln)
	}

	var header []string
	if !setBase {
		header = append(header, "openai_base_url = "+quote(listen))
	}
	if !setProv {
		header = append(header, `model_provider = `+quote(ProviderName))
	}
	res := strings.Join(append(header, out...), "\n")
	if !haveProvider {
		res = strings.TrimRight(res, "\n") + "\n\n[" + providerTable + "]\n" +
			strings.Join(providerBody(listen), "\n") + "\n"
	}
	return res
}

// scan returns top-level scalar keys and the managed provider table's keys.
func scan(text string) (top, provider map[string]string) {
	top = map[string]string{}
	provider = map[string]string{}
	section := ""
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.TrimSpace(t[1 : len(t)-1])
			continue
		}
		k := keyOf(ln)
		if k == "" {
			continue
		}
		raw := strings.TrimSpace(strings.SplitN(strings.TrimSpace(ln), "=", 2)[1])
		val := unquote(raw)
		switch section {
		case "":
			top[k] = val
		case providerTable:
			provider[k] = val
		}
	}
	return top, provider
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[0] == s[len(s)-1] {
		if s[0] == '"' {
			if v, err := strconv.Unquote(s); err == nil {
				return v
			}
		}
		return s[1 : len(s)-1]
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	return s
}

// IsWired reports whether the config really routes through our proxy: the active
// model_provider must be ours AND its base_url must equal listen.
func IsWired(cfgPath, listen string) bool {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false
	}
	top, provider := scan(string(data))
	if top["model_provider"] != ProviderName {
		return false
	}
	return provider["base_url"] == listen
}

// ActiveBaseURL returns the base_url Codex currently uses, if resolvable.
func ActiveBaseURL(cfgPath string) (string, bool) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", false
	}
	top, provider := scan(string(data))
	if top["model_provider"] == ProviderName {
		if b, ok := provider["base_url"]; ok {
			return b, true
		}
	}
	if b, ok := top["openai_base_url"]; ok && b != "" {
		return b, true
	}
	return "", false
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

// Restore reverts only the keys this tool changed (top-level openai_base_url /
// model_provider and the managed provider table), preserving any other edits
// made while the service was running.
func Restore(cfgPath string) error {
	backupData, err := os.ReadFile(BackupPath(cfgPath))
	if err != nil {
		return fmt.Errorf("no backup at %s: %w", BackupPath(cfgPath), err)
	}
	current, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}
	topBackup, _ := scan(string(backupData))

	cur := string(current)
	cur = setTopLevel(cur, "openai_base_url", topBackup["openai_base_url"], hasKey(topBackup, "openai_base_url"))
	cur = setTopLevel(cur, "model_provider", topBackup["model_provider"], hasKey(topBackup, "model_provider"))
	cur = removeTable(cur, providerTable)

	return os.WriteFile(cfgPath, []byte(cur), 0o600)
}

func hasKey(m map[string]string, k string) bool { _, ok := m[k]; return ok }

// setTopLevel replaces (or removes) a top-level key without touching tables.
func setTopLevel(text, key, value string, present bool) string {
	lines := strings.Split(text, "\n")
	var out []string
	section := ""
	done := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.TrimSpace(t[1 : len(t)-1])
			out = append(out, ln)
			continue
		}
		if section == "" && keyOf(ln) == key {
			if present && !done {
				out = append(out, key+" = "+quote(value))
				done = true
			}
			continue // drop old line (removal or replaced)
		}
		out = append(out, ln)
	}
	res := strings.Join(out, "\n")
	if present && !done {
		res = key + " = " + quote(value) + "\n" + res
	}
	return res
}

// removeTable removes a [name] table and everything up to the next table.
func removeTable(text, name string) string {
	lines := strings.Split(text, "\n")
	var out []string
	skipping := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur := strings.TrimSpace(t[1 : len(t)-1])
			skipping = cur == name
			if skipping {
				continue
			}
		}
		if skipping {
			continue
		}
		out = append(out, ln)
	}
	// collapse excessive blank lines
	res := strings.Join(out, "\n")
	for strings.Contains(res, "\n\n\n") {
		res = strings.ReplaceAll(res, "\n\n\n", "\n\n")
	}
	return res
}
