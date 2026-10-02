// Package request holds the pure CLI/MCP → API request catalog.
// Every Build* function returns an HTTPRequest describing one /v1 call,
// testable without a daemon. No I/O, no trebi imports.
package request

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// HTTPRequest is a pure description of a CLI→API call (testable without a daemon).
type HTTPRequest struct {
	Method      string
	Path        string
	Body        any    // JSON body; nil when no body
	Raw         []byte // raw body; mutually exclusive with Body
	ContentType string // for Raw; default application/octet-stream
	IfMatch     string // optional If-Match header
}

// RunSpec is the launch body of POST /v1/jobs/{name}/run. It is the one
// launch shape: the CLI, the UI, and MCP all fill it.
type RunSpec struct {
	Job            string
	Workspace      string
	Prompt         string // full prompt override
	ExtraPrompt    string // appended to the job body server-side
	Inputs         map[string]string
	Harness        string
	Kind           string
	Reason         string // defaults to manual
	ConversationID string
	IdempotencyKey string
	DryRun         bool
	WaitMS         int // long-poll: return when the run ends or the time elapses
}

// Build renders the spec as POST /v1/jobs/{name}/run.
func (r RunSpec) Build() HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(r.Job)+"/run", mergeQ(workspaceQ(r.Workspace), waitQ(r.WaitMS))),
		Body:   r.body(),
	}
}

// BuildRunsCreate renders the spec as POST /v1/runs, the canonical launch
// (B9). Job and workspace ride the body; the sugar routes carry them in the path.
func (r RunSpec) BuildRunsCreate() HTTPRequest {
	body := r.body()
	body["job"] = r.Job
	if r.Workspace != "" {
		body["workspace"] = r.Workspace
	}
	return HTTPRequest{Method: "POST", Path: withQuery("/v1/runs", waitQ(r.WaitMS)), Body: body}
}

// BuildAgentWake renders the spec as POST /v1/workspaces/{id}/agents/{name}/wake.
// Job carries the agent short name; the daemon pins job, kind, and workspace.
func (r RunSpec) BuildAgentWake() HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path: withQuery("/v1/workspaces/"+url.PathEscape(r.Workspace)+
			"/agents/"+url.PathEscape(r.Job)+"/wake", waitQ(r.WaitMS)),
		Body: r.body(),
	}
}

func (r RunSpec) body() map[string]any {
	body := map[string]any{"reason": r.Reason}
	if r.Reason == "" {
		body["reason"] = "manual"
	}
	for k, v := range map[string]string{
		"prompt_override": r.Prompt,
		"extra_prompt":    r.ExtraPrompt,
		"harness":         r.Harness,
		"kind":            r.Kind,
		"conversation_id": r.ConversationID,
		"idempotency_key": r.IdempotencyKey,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if len(r.Inputs) > 0 {
		body["inputs"] = r.Inputs
	}
	if r.DryRun {
		body["dry_run"] = true
	}
	return body
}

// BuildRunsKill builds POST /v1/runs/{id}/kill.
func BuildRunsKill(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/runs/" + url.PathEscape(id) + "/kill",
	}
}

// BuildRunsKillBulk builds POST /v1/runs/kill with an id list.
func BuildRunsKillBulk(ids []string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/runs/kill", Body: map[string]any{"ids": ids}}
}

// BuildRunsRetry builds POST /v1/runs/{id}/retry.
func BuildRunsRetry(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/runs/" + url.PathEscape(id) + "/retry",
	}
}

// BuildRunMark builds POST /v1/runs/{id}/mark. clear drops the outcome;
// otherwise status is success|complete|failed|skipped.
func BuildRunMark(id, status, note string, clear bool) HTTPRequest {
	body := map[string]any{}
	if clear {
		body["clear"] = true
	} else {
		body["status"] = status
	}
	if note != "" {
		body["note"] = note
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/runs/" + url.PathEscape(id) + "/mark",
		Body:   body,
	}
}

// BuildRunMessage builds POST /v1/runs/{id}/message.
func BuildRunMessage(id, text string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/runs/" + url.PathEscape(id) + "/message",
		Body:   map[string]any{"text": text},
	}
}

// BuildRunTranscript builds GET /v1/runs/{id}/transcript.
func BuildRunTranscript(id string, raw bool) HTTPRequest {
	path := "/v1/runs/" + url.PathEscape(id) + "/transcript"
	if raw {
		path += "?raw=1"
	}
	return HTTPRequest{Method: "GET", Path: path}
}

// BuildRunGetPath builds GET /v1/runs/{id}. A positive waitMS long-polls until
// the run ends.
func BuildRunGetPath(id string, waitMS int) string {
	return withQuery("/v1/runs/"+url.PathEscape(id), waitQ(waitMS))
}

// BuildRunTimelinePath builds GET /v1/runs/{id}/timeline.
func BuildRunTimelinePath(id string) string {
	return "/v1/runs/" + url.PathEscape(id) + "/timeline"
}

// BuildRunTurnsPath builds GET /v1/runs/{id}/turns.
func BuildRunTurnsPath(id string) string {
	return "/v1/runs/" + url.PathEscape(id) + "/turns"
}

// BuildRunLogPath builds GET /v1/runs/{id}/log. follow adds follow=1 (SSE).
func BuildRunLogPath(id string, follow bool) string {
	path := "/v1/runs/" + url.PathEscape(id) + "/log"
	if follow {
		path += "?follow=1"
	}
	return path
}

// BuildRunResume builds POST /v1/runs/{id}/resume.
func BuildRunResume(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/runs/" + url.PathEscape(id) + "/resume"}
}

// BuildRunReplay builds POST /v1/runs/{id}/replay.
func BuildRunReplay(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/runs/" + url.PathEscape(id) + "/replay"}
}

// BuildRunApprove builds POST /v1/runs/{id}/approve. decision is allow|deny.
// An empty approvalID answers the run's pending row.
func BuildRunApprove(id, decision, approvalID string) HTTPRequest {
	body := map[string]string{"decision": decision}
	if approvalID != "" {
		body["approval_id"] = approvalID
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/runs/" + url.PathEscape(id) + "/approve",
		Body:   body,
	}
}

// BuildRunsStats builds GET /v1/stats/runs. An empty range means 30d.
func BuildRunsStats(workspace, job, rng string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: withQuery("/v1/stats/runs", map[string]string{
		"workspace": workspace, "job": job, "range": rng,
	})}
}

// BuildSessionState builds GET /v1/runs/session-state.
func BuildSessionState() HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/runs/session-state"}
}

// BuildStreamPath builds GET /v1/stream?topics=&cursor=.
// cursor is the bus cursor (event_log.seq); empty omits the param.
func BuildStreamPath(topics, cursor string) string {
	return withQuery("/v1/stream", map[string]string{"topics": topics, "cursor": cursor})
}

// BuildStreamDumpPath builds GET /v1/stream in dump mode (follow=0):
// retained history only, then close.
func BuildStreamDumpPath(topics, cursor string) string {
	return withQuery(BuildStreamPath(topics, cursor), map[string]string{"follow": "0"})
}

// BuildDoctorFix builds POST /v1/system/doctor/{id}/fix.
func BuildDoctorFix(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/system/doctor/" + url.PathEscape(id) + "/fix",
	}
}

// BuildSkillGetPath builds GET /v1/skills/{name}.
func BuildSkillGetPath(name string) string {
	return "/v1/skills/" + url.PathEscape(name)
}

// SkillSpec is the POST /v1/skills body. Give SkillMD for a local skill, or
// SourceGit or SourceFolder to pull the bundle from a source.
type SkillSpec struct {
	Name         string
	SkillMD      string
	Provenance   string
	SourceGit    string
	SourceFolder string
}

// Build builds POST /v1/skills.
func (sp SkillSpec) Build() HTTPRequest {
	body := map[string]any{"name": sp.Name}
	switch {
	case sp.SourceGit != "":
		body["source"] = map[string]any{"kind": "git", "url": sp.SourceGit}
	case sp.SourceFolder != "":
		body["source"] = map[string]any{"kind": "folder", "path": sp.SourceFolder}
	default:
		body["skill_md"] = sp.SkillMD
		body["provenance"] = sp.Provenance
	}
	return HTTPRequest{Method: "POST", Path: "/v1/skills", Body: body}
}

// BuildSkillRefresh builds POST /v1/skills/{name}/refresh, which pulls the
// skill source again.
func BuildSkillRefresh(name string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/skills/" + url.PathEscape(name) + "/refresh"}
}

// BuildSkillDelete builds DELETE /v1/skills/{name}.
func BuildSkillDelete(name string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/skills/"+url.PathEscape(name), confirmQ(confirm))}
}

// BuildSecretsListPath builds GET /v1/secrets.
func BuildSecretsListPath(limit int) string {
	return withQuery("/v1/secrets", limitQ(limit))
}

// BuildSecretPut builds PUT /v1/secrets/{name}.
func BuildSecretPut(name, value string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/secrets/" + url.PathEscape(name),
		Body:   map[string]string{"value": value},
	}
}

// BuildSecretDelete builds DELETE /v1/secrets/{name}.
func BuildSecretDelete(name string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/secrets/"+url.PathEscape(name), confirmQ(confirm))}
}

// BuildSkillPut builds PUT /v1/skills/{name}.
func BuildSkillPut(name, skillMD string, harnesses map[string]string, ifMatch string) HTTPRequest {
	body := map[string]any{"skill_md": skillMD}
	if len(harnesses) > 0 {
		body["harnesses"] = harnesses
	}
	return HTTPRequest{
		Method:  "PUT",
		Path:    "/v1/skills/" + url.PathEscape(name),
		Body:    body,
		IfMatch: ifMatch,
	}
}

// ConnectorRef names the connection a route acts on: its slug or its id.
type ConnectorRef struct {
	Ref string
}

// path builds /v1/connectors/{ref}{tail} with the query kv.
func (c ConnectorRef) path(tail string, kv map[string]string) string {
	return withQuery("/v1/connectors/"+url.PathEscape(c.Ref)+tail, kv)
}

// BuildConnectorConnect builds POST /v1/connectors/{ref}/connect.
func BuildConnectorConnect(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: c.path("/connect", nil)}
}

// BuildConnectorDisconnect builds POST /v1/connectors/{ref}/disconnect.
func BuildConnectorDisconnect(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: c.path("/disconnect", nil)}
}

// BuildConnectorCursorGet builds GET /v1/connectors/{ref}/cursor.
func BuildConnectorCursorGet(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: c.path("/cursor", nil)}
}

// BuildConnectorCursorSet builds PUT /v1/connectors/{ref}/cursor with last_event_id.
func BuildConnectorCursorSet(c ConnectorRef, lastEventID string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   c.path("/cursor", nil),
		Body:   map[string]any{"last_event_id": lastEventID},
	}
}

// BuildConnectorCursorReset builds PUT /v1/connectors/{ref}/cursor with an empty body.
func BuildConnectorCursorReset(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "PUT", Path: c.path("/cursor", nil)}
}

// --- Memory (B22 §3.3 rev 2) ---
// Three owner roots share one route shape: the job root (?workspace=), the
// folder agent root, and the assistant root. Shared constructors take a base
// path plus query; per-root Build* bind them.

