package tmux

import "slices"

// Lists checked on 2026-10-01: codex from its models cache (codex-cli
// 0.159.3), copilot from its --model choices (1.0.27).
//
//nolint:gochecknoglobals // static list, read through staticModels only
var staticModelLists = map[string][]ModelInfo{
	Claude: {
		{ID: "sonnet", Label: "Sonnet (latest)"},
		{ID: "opus", Label: "Opus (latest)"},
		{ID: "haiku", Label: "Haiku (latest)"},
	},
	Codex: {
		{ID: "gpt-5.6-sol"},
		{ID: "gpt-5.6-terra"},
		{ID: "gpt-5.6-luna"},
		{ID: "gpt-5.5"},
		{ID: "gpt-5.4"},
		{ID: "gpt-5.4-mini"},
	},
	Copilot: {
		{ID: "claude-sonnet-4.6"},
		{ID: "claude-sonnet-4.5"},
		{ID: "claude-haiku-4.5"},
		{ID: "claude-opus-4.6"},
		{ID: "claude-opus-4.6-fast"},
		{ID: "claude-opus-4.6-1m"},
		{ID: "claude-opus-4.5"},
		{ID: "claude-sonnet-4"},
		{ID: "gpt-5.4"},
		{ID: "gpt-5.4-mini"},
		{ID: "gpt-5.3-codex"},
		{ID: "gpt-5.2-codex"},
		{ID: "gpt-5.2"},
		{ID: "gpt-5.1"},
		{ID: "gpt-5-mini"},
		{ID: "gpt-4.1"},
	},
}

// staticModels is the list trebi ships for a harness whose CLI has no list
// command. Aliases first: they follow the vendor's newest model. The result
// is a copy; nil means no list.
func staticModels(name string) []ModelInfo {
	return slices.Clone(staticModelLists[name])
}

// withDefault puts a configured default that the static list does not hold
// at the top, so the picker shows the model a run uses.
func withDefault(models []ModelInfo, def string) []ModelInfo {
	if def == "" || slices.ContainsFunc(models, func(m ModelInfo) bool { return m.ID == def }) {
		return models
	}
	return append([]ModelInfo{{ID: def}}, models...)
}
