package provider

import "testing"

func TestDiffHashEmptyInputStaysEmpty(t *testing.T) {
	if got := diffHash(""); got != "" {
		t.Fatalf("diffHash(\"\") = %q, want empty string (not the hash of an empty string)", got)
	}
}

func TestDiffHashIsDeterministic(t *testing.T) {
	diff := `+/- Job: "prometheus"
+/- Tags: "traefik...prometheus.sandford.hous"`

	a := diffHash(diff)
	b := diffHash(diff)
	if a != b {
		t.Fatalf("diffHash is not deterministic: %q != %q", a, b)
	}
	if a == "" {
		t.Fatal("expected a non-empty hash for non-empty input")
	}
}

func TestDiffHashDiffersForDifferentInput(t *testing.T) {
	a := diffHash(`+/- Job: "prometheus" ... .hous`)
	b := diffHash(`+/- Job: "prometheus" ... .house`)
	if a == b {
		t.Fatalf("expected different diffs to hash differently, both got %q", a)
	}
}
