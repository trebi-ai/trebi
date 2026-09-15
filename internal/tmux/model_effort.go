package tmux

import (
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
)

// ModelEffortFlags returns the session-scoped argv flags for a harness model
// and reasoning-effort override. Empty values add nothing, so an unset pin
// falls through to the vendor's own default.
//
// Trebi never writes a vendor config file for these: in shared auth mode the
// interactive CLI owns ~/.claude, ~/.codex, and friends, and a trebi default
// must not leak into a user's own session.
func ModelEffortFlags(tool, model, effort string) []string {
	if tool == Fake {
		return nil
	}
	var flags []string
	if model != "" && config.HarnessSupportsModel(tool) {
		flags = append(flags, "--model", model)
	}
	if effort == "" {
		return flags
	}
	switch tool {
	case Claude, Grok, Copilot, Pi:
		flags = append(flags, "--effort", effort)
	case Codex:
		flags = append(flags, "-c", "model_reasoning_effort="+effort)
	case OpenCode:
		flags = append(flags, "--variant", effort)
	}
	return flags
}

// ApplyModelEffort appends the model/effort flags to a launch command. A
// command that already carries its own --model or effort knob wins: the
// caller typed it, so trebi does not add a second value.
func ApplyModelEffort(command, tool, model, effort string) string {
	if model != "" && hasToken(command, "--model") {
		model = ""
	}
	if effort != "" && hasToken(command, effortToken(tool)) {
		effort = ""
	}
	return AppendFlags(command, ModelEffortFlags(tool, model, effort))
}

// effortToken is the substring that marks a caller-supplied effort choice.
func effortToken(tool string) string {
	switch tool {
	case Claude, Grok, Copilot, Pi:
		return "--effort"
	case Codex:
		return "model_reasoning_effort"
	case OpenCode:
		return "--variant"
	}
	return ""
}

// hasToken reports whether one argv field equals token or starts with token=.
func hasToken(command, token string) bool {
	if token == "" {
		return false
	}
	for _, f := range strings.Fields(command) {
		if f == token || strings.HasPrefix(f, token+"=") {
			return true
		}
	}
	return false
}
