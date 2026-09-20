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
	if !strings.Contains(out, `openai_base_url = "http://127.0.0.1:17850/backend-api/codex"`) {
		t.Errorf("missing base url:\n%s", out)
	}
	if !strings.Contains(out, `model_provider = "ccodex-rotate"`) {
		t.Errorf("missing model_provider:\n%s", out)
	}
	if !strings.Contains(out, "[model_providers.ccodex-rotate]") {
		t.Errorf("missing provider table:\n%s", out)
	}
	if !strings.Contains(out, `model = "gpt-x"`) {
		t.Errorf("original content lost:\n%s", out)
	}
}

func TestApplyReplacesExistingKeys(t *testing.T) {
	in := "openai_base_url = \"http://old\"\nmodel_provider = \"openai\"\n"
	out := Apply(in, "http://127.0.0.1:17850/backend-api/codex")
	if strings.Count(out, "openai_base_url") != 1 {
		t.Errorf("openai_base_url should appear once:\n%s", out)
	}
	if strings.Contains(out, `"http://old"`) {
		t.Errorf("old base url should be replaced:\n%s", out)
	}
}

func TestApplyIdempotent(t *testing.T) {
	in := "model = \"gpt-x\"\n"
	once := Apply(in, "http://127.0.0.1:1/backend-api/codex")
	twice := Apply(once, "http://127.0.0.1:1/backend-api/codex")
	if once != twice {
		t.Errorf("Apply is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestSetupAndRestore(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	original := "model = \"gpt-x\"\n"
	if err := os.WriteFile(cfg, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(cfg, "http://127.0.0.1:9/backend-api/codex"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !IsWired(cfg, "127.0.0.1:9") {
		t.Error("IsWired should be true after Setup")
	}
	if _, err := os.Stat(BackupPath(cfg)); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	// Running setup twice must not clobber the original backup.
	if _, err := Setup(cfg, "http://127.0.0.1:10/backend-api/codex"); err != nil {
		t.Fatal(err)
	}
	if err := Restore(cfg); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if string(got) != original {
		t.Errorf("restore mismatch:\nwant %q\ngot  %q", original, string(got))
	}
}