// BuildJobMemoryGetPath builds GET /v1/jobs/{name}/memory?workspace=.
func BuildJobMemoryGetPath(name, workspace string) string {
	return withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace))
}

// BuildAssistantMemoryGetPath builds GET /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryGetPath(workspaceID string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory"
}

// BuildAgentMemoryGetPath builds GET /v1/workspaces/{id}/agents/{name}/memory.
func BuildAgentMemoryGetPath(workspaceID, name string) string {
	return BuildAgentGetPath(workspaceID, name) + "/memory"
}

// BuildJobMemoryPut builds PUT /v1/jobs/{name}/memory.
func BuildJobMemoryPut(name, workspace, content, ifMatch string, force bool) HTTPRequest {
	return memoryPut("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace), content, ifMatch, force)
}

// BuildJobMemoryClear builds DELETE /v1/jobs/{name}/memory.
func BuildJobMemoryClear(name, workspace, ifMatch string, confirm bool) HTTPRequest {
	return memoryClear("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace), ifMatch, confirm)
}

// BuildJobMemoryValidate builds POST /v1/jobs/{name}/memory/validate.
func BuildJobMemoryValidate(name, workspace, content string) HTTPRequest {
	return memoryValidate("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace), content)
}

// BuildJobMemoryMigrate builds POST /v1/jobs/{name}/memory/migrate.
func BuildJobMemoryMigrate(name, workspace string) HTTPRequest {
	return memoryMigrate("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace))
}

// BuildJobMemorySectionPut builds PUT /v1/jobs/{name}/memory/{section}.
func BuildJobMemorySectionPut(name, workspace, section, body string) HTTPRequest {
	return memorySectionPut("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace), section, body)
}

// BuildJobMemoryEntryAdd builds POST /v1/jobs/{name}/memory/{section}.
func BuildJobMemoryEntryAdd(name, workspace, section, at, text string) HTTPRequest {
	return memoryEntryAdd("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace), section, at, text)
}

// BuildJobMemoryEntryDelete builds DELETE /v1/jobs/{name}/memory/{section}/{id}.
func BuildJobMemoryEntryDelete(name, workspace, section, entryID string) HTTPRequest {
	return memoryEntryDelete("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace), section, entryID)
}

// BuildAssistantMemoryPut builds PUT /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryPut(workspaceID, content, ifMatch string, force bool) HTTPRequest {
	return memoryPut(BuildAssistantMemoryGetPath(workspaceID), nil, content, ifMatch, force)
}

// BuildAssistantMemoryClear builds DELETE /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryClear(workspaceID, ifMatch string, confirm bool) HTTPRequest {
	return memoryClear(BuildAssistantMemoryGetPath(workspaceID), nil, ifMatch, confirm)
}

// BuildAssistantMemoryValidate builds POST /v1/workspaces/{id}/assistant/memory/validate.
func BuildAssistantMemoryValidate(workspaceID, content string) HTTPRequest {
	return memoryValidate(BuildAssistantMemoryGetPath(workspaceID), nil, content)
}

// BuildAssistantMemoryMigrate builds POST /v1/workspaces/{id}/assistant/memory/migrate.
func BuildAssistantMemoryMigrate(workspaceID string) HTTPRequest {
	return memoryMigrate(BuildAssistantMemoryGetPath(workspaceID), nil)
}

// BuildAssistantMemorySectionPut builds PUT .../assistant/memory/{section}.
func BuildAssistantMemorySectionPut(workspaceID, section, body string) HTTPRequest {
	return memorySectionPut(BuildAssistantMemoryGetPath(workspaceID), nil, section, body)
}

// BuildAssistantMemoryEntryAdd builds POST .../assistant/memory/{section}.
func BuildAssistantMemoryEntryAdd(workspaceID, section, at, text string) HTTPRequest {
	return memoryEntryAdd(BuildAssistantMemoryGetPath(workspaceID), nil, section, at, text)
}

// BuildAssistantMemoryEntryDelete builds DELETE .../assistant/memory/{section}/{id}.
func BuildAssistantMemoryEntryDelete(workspaceID, section, entryID string) HTTPRequest {
	return memoryEntryDelete(BuildAssistantMemoryGetPath(workspaceID), nil, section, entryID)
}

// BuildAgentMemoryPut builds PUT /v1/workspaces/{id}/agents/{name}/memory.
func BuildAgentMemoryPut(workspaceID, name, content, ifMatch string, force bool) HTTPRequest {
	return memoryPut(BuildAgentMemoryGetPath(workspaceID, name), nil, content, ifMatch, force)
}

// BuildAgentMemoryClear builds DELETE /v1/workspaces/{id}/agents/{name}/memory.
func BuildAgentMemoryClear(workspaceID, name, ifMatch string, confirm bool) HTTPRequest {
	return memoryClear(BuildAgentMemoryGetPath(workspaceID, name), nil, ifMatch, confirm)
}

// BuildAgentMemoryValidate builds POST /v1/workspaces/{id}/agents/{name}/memory/validate.
func BuildAgentMemoryValidate(workspaceID, name, content string) HTTPRequest {
	return memoryValidate(BuildAgentMemoryGetPath(workspaceID, name), nil, content)
}

// BuildAgentMemoryMigrate builds POST /v1/workspaces/{id}/agents/{name}/memory/migrate.
func BuildAgentMemoryMigrate(workspaceID, name string) HTTPRequest {
	return memoryMigrate(BuildAgentMemoryGetPath(workspaceID, name), nil)
}

// BuildAgentMemorySectionPut builds PUT .../agents/{name}/memory/{section}.
func BuildAgentMemorySectionPut(workspaceID, name, section, body string) HTTPRequest {
	return memorySectionPut(BuildAgentMemoryGetPath(workspaceID, name), nil, section, body)
}

// BuildAgentMemoryEntryAdd builds POST .../agents/{name}/memory/{section}.
func BuildAgentMemoryEntryAdd(workspaceID, name, section, at, text string) HTTPRequest {
	return memoryEntryAdd(BuildAgentMemoryGetPath(workspaceID, name), nil, section, at, text)
}

// BuildAgentMemoryEntryDelete builds DELETE .../agents/{name}/memory/{section}/{id}.
func BuildAgentMemoryEntryDelete(workspaceID, name, section, entryID string) HTTPRequest {
	return memoryEntryDelete(BuildAgentMemoryGetPath(workspaceID, name), nil, section, entryID)
}

// MemoryRequests binds the memory request builders to one owner root, so the
// CLI wires a whole owner with one value.
type MemoryRequests struct {
	GetPath  func(name, ws string) string
	Put      func(name, ws, content, ifMatch string, force bool) HTTPRequest
	Clear    func(name, ws, ifMatch string, confirm bool) HTTPRequest
	Validate func(name, ws, content string) HTTPRequest
	Migrate  func(name, ws string) HTTPRequest
	Section  func(name, ws, section, body string) HTTPRequest
	EntryAdd func(name, ws, section, at, text string) HTTPRequest
	EntryDel func(name, ws, section, entryID string) HTTPRequest
}

// JobMemoryRequests binds the memory builders to the job root.
func JobMemoryRequests() MemoryRequests {
	return MemoryRequests{
		GetPath:  BuildJobMemoryGetPath,
		Put:      BuildJobMemoryPut,
		Clear:    BuildJobMemoryClear,
		Validate: BuildJobMemoryValidate,
		Migrate:  BuildJobMemoryMigrate,
		Section:  BuildJobMemorySectionPut,
		EntryAdd: BuildJobMemoryEntryAdd,
		EntryDel: BuildJobMemoryEntryDelete,
	}
}

// AgentMemoryRequests binds the memory builders to the agent root.
func AgentMemoryRequests() MemoryRequests {
	return MemoryRequests{
		GetPath: func(name, ws string) string { return BuildAgentMemoryGetPath(ws, name) },
		Put: func(name, ws, content, ifMatch string, force bool) HTTPRequest {
			return BuildAgentMemoryPut(ws, name, content, ifMatch, force)
		},
		Clear: func(name, ws, ifMatch string, confirm bool) HTTPRequest {
			return BuildAgentMemoryClear(ws, name, ifMatch, confirm)
		},
		Validate: func(name, ws, content string) HTTPRequest {
			return BuildAgentMemoryValidate(ws, name, content)
		},
		Migrate: func(name, ws string) HTTPRequest { return BuildAgentMemoryMigrate(ws, name) },
		Section: func(name, ws, section, body string) HTTPRequest {
			return BuildAgentMemorySectionPut(ws, name, section, body)
		},
		EntryAdd: func(name, ws, section, at, text string) HTTPRequest {
			return BuildAgentMemoryEntryAdd(ws, name, section, at, text)
		},
		EntryDel: func(name, ws, section, entryID string) HTTPRequest {
			return BuildAgentMemoryEntryDelete(ws, name, section, entryID)
		},
	}
}

// AssistantMemoryRequests binds the memory builders to the assistant root.
func AssistantMemoryRequests() MemoryRequests {
	return MemoryRequests{
		GetPath: func(_, ws string) string { return BuildAssistantMemoryGetPath(ws) },
		Put: func(_, ws, content, ifMatch string, force bool) HTTPRequest {
			return BuildAssistantMemoryPut(ws, content, ifMatch, force)
		},
		Clear: func(_, ws, ifMatch string, confirm bool) HTTPRequest {
			return BuildAssistantMemoryClear(ws, ifMatch, confirm)
		},
		Validate: func(_, ws, content string) HTTPRequest {
			return BuildAssistantMemoryValidate(ws, content)
		},
		Migrate: func(_, ws string) HTTPRequest { return BuildAssistantMemoryMigrate(ws) },
		Section: func(_, ws, section, body string) HTTPRequest {
			return BuildAssistantMemorySectionPut(ws, section, body)
		},
		EntryAdd: func(_, ws, section, at, text string) HTTPRequest {
			return BuildAssistantMemoryEntryAdd(ws, section, at, text)
		},
		EntryDel: func(_, ws, section, entryID string) HTTPRequest {
			return BuildAssistantMemoryEntryDelete(ws, section, entryID)
		},
	}
}

// BuildWorkspacesListPath builds GET /v1/workspaces.
func BuildWorkspacesListPath(limit int) string {
	return withQuery("/v1/workspaces", limitQ(limit))
}

// BuildWorkspaceGetPath builds GET /v1/workspaces/{id}.
func BuildWorkspaceGetPath(id string) string {
	return "/v1/workspaces/" + url.PathEscape(id)
}

// BuildAssistantGetPath builds GET /v1/workspaces/{id}/assistant.
func BuildAssistantGetPath(workspaceID string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant"
}

// BuildAssistantDefaultsGet builds GET /v1/assistant: the global settings
// plus the harness option list.
func BuildAssistantDefaultsGet() HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/assistant"}
}

// BuildAssistantDefaultsPut builds PUT /v1/assistant with a settings object.
// Only the fields the caller changed need to be present.
func BuildAssistantDefaultsPut(settings map[string]any) HTTPRequest {
	return HTTPRequest{Method: "PUT", Path: "/v1/assistant", Body: settings}
}

// BuildAssistantSettingsPut builds PUT /v1/workspaces/{id}/assistant with a
// workspace override object. A stale ifMatch is 412 precondition_failed.
func BuildAssistantSettingsPut(workspaceID string, override map[string]any, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PUT",
		Path:    BuildAssistantGetPath(workspaceID),
		Body:    map[string]any{"override": override},
		IfMatch: ifMatch,
	}
}

// BuildAssistantPreviewPath builds GET /v1/workspaces/{id}/assistant/preview:
// the composed instructions of a new session.
func BuildAssistantPreviewPath(workspaceID string) string {
	return BuildAssistantGetPath(workspaceID) + "/preview"
}

// AgentIndexName maps an agent short name to its index name: cmo → agents/cmo.
// A name that already carries the prefix passes through unchanged.
func AgentIndexName(short string) string {
	if short == "" || strings.HasPrefix(short, "agents/") {
		return short
	}
	return "agents/" + short
}

// BuildProposalGet builds GET /v1/proposals/{id}.
func BuildProposalGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/proposals/" + url.PathEscape(id)}
}

// RequestCreateBody is the body of POST /v1/requests. Payload fields are
// flat; ExpiresIn is a duration string such as 24h.
type RequestCreateBody struct {
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Summary   string          `json:"summary,omitempty"`
	Target    string          `json:"target,omitempty"`
	Draft     any             `json:"draft,omitempty"`   // *RequestDraft or raw JSON
	Options   any             `json:"options,omitempty"` // []RequestOption or raw JSON
	Multi     bool            `json:"multi,omitempty"`
	Schema    json.RawMessage `json:"schema,omitempty"`
	Allow     []string        `json:"allow,omitempty"`
	Report    string          `json:"report,omitempty"`
	Delivery  string          `json:"delivery,omitempty"`
	ExpiresIn string          `json:"expires_in,omitempty"`
}

type RequestDraft struct {
	Format string `json:"format,omitempty"`
	Text   string `json:"text"`
}

type RequestOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// RequestAnswerBody is the body of POST /v1/requests/{id}/answer.
type RequestAnswerBody struct {
	Decision string          `json:"decision"`
	Text     string          `json:"text,omitempty"`
	Message  string          `json:"message,omitempty"`
	Choices  []string        `json:"choices,omitempty"`
	Values   json.RawMessage `json:"values,omitempty"`
	Note     string          `json:"note,omitempty"`
	Via      string          `json:"via,omitempty"`
}

// BuildRequestCreate builds POST /v1/requests. It needs a run token.
func BuildRequestCreate(b RequestCreateBody) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/requests", Body: b}
}

// BuildRequestGet builds GET /v1/requests/{id}; wait long-polls (e.g. 30s).
func BuildRequestGet(id, wait string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: withQuery("/v1/requests/"+url.PathEscape(id), map[string]string{"wait": wait})}
}

