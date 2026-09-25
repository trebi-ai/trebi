package agents

import _ "embed"

// AssistantTemplate is the default TREBI.md for a new workspace. It seeds
// the workspace assistant on create and on daemon start (backfill).
//
//go:embed assistant.md
var AssistantTemplate string
