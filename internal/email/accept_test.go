package email

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

// TestAcceptRuleUnion: the union of a closed rule and an address rule
// accepts that address, and only one rule needs admins or the allowlist to
// keep both open.
func TestAcceptRuleUnion(t *testing.T) {
	t.Parallel()
	closed := AcceptRule{}
	named := NewAcceptRule([]job.AcceptEntry{{Address: "a@x.io"}})
	other := NewAcceptRule([]job.AcceptEntry{{Address: "b@x.io"}, {Domain: "y.io"}})

	r := closed.Union(named)
	if !r.Accepts("a@x.io", nil, nil) {
		t.Error("union must accept the address of the second rule")
	}
	if r.Accepts("b@x.io", nil, nil) {
		t.Error("union must not accept an address that neither rule accepts")
	}

	r = named.Union(other)
	for _, addr := range []string{"a@x.io", "b@x.io", "c@y.io"} {
		if !r.Accepts(addr, nil, nil) {
			t.Errorf("union must accept %s", addr)
		}
	}
	if r.admins || r.allowlist {
		t.Error("union of two address rules must not open admins or the allowlist")
	}

	// The union keeps admins and the allowlist when either rule sets them.
	r = AcceptRule{admins: true}.Union(AcceptRule{allowlist: true})
	if !r.admins || !r.UsesAllowlist() {
		t.Errorf("union = %+v, want both flags", r)
	}
	if NewAgentAcceptRule(nil).UsesAllowlist() {
		t.Error("an agent rule without the allowlist keyword must not use the allowlist")
	}
	// The source rules stay untouched.
	if named.admins || named.allowlist || len(named.addrs) != 1 {
		t.Errorf("union changed the receiver: %+v", named)
	}
}

// TestNewJobRecipientRule: a job always accepts admins and the workspace
// allowlist, and its email trigger senders add to them. An empty list opens
// nothing else, and the match ignores case.
func TestNewJobRecipientRule(t *testing.T) {
	t.Parallel()
	r := NewJobRecipientRule(nil)
	if !r.Accepts("ops@example.com", []string{"ops@example.com"}, nil) {
		t.Error("a job rule must accept an admin")
	}
	if !r.Accepts("friend@example.com", nil, []string{"friend@example.com"}) {
		t.Error("a job rule must accept a workspace allowlist address")
	}
	if r.Accepts("emma@example.com", []string{"ops@example.com"}, nil) {
		t.Error("an empty sender list must open no address")
	}

	r = NewJobRecipientRule([]string{" Emma@Example.com ", "", "emma@example.com"})
	if !r.Accepts("emma@example.com", nil, nil) {
		t.Error("job rule must accept a trigger sender")
	}
	if !r.Accepts("EMMA@example.com", nil, nil) {
		t.Error("address match must ignore case")
	}
	if got := len(r.addrs); got != 2 {
		t.Errorf("addrs = %v, want the entry kept as given", r.addrs)
	}
	if r.Accepts("other@example.com", nil, nil) {
		t.Error("job rule must refuse a stranger")
	}
}

// TestRecipientError: the refusal keeps the errors.Is contract of
// ErrRecipient and names the address. The API names the fix.
func TestRecipientError(t *testing.T) {
	t.Parallel()
	err := RecipientError{Addr: "emma@example.com"}
	if !errors.Is(err, ErrRecipient) {
		t.Error("errors.Is(RecipientError, ErrRecipient) = false")
	}
	if !strings.Contains(err.Error(), "emma@example.com") {
		t.Errorf("message = %q, want the address", err.Error())
	}
	wrapped := fmt.Errorf("send: %w", err)
	var refused RecipientError
	if !errors.As(wrapped, &refused) || refused.Addr != "emma@example.com" {
		t.Errorf("errors.As on a wrapped refusal = %+v", refused)
	}
}
