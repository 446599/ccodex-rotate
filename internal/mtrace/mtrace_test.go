package mtrace

import (
	"math/rand"
	"strings"
	"testing"
)

func TestBankLoads(t *testing.T) {
	b, err := LoadBank()
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Robust.ModelOrder) != 16 {
		t.Fatalf("expected 16 models, got %d", len(b.Robust.ModelOrder))
	}
	found := false
	for _, id := range b.Robust.ModelOrder {
		if id == "gpt-6-astra" {
			found = true
		}
	}
	if !found {
		t.Fatal("bank missing gpt-6-astra")
	}
}

func TestParseNumbers(t *testing.T) {
	text := "1, 2, 3, abc, 400, 5"
	got := ParseNumbers(text)
	// "abc" splits runs; longest run wins. 400 is out of range.
	if len(got) == 0 {
		t.Fatal("expected numbers")
	}
	for _, n := range got {
		if n < 1 || n > 355 {
			t.Fatalf("out of range: %d", n)
		}
	}
}

func TestAttributeSmoke(t *testing.T) {
	b, err := LoadBank()
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	mkText := func() string {
		var sb strings.Builder
		for i := 0; i < 300; i++ {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(strings.TrimSpace(strings.Repeat("0", 0)))
			sb.WriteString(itoa(1 + rng.Intn(355)))
		}
		return sb.String()
	}
	texts := []string{mkText(), mkText(), mkText()}
	results, used, err := Attribute(texts, []int{300, 300, 300}, b)
	if err != nil {
		t.Fatal(err)
	}
	if used[0] != 3 {
		t.Fatalf("expected 3 used, got %v", used)
	}
	total := 0.0
	for _, r := range results {
		total += r.Probability
	}
	if total < 0.99 || total > 1.01 {
		t.Fatalf("probabilities sum to %f", total)
	}
	if len(results) != 16 {
		t.Fatalf("expected 16 results, got %d", len(results))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestGenerateChallenges(t *testing.T) {
	cs := GenerateChallenges(3)
	if len(cs) != 3 {
		t.Fatalf("expected 3 challenges, got %d", len(cs))
	}
	for _, c := range cs {
		if c.ExpectedCount < 292 || c.ExpectedCount > 332 {
			t.Fatalf("count out of range: %d", c.ExpectedCount)
		}
		if !strings.Contains(c.Prompt, "355") {
			t.Fatal("prompt missing range")
		}
	}
}

func TestJudgeMismatch(t *testing.T) {
	b, err := LoadBank()
	if err != nil {
		t.Fatal(err)
	}
	v := Judge([]string{"1, 2, 3"}, []int{300}, "gpt-6-astra", b)
	if v.Err == "" {
		t.Fatal("expected rejection of too-short output")
	}
}
