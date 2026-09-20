package codexcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplySetsKeysAndProvider(t *testing.T) {
	in := "model = \"gpt-x\"\n[desktop]\nfoo = 1\n"
	out := Apply(in, "http://127.0.0.1:17850/backend-api/codex")
	for _, want := range []string{
		`openai_base_url = "http://127.0.0.1:17850/backend-api/codex"`,
		`model_provider = "ccodex-rotate"`,
		"[model_providers.ccodex-rotate]",
		`model = "gpt-x"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Existing provider table must be updated, not skipped (bug 1).
func TestApplyUpdatesExistingProvider(t *testing.T) {
	in := "[model_providers.ccodex-rotate]\n" +
		`base_url = "https://api.openai.com/v1"` + "\n" +
		"name = \"old\"\n\n[desktop]\nfoo = 1\n"
	out := Apply(in, "http://127.0.0.1:17850/backend-api/codex")
	if strings.Contains(out, "api.openai.com") {
		t.Fatalf("stale base_url was not updated:\n%s", out)
	}
	if strings.Count(out, "[model_providers.ccodex-rotate]") != 1 {
		t.Fatalf("provider table duplicated:\n%s", out)
	}
	if !strings.Contains(out, `base_url = "http://127.0.0.1:17850/backend-api/codex"`) {
		t.Fatalf("base_url not corrected:\n%s", out)
	}
	if !strings.Contains(out, "foo = 1") {
		t.Fatalf("unrelated table lost:\n%s", out)
	}
}

func TestApplyIdempotent(t *testing.T) {
	in := "model = \"gpt-x\"\n"
	once := Apply(in, "http://127.0.0.1:1/backend-api/codex")
	twice := Apply(once, "http://127.0.0.1:1/backend-api/codex")
	if once != twice {
		t.Errorf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// IsWired must verify the real provider, not just contain a local address (bug 2).
func TestIsWiredChecksProvider(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	listen := "http://127.0.0.1:17850/backend-api/codex"

	// Local address only in a comment / other key: not wired.
	os.WriteFile(p, []byte("# see 127.0.0.1:17850\nmodel_provider = \"openai\"\n"), 0o600)
	if IsWired(p, listen) {
		t.Error("should be false when provider is not ours")
	}

	// Provider ours but base_url points elsewhere: not wired.
	os.WriteFile(p, []byte("model_provider = \"ccodex-rotate\"\n[model_providers.ccodex-rotate]\nbase_url = \"https://api.openai.com/v1\"\n"), 0o600)
	if IsWired(p, listen) {
		t.Error("should be false when base_url does not match")
	}

	// Fully wired.
	wired := Apply("model = \"x\"\n", listen)
	os.WriteFile(p, []byte(wired), 0o600)
	if !IsWired(p, listen) {
		t.Error("should be true when provider and base_url match")
	}
}

// Restore must preserve unrelated changes made while running (bug 3).
func TestRestorePreservesOtherChanges(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	original := "model = \"gpt-x\"\n\n[desktop]\nfoo = 1\n"
	os.WriteFile(cfg, []byte(original), 0o600)

	if _, err := Setup(cfg, "http://127.0.0.1:17850/backend-api/codex"); err != nil {
		t.Fatal(err)
	}
	// Simulate a change made during the run (a new table added by Codex/user).
	cur, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, append(cur, []byte("\n[projects.\"/tmp/x\"]\ntrust_level = \"trusted\"\n")...), 0o600)

	if err := Restore(cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	s := string(got)
	if strings.Contains(s, "ccodex-rotate") {
		t.Errorf("managed keys/table not reverted:\n%s", s)
	}
	if !strings.Contains(s, "foo = 1") {
		t.Errorf("original settings lost:\n%s", s)
	}
	if !strings.Contains(s, "trust_level = \"trusted\"") {
		t.Errorf("run-time change was overwritten:\n%s", s)
	}
}

func TestSetupBackupIsStable(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte("model = \"gpt-x\"\n"), 0o600)
	Setup(cfg, "http://127.0.0.1:9/backend-api/codex")
	Setup(cfg, "http://127.0.0.1:10/backend-api/codex")
	backup, _ := os.ReadFile(BackupPath(cfg))
	if string(backup) != "model = \"gpt-x\"\n" {
		t.Fatalf("backup should remain the original: %q", string(backup))
	}
}
