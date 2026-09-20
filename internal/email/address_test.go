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

func TestEncodeCrockford(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   []byte
		want string
	}{
		{[]byte{0, 0, 0, 0, 0}, "00000000"},
		{[]byte{0, 0, 0, 0, 1}, "00000001"},
		{[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, "ZZZZZZZZ"},
		{[]byte{1, 0, 0, 0, 0}, "04000000"}, // bit 32 → second group value 4
	}
	for _, c := range cases {
		if got := encodeCrockford(c.in); got != c.want {
			t.Errorf("encodeCrockford(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugify(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Ellis Labs", "ellis-labs"},
		{"", "ws"},
		{"---", "ws"},
		{"Acme!!!Corp", "acme-corp"},
		{"--Lead--Trim--", "lead-trim"},
		{"a  b   c", "a-b-c"},
		{strings.Repeat("x", 40), strings.Repeat("x", slugMax)},
		{"Über Café", "ber-caf"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if len(slugify(strings.Repeat("x", 40))) > slugMax {
		t.Error("slug must be capped")
	}
}

func TestLocalPartBudget(t *testing.T) {
	t.Parallel()
	// The normal path: max slug, token, and tag fit the RFC budget.
	lp, err := localPart(strings.Repeat("s", slugMax), "X7K2QM4P", strings.Repeat("a", agentMax))
	if err != nil {
		t.Fatalf("worst-case legal local part rejected: %v", err)
	}
	if len(lp) != slugMax+1+tokenLen+1+agentMax {
		t.Fatalf("local part %q: unexpected length %d", lp, len(lp))
	}
	if _, err := localPart("s", "X7K2QM4P", strings.Repeat("a", agentMax+1)); err == nil {
		t.Error("agent tag above the cap must fail")
	}
	longSlug := strings.Repeat("s", 40)
	if _, err := localPart(longSlug, "X7K2QM4P", strings.Repeat("a", agentMax)); err == nil {
		t.Error("local part above 64 octets must fail")
	}
}

func TestSplitAndMask(t *testing.T) {
	t.Parallel()
	slug, token, err := splitLocalPart("ellis-x7k2qm4p")
	if err != nil || slug != "ellis" || token != "x7k2qm4p" {
		t.Fatalf("splitLocalPart = %q,%q,%v", slug, token, err)
	}
	if _, _, err := splitLocalPart("notoken"); err == nil {
		t.Error("local part without a dash must fail")
	}
	cases := []struct{ in, want string }{
		{"ellis-x7k2qm4p", "ellis-********"},
		{"ellis-x7k2qm4p+research", "ellis-********+research"},
		{"ws-00000001", "ws-********"},
	}
	for _, c := range cases {
		if got := maskLocalPart(c.in); got != c.want {
			t.Errorf("maskLocalPart(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTagAddress(t *testing.T) {
	t.Parallel()
	a := Address{LocalPart: "ellis-x7k2qm4p"}
	full, err := tagAddress(a, "research")
	if err != nil || full != "ellis-x7k2qm4p+research" {
		t.Fatalf("tagAddress = %q,%v", full, err)
	}
	if _, err := tagAddress(a, strings.Repeat("a", agentMax+1)); err == nil {
		t.Error("agent name above the cap must fail")
	}
}