// BuildRequestListPath builds GET /v1/requests with its filters.
func BuildRequestListPath(status, run, job, workspace, cursor string, limit int) string {
	q := map[string]string{"status": status, "run": run, "job": job, "workspace": workspace, "cursor": cursor}
	if limit > 0 {
		q["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/requests", q)
}

// BuildRequestReport builds GET /v1/requests/{id}/report.
func BuildRequestReport(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/requests/" + url.PathEscape(id) + "/report"}
}

// BuildRequestAnswer builds POST /v1/requests/{id}/answer. ifMatch is the
// request revision; empty skips the check.
func BuildRequestAnswer(id string, b RequestAnswerBody, ifMatch string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/requests/" + url.PathEscape(id) + "/answer", Body: b, IfMatch: ifMatch}
}

// BuildRequestDismiss builds POST /v1/requests/{id}/dismiss.
func BuildRequestDismiss(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/requests/" + url.PathEscape(id) + "/dismiss", Body: map[string]any{}}
}

// BuildAgentListPath builds GET /v1/workspaces/{id}/agents.
func BuildAgentListPath(workspaceID string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/agents"
}

// BuildAgentListAllPath builds GET /v1/agents across workspaces.
func BuildAgentListAllPath(workspaceFilter string) string {
	return withQuery("/v1/agents", map[string]string{"workspace": workspaceFilter})
}

// BuildAgentGetPath builds GET /v1/workspaces/{id}/agents/{name}. name is the
// short name.
func BuildAgentGetPath(workspaceID, name string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/agents/" + url.PathEscape(name)
}

// BuildAgentRename builds POST /v1/workspaces/{id}/agents/{name}/rename.
func BuildAgentRename(workspaceID, from, to string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   BuildAgentGetPath(workspaceID, from) + "/rename",
		Body:   map[string]string{"to": to},
	}
}

// BuildAgentAsk builds POST /v1/workspaces/{id}/agents/{name}/ask.
func BuildAgentAsk(workspaceID, name, text string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   BuildAgentGetPath(workspaceID, name) + "/ask",
		Body:   map[string]string{"text": text},
	}
}

// BuildAgentEscalate builds POST /v1/workspaces/{id}/agents/{name}/escalate.
func BuildAgentEscalate(workspaceID, name, to, reason, runID string) HTTPRequest {
	body := map[string]string{"to": to, "reason": reason}
	if runID != "" {
		body["run_id"] = runID
	}
	return HTTPRequest{
		Method: "POST",
		Path:   BuildAgentGetPath(workspaceID, name) + "/escalate",
		Body:   body,
	}
}

// AgentSpec is the PUT /v1/workspaces/{id}/agents/{name} create body. Give
// Content for full control, or the structured fields and let the daemon
// compose the front matter. Every field empty seeds the default AGENT.md.
type AgentSpec struct {
	Workspace   string
	Name        string // short name
	Content     string
	Prompt      string
	Harness     string
	Description string
	Cron        string
	Every       string
	Event       string
	Match       string
	Extract     string
	Rooms       []string
	Allowlist   []string
	Contains    []string
	From        string
	Title       string
	EmailAccept []string
	Template    string
	Inputs      map[string]string
}

// Build renders the spec as PUT /v1/workspaces/{id}/agents/{name} (upsert).
func (a AgentSpec) Build() HTTPRequest {
	body := map[string]any{"content": a.Content}
	for k, v := range map[string]string{
		"prompt": a.Prompt, "harness": a.Harness, "description": a.Description,
		"cron": a.Cron, "every": a.Every,
	} {
		if v != "" {
			body[k] = v
		}
	}
	for k, v := range map[string]string{"title": a.Title, "template": a.Template} {
		if v != "" {
			body[k] = v
		}
	}
	if len(a.EmailAccept) > 0 {
		body["email_accept"] = a.EmailAccept
	}
	if len(a.Inputs) > 0 {
		body["inputs"] = a.Inputs
	}
	if a.Event != "" {
		body["on"] = RuleTest{Match: a.Match, Extract: a.Extract, Rooms: a.Rooms, Allowlist: a.Allowlist, Contains: a.Contains, From: a.From}.on(a.Event)
	}
	return HTTPRequest{Method: "PUT", Path: BuildAgentGetPath(a.Workspace, a.Name), Body: body}
}

// BuildAgentPut builds PUT /v1/workspaces/{id}/agents/{name} with the full
// AGENT.md content (set).
func BuildAgentPut(workspaceID, name, content, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PUT",
		Path:    BuildAgentGetPath(workspaceID, name),
		Body:    map[string]string{"content": content},
		IfMatch: ifMatch,
	}
}

// BuildAgentPatch builds PATCH /v1/workspaces/{id}/agents/{name} with a
// JSON-merge-patch of frontmatter scalar fields.
func BuildAgentPatch(workspaceID, name string, patch map[string]any, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PATCH",
		Path:    BuildAgentGetPath(workspaceID, name),
		Body:    patch,
		IfMatch: ifMatch,
	}
}

// BuildAgentDelete builds DELETE /v1/workspaces/{id}/agents/{name}. purge
// removes the whole agents/<name>/ tree and needs confirm.
func BuildAgentDelete(workspaceID, name, ifMatch string, purge, confirm bool) HTTPRequest {
	kv := confirmQ(confirm)
	if purge {
		kv["purge"] = "1"
	}
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery(BuildAgentGetPath(workspaceID, name), kv),
		IfMatch: ifMatch,
	}
}

// BuildWorkspaceDelete builds DELETE /v1/workspaces/{id}.
func BuildWorkspaceDelete(id string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/workspaces/"+url.PathEscape(id), confirmQ(confirm))}
}

// BuildWorkspaceAdd builds POST /v1/workspaces. An empty handle lets the
// daemon derive one from the name.
func BuildWorkspaceAdd(path, name, handle string, create bool) HTTPRequest {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	if handle != "" {
		body["handle"] = handle
	}
	if path != "" {
		body["path"] = path
	}
	if create {
		body["create"] = true
	}
	return HTTPRequest{Method: "POST", Path: "/v1/workspaces", Body: body}
}

// BuildWorkspacePatch builds PATCH /v1/workspaces/{id}.
// route holds contact and notify as raw values; a nil value removes the key.
func BuildWorkspacePatch(id string, name *string, gitEnabled *bool, route map[string]any) HTTPRequest {
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if gitEnabled != nil {
		body["git_enabled"] = *gitEnabled
	}
	for k, v := range route {
		body[k] = v
	}
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/workspaces/" + url.PathEscape(id),
		Body:   body,
	}
}

// BuildWorkspaceRename builds PATCH /v1/workspaces/{id} with name.
func BuildWorkspaceRename(id, name string) HTTPRequest {
	return BuildWorkspacePatch(id, &name, nil, nil)
}

// BuildEmailStatusPath builds GET /v1/email/status.
func BuildEmailStatusPath() string {
	return "/v1/email/status"
}

// BuildEmailTest builds POST /v1/email/test (fixed message to the admins).
func BuildEmailTest() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/email/test"}
}

// BuildWorkspaceEmailGetPath builds GET /v1/workspaces/{id}/email. The local
// part is masked unless reveal is set.
func BuildWorkspaceEmailGetPath(id string, reveal bool) string {
	q := map[string]string{}
	if reveal {
		q["reveal"] = "1"
	}
	return withQuery("/v1/workspaces/"+url.PathEscape(id)+"/email", q)
}

// BuildWorkspaceEmailRotate builds POST /v1/workspaces/{id}/email/rotate.
// A non-empty handle replaces the workspace handle.
func BuildWorkspaceEmailRotate(id, handle string, confirm bool) HTTPRequest {
	req := HTTPRequest{Method: "POST",
		Path: withQuery("/v1/workspaces/"+url.PathEscape(id)+"/email/rotate", confirmQ(confirm))}
	if handle != "" {
		req.Body = map[string]any{"handle": handle}
	}
	return req
}

// BuildEmailMessagesPath builds GET /v1/email/messages (the envelope log).
// Empty filters match every row.
func BuildEmailMessagesPath(direction, workspace, thread, cursor string, limit int) string {
	q := map[string]string{
		"direction": direction,
		"workspace": workspace,
		"thread":    thread,
		"cursor":    cursor,
	}
	if limit > 0 {
		q["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/email/messages", q)
}

// BuildEmailMessagePath builds GET /v1/email/messages/{id}.
func BuildEmailMessagePath(id string) string {
	return "/v1/email/messages/" + url.PathEscape(id)
}

// BuildEmailAttachmentPath builds GET /v1/email/messages/{id}/attachments/{aid}.
func BuildEmailAttachmentPath(id, aid string) string {
	return "/v1/email/messages/" + url.PathEscape(id) + "/attachments/" + url.PathEscape(aid)
}

// BuildEmailPoll builds POST /v1/email/poll (one pull outside the tick).
func BuildEmailPoll() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/email/poll"}
}

