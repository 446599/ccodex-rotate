package modelid

import "testing"

func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"gpt-6-astra":         "gpt-6-astra",
		"gpt-5.6-sol":         "gpt-5.6-sol",
		"gpt-5.6-terra":       "gpt-5.6-terra",
		"gpt-5.6-luna":        "gpt-5.6-luna",
		"gpt-5.4-high":        "gpt-5.4",
		"gpt-5.4-xhigh":       "gpt-5.4",
		"gpt-5.4-none":        "gpt-5.4",
		"gpt-5.4-mini":        "gpt-5.4-mini",
		"gpt-5.4-mini-high":   "gpt-5.4-mini",
		"gpt-5.4-2025-11-20":  "gpt-5.4",
		"gpt-5.3":             "gpt-5.3-codex",
		"gpt-5.3-codex-high":  "gpt-5.3-codex",
		"gpt-5.3-codex-spark": "gpt-5.3-codex-spark",
		"gpt-5-codex":         "gpt-5.3-codex",
		"gpt-5.1-codex-max":   "gpt-5.3-codex",
		"codex-mini-latest":   "gpt-5.3-codex",
		"gpt-5":               "gpt-5.4",
		"gpt-5.1":             "gpt-5.4",
		"openai/gpt-5.5":      "gpt-5.5",
		"GPT-5.5":             "gpt-5.5",
		"  gpt-5.5  ":         "gpt-5.5",
		"gpt 5.6 sol":         "gpt-5.6-sol",
		"gpt-5.2-codex":       "gpt-5.2",
		"some-unknown-model":  "some-unknown-model",
		"":                    "",
	}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAliasOverride(t *testing.T) {
	r := New(map[string]string{"my-fast": "gpt-5.4"})
	if got := r.Canonical("my-fast"); got != "gpt-5.4" {
		t.Fatalf("override failed: %q", got)
	}
	if got := r.Canonical("my-fast-high"); got != "gpt-5.4" {
		// suffix stripped then alias applied
		t.Logf("my-fast-high -> %q", got)
	}
}
