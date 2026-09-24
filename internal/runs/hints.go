package runs

import (
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"
)

// The wire library classifies a failure into a family plus a stable machine
// code and ships no operator prose; the wording below is trebi's. The texts
// are the ones trebi's own cli package shipped before the wire moved to
// agentwire, so an operator sees no change.
const (
	hintCredits  = "agent credits exhausted: check vendor billing"
	hintQuota    = "vendor quota reached: the run can resume with runtime: tui or after the window resets"
	hintPlan     = "vendor plan/entitlement refused programmatic use: check vendor status"
	hintAuth     = "re-login with `trebi harness auth login <harness>`"
	hintCapacity = "vendor capacity error: retry"
	hintTooOld   = "vendor CLI is too old for this wire: update the binary pin or relaunch with runtime: tui"
	hintDrift    = "vendor protocol drift: check the binary pin and doctor"
)

// hintByCode attaches the wording to the library's classification code.
var hintByCode = map[string]string{
	"credits_exhausted":    hintCredits,
	"rate_limited":         hintQuota,
	"plan_refused":         hintPlan,
	"invalid_credentials":  hintAuth,
	"token_revoked":        hintAuth,
	"not_logged_in":        hintAuth,
	"token_refresh_failed": hintAuth,
	"capacity":             hintCapacity,
	"cli_too_old":          hintTooOld,
	"wire_drift":           hintDrift,
	"http_401":             hintAuth,
	"http_403":             hintAuth,
	"http_402":             hintQuota,
	"http_429":             hintQuota,
	"http_502":             hintCapacity,
	"http_503":             hintCapacity,
	"http_504":             hintCapacity,
	"http_529":             hintCapacity,
}

// hintByClass is the fallback for a failure family whose code carries no rule
// of its own (an error result subtype, an unknown code).
var hintByClass = map[agentwire.FailureClass]string{
	agentwire.FailAuth:        hintAuth,
	agentwire.FailEntitlement: hintPlan,
	agentwire.FailLimit:       hintQuota,
	agentwire.FailOverloaded:  hintCapacity,
	agentwire.FailProtocol:    hintDrift,
}

// hintFor returns the operator-facing hint for a classification.
// The library reports the code; the wording is trebi's.
func hintFor(class agentwire.FailureClass, code string) string {
	if h, ok := hintByCode[code]; ok {
		return h
	}
	// A harness-qualified code ("claude.rate_limited") still maps to its rule.
	if _, bare, ok := strings.Cut(code, "."); ok {
		if h, ok := hintByCode[bare]; ok {
			return h
		}
	}
	return hintByClass[class]
}