// EmailSendSpec is POST /v1/email/send (plan I.3): one explicit message
// from the workspace address, or the agent subaddress when Agent is set.
type EmailSendSpec struct {
	Workspace   string
	Agent       string
	To          []string
	Subject     string
	Text        string
	HTML        string
	InReplyTo   string
	Attachments []EmailFile
}

// EmailFile is one file to send. Path names a workspace file that the
// daemon reads. Otherwise Name and Content (base64) carry the file.
type EmailFile struct {
	Path    string `json:"path,omitempty"`
	Name    string `json:"name,omitempty"`
	Type    string `json:"type,omitempty"`
	Content string `json:"content,omitempty"`
}

// Build renders the spec as POST /v1/email/send.
func (e EmailSendSpec) Build() HTTPRequest {
	body := map[string]any{"workspace": e.Workspace, "to": e.To, "subject": e.Subject, "text": e.Text}
	if e.Agent != "" {
		body["agent"] = e.Agent
	}
	if e.HTML != "" {
		body["html"] = e.HTML
	}
	if e.InReplyTo != "" {
		body["in_reply_to"] = e.InReplyTo
	}
	if len(e.Attachments) > 0 {
		body["attachments"] = e.Attachments
	}
	return HTTPRequest{Method: "POST", Path: "/v1/email/send", Body: body}
}

// BuildWorkspaceMove builds POST /v1/workspaces/{id}/move.
func BuildWorkspaceMove(id, path string, mkdir, dryRun, confirm bool) HTTPRequest {
	body := map[string]any{"path": path}
	if mkdir {
		body["mkdir"] = true
	}
	if dryRun {
		body["dry_run"] = true
	}
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/workspaces/"+url.PathEscape(id)+"/move", confirmQ(confirm)),
		Body:   body,
	}
}

// BuildFileListPath builds GET /v1/workspaces/{id}/files?prefix=&cursor=&limit=.
func BuildFileListPath(workspaceID, prefix, afterPath string, limit int, recursive bool) string {
	return withQuery("/v1/workspaces/"+url.PathEscape(workspaceID)+"/files",
		fileListQ(prefix, afterPath, "", limit, recursive))
}

// BuildFileGetPath builds GET /v1/workspaces/{id}/files/{path}. stat adds ?stat=1.
func BuildFileGetPath(workspaceID, rel string, stat bool) string {
	p := filePathURL(workspaceID, rel)
	if stat {
		return withQuery(p, map[string]string{"stat": "1"})
	}
	return p
}

// BuildFilePutPath builds PUT /v1/workspaces/{id}/files/{path}.
func BuildFilePutPath(workspaceID, rel string) string {
	return filePathURL(workspaceID, rel)
}

// BuildFileDeletePath builds DELETE /v1/workspaces/{id}/files/{path}.
func BuildFileDeletePath(workspaceID, rel string) string {
	return filePathURL(workspaceID, rel)
}

// BuildJobFileListPath builds GET /v1/jobs/{name}/files?prefix=&cursor=&limit=&workspace=.
func BuildJobFileListPath(name, workspace, prefix, afterPath string, limit int, recursive bool) string {
	return withQuery("/v1/jobs/"+url.PathEscape(name)+"/files",
		fileListQ(prefix, afterPath, workspace, limit, recursive))
}

// BuildJobFileGetPath builds GET /v1/jobs/{name}/files/{path}. stat adds ?stat=1.
func BuildJobFileGetPath(name, workspace, rel string, stat bool) string {
	kv := map[string]string{"workspace": workspace}
	if stat {
		kv["stat"] = "1"
	}
	return withQuery(jobFilePathURL(name, rel), kv)
}

// BuildJobFilePutPath builds PUT /v1/jobs/{name}/files/{path}?workspace=.
func BuildJobFilePutPath(name, workspace, rel string) string {
	return withQuery(jobFilePathURL(name, rel), workspaceQ(workspace))
}

// BuildJobFileDeletePath builds DELETE /v1/jobs/{name}/files/{path}?workspace=.
func BuildJobFileDeletePath(name, workspace, rel string) string {
	return withQuery(jobFilePathURL(name, rel), workspaceQ(workspace))
}

// BuildSkillFileListPath builds GET /v1/skills/{name}/files?prefix=&cursor=&limit=.
func BuildSkillFileListPath(name, prefix, afterPath string, limit int, recursive bool) string {
	return withQuery("/v1/skills/"+url.PathEscape(name)+"/files",
		fileListQ(prefix, afterPath, "", limit, recursive))
}

// BuildSkillFileGetPath builds GET /v1/skills/{name}/files/{path}. stat adds ?stat=1.
func BuildSkillFileGetPath(name, rel string, stat bool) string {
	p := skillFilePathURL(name, rel)
	if stat {
		return withQuery(p, map[string]string{"stat": "1"})
	}
	return p
}

// BuildSkillFilePutPath builds PUT /v1/skills/{name}/files/{path}.
func BuildSkillFilePutPath(name, rel string) string {
	return skillFilePathURL(name, rel)
}

// BuildSkillFileDeletePath builds DELETE /v1/skills/{name}/files/{path}.
func BuildSkillFileDeletePath(name, rel string) string {
	return skillFilePathURL(name, rel)
}

// BuildFilePut builds PUT /v1/workspaces/{id}/files/{path} with a raw body.
// contentType empty defaults to application/octet-stream at send time.
func BuildFilePut(workspaceID, rel string, content []byte, contentType, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:      "PUT",
		Path:        BuildFilePutPath(workspaceID, rel),
		Raw:         content,
		ContentType: contentType,
		IfMatch:     ifMatch,
	}
}

// BuildFileDelete builds DELETE /v1/workspaces/{id}/files/{path} with optional If-Match.
func BuildFileDelete(workspaceID, rel, ifMatch string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery(BuildFileDeletePath(workspaceID, rel), confirmQ(confirm)),
		IfMatch: ifMatch,
	}
}

// BuildJobFilePut builds PUT /v1/jobs/{name}/files/{path}?workspace= with a raw body.
func BuildJobFilePut(name, workspace, rel string, content []byte, contentType, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:      "PUT",
		Path:        BuildJobFilePutPath(name, workspace, rel),
		Raw:         content,
		ContentType: contentType,
		IfMatch:     ifMatch,
	}
}

// BuildJobFileDelete builds DELETE /v1/jobs/{name}/files/{path}?workspace= with optional If-Match.
func BuildJobFileDelete(name, workspace, rel, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    BuildJobFileDeletePath(name, workspace, rel),
		IfMatch: ifMatch,
	}
}

// BuildSkillFilePut builds PUT /v1/skills/{name}/files/{path} with a raw body.
func BuildSkillFilePut(name, rel string, content []byte, contentType, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:      "PUT",
		Path:        BuildSkillFilePutPath(name, rel),
		Raw:         content,
		ContentType: contentType,
		IfMatch:     ifMatch,
	}
}

// BuildSkillFileDelete builds DELETE /v1/skills/{name}/files/{path} with optional If-Match.
func BuildSkillFileDelete(name, rel, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    BuildSkillFileDeletePath(name, rel),
		IfMatch: ifMatch,
	}
}

// RuleTest is the trigger side of a rules test: the code and the scope.
// Replies is "continue" or "off". With Job, an event of an open conversation
// of that job is a follow-up.
type RuleTest struct {
	Match     string
	Extract   string
	Rooms     []string
	Allowlist []string
	Contains  []string
	From      string
	Replies   string
	Workspace string
	Job       string
}

// Build builds POST /v1/rules/test against a stored event (eventID) or a
// draft event. connector names the connection of a draft.
func (t RuleTest) Build(eventID, connector string, event any) HTTPRequest {
	body := t.body()
	if eventID != "" {
		body["event_id"] = eventID
	} else {
		body["event"] = event
	}
	if connector != "" {
		body["connector"] = connector
	}
	return HTTPRequest{Method: "POST", Path: "/v1/rules/test", Body: body}
}

// BuildBatch builds POST /v1/rules/test-batch over stored events.
func (t RuleTest) BuildBatch(eventIDs []string) HTTPRequest {
	body := t.body()
	body["event_ids"] = eventIDs
	return HTTPRequest{Method: "POST", Path: "/v1/rules/test-batch", Body: body}
}

// BuildRulesFire builds POST /v1/rules/fire.
func BuildRulesFire(connector, workspace, job, match, extract string, event any, onIndex int) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/rules/fire",
		Body: map[string]any{
			"connector": connector,
			"workspace": workspace,
			"job":       job,
			"match":     match,
			"extract":   extract,
			"event":     event,
			"on_index":  onIndex,
		},
	}
}

// BuildEventsListPath builds GET /v1/events?cursor=&limit=&connector=&since=&q=.
func BuildEventsListPath(cursor string, limit int, connector, since, q string) string {
	return withQuery("/v1/events", mergeQ(
		map[string]string{"cursor": cursor, "connector": connector, "since": since, "q": q},
		limitQ(limit)))
}

// BuildEventGet builds GET /v1/events/{id}.
func BuildEventGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/events/" + url.PathEscape(id)}
}

// BuildEventsRetrigger builds POST /v1/events/{id}/retrigger.
func BuildEventsRetrigger(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/events/" + url.PathEscape(id) + "/retrigger",
	}
}

// BuildConversationsListPath builds GET /v1/conversations?workspace=&channel=&cursor=&limit=.
func BuildConversationsListPath(workspace, channel, operator, cursor string, limit int) string {
	kv := map[string]string{"workspace": workspace, "channel": channel, "operator": operator, "cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/conversations", kv)
}

// BuildConversationGet builds GET /v1/conversations/{id}.
func BuildConversationGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/conversations/" + url.PathEscape(id)}
}

// BuildConversationCreate builds POST /v1/conversations.
func BuildConversationCreate(workspace, title, originChannel, connector, room, operator string) HTTPRequest {
	body := map[string]any{"workspace": workspace, "origin_channel": originChannel}
	if title != "" {
		body["title"] = title
	}
	if connector != "" && room != "" {
		body["activate"] = map[string]any{"connector": connector, "room": room}
	}
	if operator != "" {
		body["operator"] = operator
	}
	return HTTPRequest{Method: "POST", Path: "/v1/conversations", Body: body}
}

// BuildAgentConversationsPath builds the filtered agent conversation list.
func BuildAgentConversationsPath(workspace, agent string) string {
	return "/v1/workspaces/" + url.PathEscape(workspace) + "/agents/" +
		url.PathEscape(agent) + "/conversations"
}

// BuildConversationPatch builds PATCH /v1/conversations/{id}.
// A nil title or summary is left alone; a pointer to "" clears the field.
func BuildConversationPatch(id string, title, summary *string, revision int64) HTTPRequest {
	body := map[string]any{}
	if title != nil {
		body["title"] = *title
	}
	if summary != nil {
		body["summary"] = *summary
	}
	if revision > 0 {
		body["revision"] = revision
	}
	return HTTPRequest{Method: "PATCH", Path: "/v1/conversations/" + url.PathEscape(id), Body: body}
}

