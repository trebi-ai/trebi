package cli

import (
	"fmt"
	"strings"
)

// FailureClass is the entitlement/quota classification family (I.7). The
// runs layer maps it to run end reasons; no pricing logic lives here.
type FailureClass string

const (
	FailNone        FailureClass = ""
	FailAuth        FailureClass = "auth"
	FailLimit       FailureClass = "limit"
	FailEntitlement FailureClass = "entitlement"
	FailOverloaded  FailureClass = "overloaded"
	FailProtocol    FailureClass = "protocol"
	FailUnknown     FailureClass = "unknown"
)

// Classification carries the family plus the operator-facing hint.
type Classification struct {
	Class   FailureClass
	Message string
	Hint    string
}

// resultErrorText picks the most informative text from a result frame, the
// way the SDK's _error_result_text does: errors[], then result, then subtype,
// then api_error_status.
func resultErrorText(errors []string, result, subtype string, apiStatus int) string {
	if len(errors) > 0 {
		return strings.Join(errors, "; ")
	}
	if strings.TrimSpace(result) != "" {
		return strings.TrimSpace(result)
	}
	if subtype != "" && subtype != "success" {
		return subtype
	}
	if apiStatus > 0 {
		return fmt.Sprintf("API error (HTTP %d)", apiStatus)
	}
	return "unknown error"
}

// classPattern is one ordered classifier rule.
type classPattern struct {
	class FailureClass
	words []string
	hint  string
}

// classification rules, most specific first. Vendors word these differently
// per harness; fixtures pin each shape.
var classPatterns = []classPattern{
	{FailLimit, []string{
		"out of credits", "credit balance", "credit exhausted", "no credits",
		"insufficient credits", "agent credit",
	}, "agent credits exhausted: check vendor billing"},
	{FailLimit, []string{
		"rate limit", "rate_limit", "429", "too many requests",
		"usage limit", "session limit", "weekly limit", "5-hour limit",
		"quota exceeded", "quota reached", "limit reached", "limit exceeded",
		"over usage", "usage cap",
	}, "vendor quota reached: the run can resume with runtime: tui or after the window resets"},
	{FailEntitlement, []string{
		"entitlement", "not entitled", "upgrade required", "subscription required",
		"no active subscription", "plan does not include", "access denied for your plan",
	}, "vendor plan/entitlement refused programmatic use: check vendor status"},
	{FailAuth, []string{
		"invalid api key", "invalid_api_key", "authentication_error", "authentication failed",
		"unauthorized", "401", "token has been revoked", "token revoked",
		"not logged in", "please run /login", "please login", "login required",
		"oauth token", "oauth refresh failed", "invalid_grant", "credentials",
	}, "re-login with `trebi harness auth login <harness>`"},
	{FailOverloaded, []string{"overloaded", "529", "503", "server error", "capacity"},
		"vendor capacity error: retry"},
	{FailProtocol, []string{
		"unrecognized subcommand", "unknown subcommand", "invalid subcommand",
	}, "vendor CLI is too old for this wire: update the binary pin or relaunch with runtime: tui"},
	{FailProtocol, []string{
		"protocol", "json-rpc", "parse error", "unexpected message", "invalid frame",
	}, "vendor protocol drift: check the binary pin and doctor"},
}

// ClassifyError inspects an error/limit string and returns the family. status
// is an optional HTTP status from the wire (0 = unknown).
func ClassifyError(text string, status int) Classification {
	low := strings.ToLower(text)
	if status > 0 {
		low += " " + fmt.Sprint(status)
	}
	for _, p := range classPatterns {
		for _, w := range p.words {
			if strings.Contains(low, w) {
				return Classification{Class: p.class, Message: strings.TrimSpace(text), Hint: p.hint}
			}
		}
	}
	return Classification{Class: FailUnknown, Message: strings.TrimSpace(text)}
}

// ClassifyResult classifies an error result frame.
func ClassifyResult(r *Result) Classification {
	if r == nil {
		return Classification{Class: FailUnknown}
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		text = r.Subtype
	}
	c := ClassifyError(text, 0)
	if c.Class == FailUnknown && r.Subtype != "" && r.Subtype != "success" {
		// error_max_turns / error_during_execution are harness failures, not
		// entitlement classes.
		c = Classification{Class: FailNone, Message: text}
	}
	return c
}

// EndReason maps a class to the runs end reason vocabulary
// (auth|limit|failed|launch).
func (c Classification) EndReason() string {
	switch c.Class {
	case FailAuth, FailEntitlement:
		return "auth"
	case FailLimit:
		return "limit"
	case FailProtocol:
		return "launch"
	}
	return "failed"
}
