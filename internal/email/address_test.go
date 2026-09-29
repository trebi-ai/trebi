package email

import (
	"strings"
	"testing"
)

func TestMintToken(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		tok, err := mintToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != tokenLen {
			t.Fatalf("token %q: want %d chars", tok, tokenLen)
		}
		for _, r := range tok {
			if !strings.ContainsRune(crockford, r) {
				t.Fatalf("token %q: %q outside the Crockford alphabet", tok, r)
			}
		}
		if seen[tok] {
			t.Fatalf("token %q minted twice", tok)
		}
		seen[tok] = true
	}
}

func TestLocalPartBudget(t *testing.T) {
	t.Parallel()
	// The normal path: max handle, token, and tag fit the RFC budget.
	lp, err := localPart(strings.Repeat("s", 20), "x7k2qm", strings.Repeat("a", agentMax))
	if err != nil {
		t.Fatalf("worst-case legal local part rejected: %v", err)
	}
	if len(lp) != 20+1+tokenLen+1+agentMax {
		t.Fatalf("local part %q: unexpected length %d", lp, len(lp))
	}
	if _, err := localPart("s", "x7k2qm", strings.Repeat("a", agentMax+1)); err == nil {
		t.Error("agent tag above the cap must fail")
	}
	longHandle := strings.Repeat("s", 40)
	if _, err := localPart(longHandle, "x7k2qm", strings.Repeat("a", agentMax)); err == nil {
		t.Error("local part above 64 octets must fail")
	}
}

func TestSplitAndMask(t *testing.T) {
	t.Parallel()
	handle, token, err := splitLocalPart("ellis-x7k2qm")
	if err != nil || handle != "ellis" || token != "x7k2qm" {
		t.Fatalf("splitLocalPart = %q,%q,%v", handle, token, err)
	}
	if _, _, err := splitLocalPart("notoken"); err == nil {
		t.Error("local part without a dash must fail")
	}
	cases := []struct{ in, want string }{
		{"ellis-x7k2qm", "ellis-******"},
		{"ellis-x7k2qm+research", "ellis-******+research"},
		{"ws-000001", "ws-******"},
	}
	for _, c := range cases {
		if got := maskLocalPart(c.in); got != c.want {
			t.Errorf("maskLocalPart(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTagAddress(t *testing.T) {
	t.Parallel()
	a := Address{LocalPart: "ellis-x7k2qm"}
	full, err := tagAddress(a, "research")
	if err != nil || full != "ellis-x7k2qm+research" {
		t.Fatalf("tagAddress = %q,%v", full, err)
	}
	if _, err := tagAddress(a, strings.Repeat("a", agentMax+1)); err == nil {
		t.Error("agent name above the cap must fail")
	}
}