// BuildConversationMessagesPath builds GET /v1/conversations/{id}/messages.
func BuildConversationMessagesPath(id, cursor string, limit int) string {
	return listQuery("/v1/conversations/"+url.PathEscape(id)+"/messages", cursor, limit)
}

// BuildConversationSend builds POST /v1/conversations/{id}/messages.
func BuildConversationSend(id, text, connector, room, idempotencyKey string) HTTPRequest {
	body := map[string]any{"text": text, "connector": connector, "room": room}
	if idempotencyKey != "" {
		body["idempotency_key"] = idempotencyKey
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/conversations/" + url.PathEscape(id) + "/messages",
		Body:   body,
	}
}

// BuildConversationDelete builds DELETE /v1/conversations/{id}. It closes the
// assistant process and deletes the conversation with its messages, events,
// and session row. confirm rides the shared destructive gate.
func BuildConversationDelete(id string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   withQuery("/v1/conversations/"+url.PathEscape(id), confirmQ(confirm)),
	}
}

// BuildConversationEventsPath builds GET /v1/conversations/{id}/events.
// after is the last seq seen; follow=1 streams live events as SSE and must
// not be used by a one-shot caller.
func BuildConversationEventsPath(id string, after int64, limit int, follow bool) string {
	q := map[string]string{}
	if after > 0 {
		q["after"] = strconv.FormatInt(after, 10)
	}
	if limit > 0 {
		q["limit"] = strconv.Itoa(limit)
	}
	if follow {
		q["follow"] = "1"
	}
	return withQuery("/v1/conversations/"+url.PathEscape(id)+"/events", q)
}

// BuildConversationInterrupt builds POST /v1/conversations/{id}/interrupt.
func BuildConversationInterrupt(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/conversations/" + url.PathEscape(id) + "/interrupt"}
}

// BuildConversationClose builds POST /v1/conversations/{id}/close. The
// conversation stays; the process goes.
func BuildConversationClose(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/conversations/" + url.PathEscape(id) + "/close"}
}

// BuildConversationPermission builds POST /v1/conversations/{id}/permissions/{pid}.
// decision is allow_once|allow_session|deny.
func BuildConversationPermission(id, permissionID, decision, message string) HTTPRequest {
	body := map[string]any{"decision": decision}
	if message != "" {
		body["message"] = message
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/conversations/" + url.PathEscape(id) + "/permissions/" + url.PathEscape(permissionID),
		Body:   body,
	}
}

// BuildConversationMode builds POST /v1/conversations/{id}/mode. It applies
// at the next process start.
func BuildConversationMode(id, mode string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/conversations/" + url.PathEscape(id) + "/mode",
		Body:   map[string]string{"mode": mode},
	}
}

// BuildConversationRevert builds POST /v1/conversations/{id}/revert for one
// pre-edit snapshot file.
func BuildConversationRevert(id string, turn int, path string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/conversations/" + url.PathEscape(id) + "/revert",
		Body:   map[string]any{"turn": turn, "path": path},
	}
}

// BuildConversationOutboxPath builds GET /v1/conversations/{id}/outbox.
func BuildConversationOutboxPath(id string) string {
	return "/v1/conversations/" + url.PathEscape(id) + "/outbox"
}

// BuildConversationOutboxRetry builds POST /v1/conversations/{id}/outbox/{row}/retry.
func BuildConversationOutboxRetry(id, row string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/conversations/" + url.PathEscape(id) + "/outbox/" + url.PathEscape(row) + "/retry",
	}
}

// BuildPointerGet builds GET /v1/channel-pointers/{connector}/{room}?workspace=&thread=.
// Pointers are workspace-scoped, so the caller names the workspace.
func BuildPointerGet(connector, room, thread, workspace string) HTTPRequest {
	return HTTPRequest{
		Method: "GET",
		Path: withQuery("/v1/channel-pointers/"+url.PathEscape(connector)+"/"+url.PathEscape(room),
			map[string]string{"workspace": workspace, "thread": thread}),
	}
}

// BuildPointerPut builds PUT /v1/channel-pointers/{connector}/{room}.
func BuildPointerPut(connector, room, thread, conversationID string) HTTPRequest {
	body := map[string]any{"conversation_id": conversationID}
	if thread != "" {
		body["thread"] = thread
	}
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/channel-pointers/" + url.PathEscape(connector) + "/" + url.PathEscape(room),
		Body:   body,
	}
}

// BuildApprovalsListPath builds GET /v1/approvals?cursor=&limit=&state=.
func BuildApprovalsListPath(cursor string, limit int, state string) string {
	return withQuery(listQuery("/v1/approvals", cursor, limit), map[string]string{"state": state})
}

// BuildServiceShutdown builds POST /v1/system/shutdown.
func BuildServiceShutdown(confirm bool) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery("/v1/system/shutdown", confirmQ(confirm))}
}

// BuildSystemHealthPath builds GET /v1/system/health.
func BuildSystemHealthPath() string {
	return "/v1/system/health"
}

// BuildSystemDoctorPath builds GET /v1/system/doctor.
func BuildSystemDoctorPath() string {
	return "/v1/system/doctor"
}

// BuildSystemLicenseGetPath builds GET /v1/system/license.
func BuildSystemLicenseGetPath() string {
	return "/v1/system/license"
}

// BuildSystemLicenseInstall builds PUT /v1/system/license with the raw key.
func BuildSystemLicenseInstall(raw string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/system/license",
		Body:   map[string]string{"license": raw},
	}
}

// BuildSystemLogsPath builds GET /v1/system/logs (last 200 daemon log lines).
func BuildSystemLogsPath() string {
	return "/v1/system/logs"
}

// BuildSystemUpdatePath builds GET /v1/system/update (version check).
func BuildSystemUpdatePath() string {
	return "/v1/system/update"
}

// BuildSystemGuidesPath builds GET /v1/system/guides.
func BuildSystemGuidesPath() string {
	return "/v1/system/guides"
}

// BuildSystemGuidePath builds GET /v1/system/guides/{topic}.
func BuildSystemGuidePath(topic string) string {
	return "/v1/system/guides/" + url.PathEscape(topic)
}

// BuildSystemDrain builds POST /v1/system/drain.
func BuildSystemDrain() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/system/drain"}
}

// BuildSystemUndrain builds POST /v1/system/undrain.
func BuildSystemUndrain() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/system/undrain"}
}

// BuildSystemOverviewPath builds GET /v1/system/overview.
func BuildSystemOverviewPath() string {
	return "/v1/system/overview"
}

// BuildCloudStatusPath builds GET /v1/cloud/status.
func BuildCloudStatusPath() string {
	return "/v1/cloud/status"
}

// BuildCloudAddressesPath builds GET /v1/cloud/addresses. The rows are
// masked unless reveal is set.
func BuildCloudAddressesPath(reveal bool) string {
	q := map[string]string{}
	if reveal {
		q["reveal"] = "1"
	}
	return withQuery("/v1/cloud/addresses", q)
}

// BuildCloudLink builds POST /v1/cloud/link: exchange the enrollment code
// from the account page for a device identity.
func BuildCloudLink(enrollToken, name string) HTTPRequest {
	body := map[string]any{"enrollment_token": enrollToken}
	if name != "" {
		body["name"] = name
	}
	return HTTPRequest{Method: "POST", Path: "/v1/cloud/link", Body: body}
}

// BuildCloudUnlink builds DELETE /v1/cloud/link.
func BuildCloudUnlink(confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/cloud/link", confirmQ(confirm))}
}

// BuildHooksPath builds GET /v1/hooks. The URLs are masked unless reveal
// is set: the token is the URL, so it is a weak credential.
func BuildHooksPath(reveal bool) string {
	q := map[string]string{}
	if reveal {
		q["reveal"] = "1"
	}
	return withQuery("/v1/hooks", q)
}

// BuildHookCreate builds POST /v1/hooks: mint one hosted webhook and get
// its URL back once.
func BuildHookCreate(workspace, label string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/hooks",
		Body: map[string]any{"workspace_id": workspace, "label": label}}
}

// BuildHookDelete builds DELETE /v1/hooks/{id}.
func BuildHookDelete(id string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE",
		Path: withQuery("/v1/hooks/"+url.PathEscape(id), confirmQ(confirm))}
}

// BuildQueueRequeue builds POST /v1/queue/{id}/requeue.
func BuildQueueRequeue(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/queue/" + url.PathEscape(id) + "/requeue"}
}

// BuildQueueCancel builds POST /v1/queue/{id}/cancel.
func BuildQueueCancel(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/queue/" + url.PathEscape(id) + "/cancel"}
}

// BuildQueueCancelBulk builds POST /v1/queue/cancel with an id list.
func BuildQueueCancelBulk(ids []string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/queue/cancel", Body: map[string]any{"ids": ids}}
}

// BuildQueueListPath builds GET /v1/queue?status=&cursor=&limit=.
func BuildQueueListPath(status, cursor string, limit int) string {
	kv := map[string]string{"status": status, "cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/queue", kv)
}

// BuildSchedulesListPath builds GET /v1/schedules?limit=&workspace=&due_before=.
func BuildSchedulesListPath(limit int, workspace, dueBefore string) string {
	return withQuery("/v1/schedules", mergeQ(
		map[string]string{"workspace": workspace, "due_before": dueBefore},
		limitQ(limit)))
}

// BuildConfigValidatePath builds GET /v1/config/validate.
func BuildConfigValidatePath() string { return "/v1/config/validate" }

// BuildConfigSchemaPath builds GET /v1/config/schema.
func BuildConfigSchemaPath() string { return "/v1/config/schema" }

// BuildConfigYAMLPath builds GET /v1/config/yaml.
func BuildConfigYAMLPath() string { return "/v1/config/yaml" }

// BuildConfigValidateDraft builds POST /v1/config/validate. A non-nil yaml
// replaces the file; set and unset change a copy of it.
func BuildConfigValidateDraft(set map[string]any, unset []string, yaml *string) HTTPRequest {
	body := map[string]any{}
	if len(set) > 0 {
		body["set"] = set
	}
	if len(unset) > 0 {
		body["unset"] = unset
	}
	if yaml != nil {
		body["yaml"] = *yaml
	}
	return HTTPRequest{Method: "POST", Path: "/v1/config/validate", Body: body}
}

// BuildSkillsListPath builds GET /v1/skills?limit=.
func BuildSkillsListPath(limit int) string {
	return listQuery("/v1/skills", "", limit)
}

// BuildConnectorsListPath builds GET /v1/connectors with optional filters.
// A workspace resolves each row with the layer of that workspace.
func BuildConnectorsListPath(limit int, capability, provenance, workspace string) string {
	kv := map[string]string{"capability": capability, "provenance": provenance, "workspace": workspace}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/connectors", kv)
}

// BuildInboxListPath builds GET /v1/inbox query from list flags.
func BuildInboxListPath(status, kind, cursor string, limit int) string {
	kv := map[string]string{"status": status, "kind": kind, "cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/inbox", kv)
}

// BuildInboxGet builds GET /v1/inbox/{id}.
func BuildInboxGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/inbox/" + url.PathEscape(id)}
}

// BuildInboxRead builds POST /v1/inbox/{id}/read.
func BuildInboxRead(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/inbox/" + url.PathEscape(id) + "/read"}
}

// BuildInboxResolve builds POST /v1/inbox/{id}/resolve.
func BuildInboxResolve(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/inbox/" + url.PathEscape(id) + "/resolve"}
}

// BuildInboxReadAll builds POST /v1/inbox/read-all.
func BuildInboxReadAll() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/inbox/read-all"}
}

// BuildInboxResolveAll builds POST /v1/inbox/resolve-all. An empty kind covers
// every kind.
func BuildInboxResolveAll(kind string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery("/v1/inbox/resolve-all", map[string]string{"kind": kind})}
}

// BuildAlertsPath builds GET /v1/alerts?since=.
func BuildAlertsPath(since string, limit int) string {
	return withQuery("/v1/alerts", mergeQ(map[string]string{"since": since}, limitQ(limit)))
}

// AskSpec is POST /v1/workspaces/{id}/assistant/messages. The daemon resolves
// or creates the channel conversation, so a client sends no pointer calls.
type AskSpec struct {
	Workspace      string
	Text           string
	Connector      string // defaults to cli server-side
	Room           string // defaults to the workspace id server-side
	IdempotencyKey string
	WaitMS         int // long-poll for the assistant reply
}

// Build renders the ask as POST /v1/workspaces/{id}/assistant/messages.
func (a AskSpec) Build() HTTPRequest {
	body := map[string]any{"text": a.Text}
	for k, v := range map[string]string{
		"connector":       a.Connector,
		"room":            a.Room,
		"idempotency_key": a.IdempotencyKey,
	} {
		if v != "" {
			body[k] = v
		}
	}
	return HTTPRequest{
		Method: "POST",
		Path: withQuery("/v1/workspaces/"+url.PathEscape(a.Workspace)+"/assistant/messages",
			waitQ(a.WaitMS)),
		Body: body,
	}
}

// AppendExtraPrompt joins the job base prompt with extra text (trebi run -e semantics).
func AppendExtraPrompt(base, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return base
	}
	base = strings.TrimRight(base, "\n")
	if base == "" {
		return extra
	}
	return base + "\n" + extra
}

// RunsQuery is the filter set of GET /v1/runs. Harness filters runs.harness;
// Model filters tokens_model. Since and Until take RFC3339 or a duration such
// as 24h.
type RunsQuery struct {
	Status       string
	Job          string
	Workspace    string
	Kind         string
	Cursor       string
	Harness      string
	Model        string
	Conversation string
	EndReason    string
	Q            string
	Since        string
	Until        string
	Limit        int
}

// Path renders the query as GET /v1/runs.
func (r RunsQuery) Path() string {
	kv := map[string]string{
		"status":       r.Status,
		"job":          r.Job,
		"workspace":    r.Workspace,
		"kind":         r.Kind,
		"cursor":       r.Cursor,
		"harness":      r.Harness,
		"model":        r.Model,
		"conversation": r.Conversation,
		"end_reason":   r.EndReason,
		"q":            r.Q,
		"since":        r.Since,
		"until":        r.Until,
	}
	if r.Limit > 0 {
		kv["limit"] = strconv.Itoa(r.Limit)
	}
	return withQuery("/v1/runs", kv)
}

// MatchConversation reports whether conversation_key contains substr (case-insensitive).
// Empty substr matches all.
func MatchConversation(conversationKey, substr string) bool {
	if substr == "" {
		return true
	}
	return strings.Contains(strings.ToLower(conversationKey), strings.ToLower(substr))
}

// BuildJobsListPath builds GET /v1/jobs?workspace=&q=&harness=&active=.
func BuildJobsListPath(workspace, q, harness, active string) string {
	return withQuery("/v1/jobs", mergeQ(workspaceQ(workspace),
		map[string]string{"q": q, "harness": harness, "active": active}))
}

// JobSpec is the POST /v1/jobs body. Give Content for full control, or the
// structured fields and let the daemon compose the front matter.
type JobSpec struct {
	Name           string
	Workspace      string
	Content        string
	Prompt         string
	Harness        string
	Model          string
	Effort         string
	Description    string
	Cron           string
	Every          string
	Event          string
	Match          string
	Extract        string
	IdempotencyKey string
	Agent          string // short name; the file lands under agents/<agent>/
	Rooms          []string
	Allowlist      []string
	Contains       []string
	From           string
	Replies        string
}

// Build renders the spec as POST /v1/jobs.
func (j JobSpec) Build() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/jobs", Body: j.body()}
}

// BuildValidate renders the spec as POST /v1/jobs/validate, which checks the
// definition without writing a job.
func (j JobSpec) BuildValidate() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/jobs/validate", Body: j.body()}
}

func (j JobSpec) body() map[string]any {
	body := map[string]any{"name": j.Name, "workspace": j.Workspace, "content": j.Content}
	for k, v := range map[string]string{
		"prompt": j.Prompt, "harness": j.Harness, "model": j.Model, "effort": j.Effort,
		"description": j.Description,
		"cron":        j.Cron, "every": j.Every, "idempotency_key": j.IdempotencyKey,
		"agent": j.Agent,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if j.Event != "" {
		body["on"] = RuleTest{Match: j.Match, Extract: j.Extract, Rooms: j.Rooms, Allowlist: j.Allowlist, Contains: j.Contains, From: j.From, Replies: j.Replies}.on(j.Event)
	}
	return body
}

// BuildJobPatch builds PATCH /v1/jobs/{name}?workspace= with a JSON-merge-patch
// of frontmatter scalar fields. ifMatch is sent as If-Match when non-empty.
func BuildJobPatch(name, workspace string, patch map[string]any, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PATCH",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name), workspaceQ(workspace)),
		Body:    patch,
		IfMatch: ifMatch,
	}
}

// BuildJobGetPath builds GET /v1/jobs/{name}?workspace=.
func BuildJobGetPath(name, workspace string) string {
	return withQuery("/v1/jobs/"+url.PathEscape(name), workspaceQ(workspace))
}

// BuildJobValidatePath builds POST /v1/jobs/{name}/validate?workspace=.
func BuildJobValidatePath(name, workspace string) string {
	return withQuery("/v1/jobs/"+url.PathEscape(name)+"/validate", workspaceQ(workspace))
}

// BuildJobDelete builds DELETE /v1/jobs/{name}.
func BuildJobDelete(name, workspace, ifMatch string, purge, confirm bool) HTTPRequest {
	kv := mergeQ(workspaceQ(workspace), confirmQ(confirm))
	if purge {
		kv["purge"] = "1"
	}
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name), kv),
		IfMatch: ifMatch,
	}
}

// BuildJobPause builds POST pause/unpause.
func BuildJobPause(name, workspace, ifMatch string, pause bool) HTTPRequest {
	op := "unpause"
	if pause {
		op = "pause"
	}
	return HTTPRequest{
		Method:  "POST",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name)+"/"+op, workspaceQ(workspace)),
		IfMatch: ifMatch,
	}
}

// BuildJobSchedule describes how to apply a schedule value.
// enable/disable → pause endpoints; cron/@every → caller must GET markdown, ApplyScheduleMarkdown, PUT.
type JobSchedulePlan struct {
	Mode string // "pause" | "unpause" | "put_markdown"
	HTTP HTTPRequest
}

// PlanJobSchedule maps enable|disable|cron|@every to an API plan.
// For put_markdown, HTTP is empty — use ApplyScheduleMarkdown + BuildJobPutMarkdown.
func PlanJobSchedule(name, workspace, value string) (JobSchedulePlan, error) {
	v := strings.TrimSpace(value)
	switch v {
	case "enable":
		return JobSchedulePlan{Mode: "unpause", HTTP: BuildJobPause(name, workspace, "", false)}, nil
	case "disable":
		return JobSchedulePlan{Mode: "pause", HTTP: BuildJobPause(name, workspace, "", true)}, nil
	case "":
		return JobSchedulePlan{}, fmt.Errorf("schedule value required (enable|disable|cron|@every …)")
	default:
		return JobSchedulePlan{Mode: "put_markdown"}, nil
	}
}

// BuildJobPutMarkdown builds PUT /v1/jobs/{name} with full markdown body.
func BuildJobPutMarkdown(name, workspace, markdown, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PUT",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name), workspaceQ(workspace)),
		Body:    map[string]any{"markdown": markdown},
		IfMatch: ifMatch,
	}
}

// ApplyScheduleMarkdown replaces time-based triggers (cron/every) with value.
// Event triggers are preserved. value is cron, comma-separated crons, or @every 30m.
func ApplyScheduleMarkdown(markdown, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("empty schedule")
	}
	fm, body, err := splitFrontmatter(markdown)
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return "", fmt.Errorf("parse frontmatter: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	// Drop legacy schedule key if present (rejected by trebi parser).
	delete(doc, "schedule")

	kept := keepEventTriggers(doc["triggers"])
	timed, err := timeTriggers(value)
	if err != nil {
		return "", err
	}
	kept = append(kept, timed...)
	if len(kept) == 0 {
		delete(doc, "triggers")
	} else {
		doc["triggers"] = kept
	}
	outFM, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "---\n" + string(outFM) + "---\n" + body, nil
}

// BuildHarnessesListPath builds GET /v1/harnesses.
func BuildHarnessesListPath() string {
	return "/v1/harnesses"
}

// BuildHarnessLoginCancel builds DELETE /v1/harnesses/{name}/login.
func BuildHarnessLoginCancel(name string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/login",
	}
}

// BuildHarnessEnsure builds POST /v1/harnesses/{name}/ensure.
func BuildHarnessEnsure(name string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/ensure",
	}
}

// BuildHarnessAuthGet builds GET /v1/harnesses/{name}/auth.
func BuildHarnessAuthGet(name string, live bool) HTTPRequest {
	path := "/v1/harnesses/" + url.PathEscape(name) + "/auth"
	if live {
		path += "?live=1"
	}
	return HTTPRequest{Method: "GET", Path: path}
}

// BuildHarnessModels builds GET /v1/harnesses/{name}/models.
func BuildHarnessModels(name string) HTTPRequest {
	return HTTPRequest{
		Method: "GET",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/models",
	}
}

// BuildHarnessPatch builds PATCH /v1/harnesses/{name}.
func BuildHarnessPatch(name, authMode string) HTTPRequest {
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/harnesses/" + url.PathEscape(name),
		Body:   map[string]any{"auth_mode": authMode},
	}
}

// BuildHarnessLogout builds POST /v1/harnesses/{name}/logout.
func BuildHarnessLogout(name, mode string, confirm bool) HTTPRequest {
	body := map[string]any{"confirm": confirm}
	if mode != "" {
		body["mode"] = mode
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/logout",
		Body:   body,
	}
}

// BuildHarnessLoginStart builds POST /v1/harnesses/{name}/login (interactive).
func BuildHarnessLoginStart(name, mode string) HTTPRequest {
	body := map[string]any{}
	if mode != "" {
		body["mode"] = mode
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/login",
		Body:   body,
	}
}

// BuildHarnessLoginToken builds POST /v1/harnesses/{name}/login with an API key.
func BuildHarnessLoginToken(name, token string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/login",
		Body:   map[string]any{"token": token},
	}
}

// BuildHarnessLoginCode builds POST /v1/harnesses/{name}/login with a paste-back code.
func BuildHarnessLoginCode(name, code string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/login",
		Body:   map[string]any{"code": code},
	}
}

// BuildHarnessLoginGet builds GET /v1/harnesses/{name}/login.
func BuildHarnessLoginGet(name string) HTTPRequest {
	return HTTPRequest{
		Method: "GET",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/login",
	}
}

// BuildConnectorGetPath builds GET /v1/connectors/{ref}?workspace=.
func BuildConnectorGetPath(ref, workspace string) string {
	return withQuery("/v1/connectors/"+url.PathEscape(ref), map[string]string{"workspace": workspace})
}

// BuildConnectorCreate builds POST /v1/connectors with a connection:
// {type, slug, title, from, version, track, inputs, patch, manifest}.
func BuildConnectorCreate(conn map[string]any) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/connectors", Body: conn}
}

// BuildConnectorRestart builds POST /v1/connectors/{ref}/restart.
func BuildConnectorRestart(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: c.path("/restart", nil)}
}

// ConnectorPatch is the PATCH /v1/connectors/{ref} body. A nil field
// stays. Patch and Manifest are a JSON object, or raw "null" to clear.
type ConnectorPatch struct {
	Ref                         string
	Slug, Title, Version, Track *string
	Inputs                      map[string]string // nil leaves the stored inputs
	Patch, Manifest             json.RawMessage
}

// Build renders the patch as PATCH /v1/connectors/{ref}.
func (i ConnectorPatch) Build() HTTPRequest {
	body := map[string]any{}
	if i.Slug != nil {
		body["slug"] = *i.Slug
	}
	if i.Title != nil {
		body["title"] = *i.Title
	}
	if i.Version != nil {
		body["version"] = *i.Version
	}
	if i.Track != nil {
		body["track"] = *i.Track
	}
	if i.Inputs != nil {
		body["inputs"] = i.Inputs
	}
	if i.Patch != nil {
		body["patch"] = i.Patch
	}
	if i.Manifest != nil {
		body["manifest"] = i.Manifest
	}
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/connectors/" + url.PathEscape(i.Ref),
		Body:   body,
	}
}

// BuildConnectorDelete builds DELETE /v1/connectors/{ref}.
func BuildConnectorDelete(ref string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   withQuery("/v1/connectors/"+url.PathEscape(ref), confirmQ(confirm)),
	}
}

// BuildConnectorTest builds POST /v1/connectors/{ref}/test.
func BuildConnectorTest(ref string, event any) HTTPRequest {
	body := map[string]any{}
	if event != nil {
		body["event"] = event
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/connectors/" + url.PathEscape(ref) + "/test",
		Body:   body,
	}
}

// BuildConnectorLogsPath builds GET /v1/connectors/{ref}/logs.
func BuildConnectorLogsPath(c ConnectorRef, follow bool) string {
	kv := map[string]string{}
	if follow {
		kv["follow"] = "1"
	}
	return c.path("/logs", kv)
}

// BuildConnectorOverrideGet builds GET /v1/workspaces/{id}/connectors/{ref}.
func BuildConnectorOverrideGet(workspace, ref string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: connectorOverridePath(workspace, ref)}
}

// BuildConnectorOverrideSet builds PUT /v1/workspaces/{id}/connectors/{ref}
// with the whole layer: {patch, disabled}.
func BuildConnectorOverrideSet(workspace, ref string, layer map[string]any) HTTPRequest {
	return HTTPRequest{Method: "PUT", Path: connectorOverridePath(workspace, ref), Body: layer}
}

// BuildConnectorOverrideDelete builds DELETE /v1/workspaces/{id}/connectors/{ref}.
func BuildConnectorOverrideDelete(workspace, ref string) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: connectorOverridePath(workspace, ref)}
}

// BuildConnectorLogin builds POST /v1/connectors/{ref}/login. An empty kind
// lets the adapter choose.
func BuildConnectorLogin(c ConnectorRef, kind string) HTTPRequest {
	body := map[string]any{}
	if kind != "" {
		body["kind"] = kind
	}
	return HTTPRequest{Method: "POST", Path: c.path("/login", nil), Body: body}
}

// BuildConnectorLoginStreamPath builds GET /v1/connectors/{ref}/login/{flow_id}.
func BuildConnectorLoginStreamPath(ref, flowID string) string {
	return "/v1/connectors/" + url.PathEscape(ref) + "/login/" + url.PathEscape(flowID)
}

// BuildConnectorLoginSubmit builds POST /v1/connectors/{ref}/login/{flow_id}.
func BuildConnectorLoginSubmit(name, flowID string, fields map[string]string) HTTPRequest {
	if fields == nil {
		fields = map[string]string{}
	}
	return HTTPRequest{
		Method: "POST",
		Path:   BuildConnectorLoginStreamPath(name, flowID),
		Body:   map[string]any{"fields": fields},
	}
}

// BuildConnectorLoginCancel builds DELETE /v1/connectors/{ref}/login/{flow_id}.
func BuildConnectorLoginCancel(name, flowID string) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: BuildConnectorLoginStreamPath(name, flowID)}
}

// BuildConnectorLogout builds POST /v1/connectors/{ref}/logout.
func BuildConnectorLogout(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: c.path("/logout", nil), Body: map[string]any{}}
}

// BuildConnectorRoomsPath builds GET /v1/connectors/{ref}/rooms.
func BuildConnectorRoomsPath(c ConnectorRef, q, kind, cursor string, limit int) string {
	kv := map[string]string{"q": q, "kind": kind, "cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return c.path("/rooms", kv)
}

// BuildConnectorSendersPath builds GET /v1/connectors/{ref}/senders.
func BuildConnectorSendersPath(c ConnectorRef, q string, limit int) string {
	kv := map[string]string{"q": q}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return c.path("/senders", kv)
}

// BuildConnectorDependentsPath builds GET /v1/connectors/{ref}/dependents.
func BuildConnectorDependentsPath(c ConnectorRef) string {
	return c.path("/dependents", nil)
}

// BuildConnectorThreadsPath builds GET /v1/connectors/{ref}/rooms/{room}/threads.
func BuildConnectorThreadsPath(c ConnectorRef, room, cursor string, limit int) string {
	kv := map[string]string{"cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return c.path("/rooms/"+url.PathEscape(room)+"/threads", kv)
}

// BuildConnectorBind builds POST /v1/connectors/{ref}/rooms/{room}/bind.
// The operator is "assistant" or "agents/<name>"; the workspace is required.
func BuildConnectorBind(c ConnectorRef, workspace, room, thread, operator string) HTTPRequest {
	body := map[string]any{}
	if workspace != "" {
		body["workspace"] = workspace
	}
	if thread != "" {
		body["thread"] = thread
	}
	if operator != "" {
		body["operator"] = operator
	}
	return HTTPRequest{
		Method: "POST",
		Path:   c.path("/rooms/"+url.PathEscape(room)+"/bind", nil),
		Body:   body,
	}
}

// BuildConnectorCatalogPath builds GET /v1/connectors/catalog.
func BuildConnectorCatalogPath(q, registry, cursor string, limit int) string {
	kv := map[string]string{"q": q, "registry": registry, "cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/connectors/catalog", kv)
}

// BuildConnectorCatalogEntryPath builds GET /v1/connectors/catalog/entry.
func BuildConnectorCatalogEntryPath(registry, name, version string) string {
	return withQuery("/v1/connectors/catalog/entry", map[string]string{
		"registry": registry, "name": name, "version": version,
	})
}

// ConnectorInstall is the POST /v1/connectors/install body. Package and
// Remote are indices into the entry; -1 omits one. Track applies to a Trebi
// catalog entry only. Slug and Title name the new connection.
type ConnectorInstall struct {
	Registry, Name, Version, Track, Slug, Title string
	Package, Remote                             int
	Inputs                                      map[string]string
}

// Build returns the install request.
func (i ConnectorInstall) Build() HTTPRequest {
	body := map[string]any{"registry": i.Registry, "name": i.Name}
	if i.Version != "" {
		body["version"] = i.Version
	}
	if i.Track != "" {
		body["track"] = i.Track
	}
	if i.Slug != "" {
		body["slug"] = i.Slug
	}
	if i.Title != "" {
		body["title"] = i.Title
	}
	if i.Package >= 0 {
		body["package"] = i.Package
	}
	if i.Remote >= 0 {
		body["remote"] = i.Remote
	}
	if len(i.Inputs) > 0 {
		body["inputs"] = i.Inputs
	}
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/install", Body: body}
}

// BuildConnectorVersions builds GET /v1/connectors/{ref}/versions.
func BuildConnectorVersions(ref string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/connectors/" + url.PathEscape(ref) + "/versions"}
}

// BuildConnectorCheckUpdates builds POST /v1/connectors/{ref}/check-updates,
// or POST /v1/connectors/check-updates for every connection with no ref.
func BuildConnectorCheckUpdates(ref string) HTTPRequest {
	if ref == "" {
		return HTTPRequest{Method: "POST", Path: "/v1/connectors/check-updates"}
	}
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/" + url.PathEscape(ref) + "/check-updates"}
}

// BuildConnectorUpdate builds POST /v1/connectors/{ref}/update. No version
// is the newest version on the track.
func BuildConnectorUpdate(ref, version string) HTTPRequest {
	body := map[string]any{}
	if version != "" {
		body["version"] = version
	}
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/" + url.PathEscape(ref) + "/update", Body: body}
}

// BuildConnectorCapabilities builds GET /v1/connectors/{ref}/capabilities.
// A workspace applies its layer.
func BuildConnectorCapabilities(c ConnectorRef, workspace string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: c.path("/capabilities", map[string]string{"workspace": workspace})}
}

// BuildConnectorConformance builds POST /v1/connectors/{ref}/conformance.
// No suites runs each suite that the manifest has.
func BuildConnectorConformance(c ConnectorRef, room string, suites []string) HTTPRequest {
	body := map[string]any{}
	if room != "" {
		body["room"] = room
	}
	if len(suites) > 0 {
		body["suites"] = suites
	}
	return HTTPRequest{Method: "POST", Path: c.path("/conformance", nil), Body: body}
}

// BuildConnectorGC builds POST /v1/connectors/gc.
func BuildConnectorGC(dryRun bool) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/gc", Body: map[string]any{"dry_run": dryRun}}
}

// BuildConfigGet builds GET /v1/config.
func BuildConfigGet() HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/config"}
}

// BuildConfigSet builds PATCH /v1/config with one dotted key.
func BuildConfigSet(key, value string) HTTPRequest {
	return BuildConfigSetValues(map[string]any{key: value})
}

// BuildConfigSetValues builds PATCH /v1/config with several dotted keys. The
// values keep their JSON type: the handler accepts any scalar.
func BuildConfigSetValues(values map[string]any) HTTPRequest {
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/config",
		Body:   map[string]any{"set": values},
	}
}

// BuildConfigUnset builds PATCH /v1/config with an unset list.
func BuildConfigUnset(key string) HTTPRequest {
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/config",
		Body:   map[string]any{"unset": []string{key}},
	}
}

// BuildConfigBridgeSet builds PATCH body for a per-tool/global bridge resource mode.
// tool empty → tools.bridge.resources.<resource>
// tool set → tools.<tool>.bridge.resources.<resource>
// For tools.bridge.never appends, use mergeBridgeNever + BuildConfigSet (do not replace here).
func BuildConfigBridgeSet(tool, resource, mode string) (HTTPRequest, error) {
	norm, ok := normalizeBridgeModeCLI(mode)
	if !ok {
		return HTTPRequest{}, fmt.Errorf("mode %q not in [inherit|true|link|false|off|none]", mode)
	}
	key := "tools.bridge.resources." + resource
	if tool != "" {
		key = "tools." + tool + ".bridge.resources." + resource
	}
	return BuildConfigSet(key, norm), nil
}

// FilterJobsByKindFlag keeps rows matching --kind (job|agent|all).
func FilterJobsByKindFlag(rows []map[string]any, kind string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, j := range rows {
		rowKind, _ := j["kind"].(string)
		if keepJobKind(kind, rowKind) {
			out = append(out, j)
		}
	}
	return out
}

// ParseJobListKind validates a --kind flag value (job|agent|all). The
// assistant is not a job kind; its sessions list lives under `trebi
// assistant sessions`.
func ParseJobListKind(s string) (string, error) {
	switch strings.TrimSpace(s) {
	case "", "job":
		return "job", nil
	case "agent", "all":
		return s, nil
	default:
		return "", fmt.Errorf("invalid --kind %q (want job|agent|all)", s)
	}
}

func keepJobKind(want, rowKind string) bool {
	switch want {
	case "all":
		return true
	case "agent":
		return rowKind == "agent"
	default:
		return rowKind == "job" || rowKind == ""
	}
}

func listQuery(path, cursor string, limit int) string {
	kv := map[string]string{"cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery(path, kv)
}

func withQuery(path string, kv map[string]string) string {
	q := url.Values{}
	for k, v := range kv {
		if v == "" {
			continue
		}
		q.Set(k, v)
	}
	enc := q.Encode()
	if enc == "" {
		return path
	}
	if strings.Contains(path, "?") {
		return path + "&" + enc
	}
	return path + "?" + enc
}

func filePathURL(workspaceID, rel string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/files/" + escapeFileRel(rel)
}

func jobFilePathURL(name, rel string) string {
	return "/v1/jobs/" + url.PathEscape(name) + "/files/" + escapeFileRel(rel)
}

func skillFilePathURL(name, rel string) string {
	return "/v1/skills/" + url.PathEscape(name) + "/files/" + escapeFileRel(rel)
}

func escapeFileRel(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func workspaceQ(ws string) map[string]string {
	if ws == "" {
		return nil
	}
	return map[string]string{"workspace": ws}
}

// keepEventTriggers keeps event connectors from a triggers list; time triggers drop.
func keepEventTriggers(raw any) []any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var kept []any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		if ev, _ := m["event"].(string); strings.TrimSpace(ev) != "" {
			kept = append(kept, item)
		}
	}
	return kept
}

// timeTriggers parses a schedule value into cron/every trigger maps.
func timeTriggers(value string) ([]any, error) {
	var out []any
	for _, part := range splitScheduleValues(value) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "@every") {
			rest := strings.TrimSpace(strings.TrimPrefix(part, "@every"))
			if rest == "" {
				return nil, fmt.Errorf("invalid @every schedule %q", part)
			}
			out = append(out, map[string]any{"every": rest})
			continue
		}
		out = append(out, map[string]any{"cron": part})
	}
	return out, nil
}

func splitScheduleValues(value string) []string {
	// Multiple schedules use ", " (comma+space), matching trebi multi-cron examples.
	// Bare commas inside a field (e.g. DOW 1,3,5) stay intact.
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "@every") {
		return []string{value}
	}
	if !strings.Contains(value, ", ") {
		return []string{value}
	}
	parts := strings.Split(value, ", ")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitFrontmatter(md string) (fm, body string, err error) {
	if !strings.HasPrefix(md, "---") {
		return "", md, fmt.Errorf("no frontmatter")
	}
	rest := md[3:]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", "", fmt.Errorf("unclosed frontmatter")
	}
	fm = rest[:idx]
	after := rest[idx+len("\n---"):]
	if strings.HasPrefix(after, "\n") {
		after = after[1:]
	} else if strings.HasPrefix(after, "\r\n") {
		after = after[2:]
	}
	return fm, after, nil
}

func normalizeBridgeModeCLI(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "inherit", "link":
		return "inherit", true
	case "false", "off", "none":
		return "false", true
	default:
		return "", false
	}
}

// limitQ carries a page size. A non-positive limit means "server default".
// waitQ carries the long-poll deadline. The API caps it at 120 s.
func waitQ(waitMS int) map[string]string {
	if waitMS <= 0 {
		return nil
	}
	return map[string]string{"wait_ms": strconv.Itoa(waitMS)}
}

func limitQ(limit int) map[string]string {
	if limit <= 0 {
		return nil
	}
	return map[string]string{"limit": strconv.Itoa(limit)}
}

// confirmQ carries the destructive-route confirm gate. The API answers
// `validation` with `confirm_required` when it is absent.
func confirmQ(confirm bool) map[string]string {
	if !confirm {
		return nil
	}
	return map[string]string{"confirm": "1"}
}

// forceQ carries the force flag of memory PUT: the daemon skips validation.
func forceQ(force bool) map[string]string {
	if !force {
		return nil
	}
	return map[string]string{"force": "1"}
}

// mergeQ combines query maps; a later map wins on a repeated key.
func mergeQ(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// memoryPut builds PUT {base}; force skips server-side validation.
func memoryPut(base string, q map[string]string, content, ifMatch string, force bool) HTTPRequest {
	return HTTPRequest{
		Method:  "PUT",
		Path:    withQuery(base, mergeQ(q, forceQ(force))),
		Body:    map[string]any{"content": content},
		IfMatch: ifMatch,
	}
}

// memoryClear builds DELETE {base} behind the confirm query gate.
func memoryClear(base string, q map[string]string, ifMatch string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery(base, mergeQ(q, confirmQ(confirm))),
		IfMatch: ifMatch,
	}
}

// memoryValidate builds POST {base}/validate. Empty content validates the
// file on disk.
func memoryValidate(base string, q map[string]string, content string) HTTPRequest {
	var body map[string]any
	if content != "" {
		body = map[string]any{"content": content}
	}
	return HTTPRequest{Method: "POST", Path: withQuery(base+"/validate", q), Body: body}
}

// memoryMigrate builds POST {base}/migrate.
func memoryMigrate(base string, q map[string]string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery(base+"/migrate", q)}
}

// memorySectionPut builds PUT {base}/{section} (context or state).
func memorySectionPut(base string, q map[string]string, section, body string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   withQuery(base+"/"+section, q),
		Body:   map[string]any{"body": body},
	}
}

// memoryEntryAdd builds POST {base}/{section} (lessons or log).
func memoryEntryAdd(base string, q map[string]string, section, at, text string) HTTPRequest {
	body := map[string]any{"text": text}
	if at != "" {
		body["at"] = at
	}
	return HTTPRequest{Method: "POST", Path: withQuery(base+"/"+section, q), Body: body}
}

// memoryEntryDelete builds DELETE {base}/{section}/{entry_id}.
func memoryEntryDelete(base string, q map[string]string, section, entryID string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   withQuery(base+"/"+section+"/"+url.PathEscape(entryID), q),
	}
}

// fileListQ is the shared query of the three file list routes. recursive adds a
// row per directory, so a client sees the folder shape.
func fileListQ(prefix, afterPath, workspace string, limit int, recursive bool) map[string]string {
	kv := map[string]string{"prefix": prefix, "cursor": afterPath, "workspace": workspace}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	if recursive {
		kv["recursive"] = "1"
	}
	return kv
}

// OutboxQuery is the filter of GET /v1/outbox. Empty fields stay out.
type OutboxQuery struct {
	Kind, Status, Purpose, Workspace, Conversation, Inbox, Cursor string
	Limit                                                         int
}

// BuildOutboxListPath builds GET /v1/outbox with the filter of q.
func BuildOutboxListPath(q OutboxQuery) string {
	kv := map[string]string{
		"kind": q.Kind, "status": q.Status, "purpose": q.Purpose, "workspace": q.Workspace,
		"conversation": q.Conversation, "inbox": q.Inbox, "cursor": q.Cursor,
	}
	if q.Limit > 0 {
		kv["limit"] = strconv.Itoa(q.Limit)
	}
	return withQuery("/v1/outbox", kv)
}

// BuildOutboxGet builds GET /v1/outbox/{id}.
func BuildOutboxGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/outbox/" + url.PathEscape(id)}
}

// BuildOutboxRetry builds POST /v1/outbox/{id}/retry.
func BuildOutboxRetry(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/outbox/" + url.PathEscape(id) + "/retry"}
}

// BuildOutboxCancel builds POST /v1/outbox/{id}/cancel.
func BuildOutboxCancel(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/outbox/" + url.PathEscape(id) + "/cancel"}
}

// NotifyScope selects the layers of a notify route.
type NotifyScope struct{ Workspace, Agent, Job string }

// BuildNotifyRoutePath builds GET /v1/notify/route for sc.
func BuildNotifyRoutePath(sc NotifyScope) string {
	return withQuery("/v1/notify/route", map[string]string{"workspace": sc.Workspace, "agent": sc.Agent, "job": sc.Job})
}

// BuildNotifyTest builds POST /v1/notify/test. A nil step tests every step.
func BuildNotifyTest(sc NotifyScope, step *int) HTTPRequest {
	body := map[string]any{}
	for k, v := range map[string]string{"workspace": sc.Workspace, "agent": sc.Agent, "job": sc.Job} {
		if v != "" {
			body[k] = v
		}
	}
	if step != nil {
		body["step"] = *step
	}
	return HTTPRequest{Method: "POST", Path: "/v1/notify/test", Body: body}
}

func connectorOverridePath(workspace, ref string) string {
	return "/v1/workspaces/" + url.PathEscape(workspace) + "/connectors/" + url.PathEscape(ref)
}

// on renders the on: body of a job or agent create. Empty fields stay out.
func (t RuleTest) on(event string) map[string]any {
	on := t.body()
	on["event"] = event
	for _, k := range []string{"match", "extract"} {
		if on[k] == "" {
			delete(on, k)
		}
	}
	return on
}

func (t RuleTest) body() map[string]any {
	body := map[string]any{"match": t.Match, "extract": t.Extract}
	if len(t.Rooms) > 0 {
		body["rooms"] = t.Rooms
	}
	if len(t.Allowlist) > 0 {
		body["allowlist"] = t.Allowlist
	}
	if len(t.Contains) > 0 {
		body["contains"] = t.Contains
	}
	for k, v := range map[string]string{"from": t.From, "replies": t.Replies, "workspace": t.Workspace, "job": t.Job} {
		if v != "" {
			body[k] = v
		}
	}
	return body
}
