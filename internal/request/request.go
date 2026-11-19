// Package request holds the pure CLI/MCP → API request catalog.
// Every Build* function returns an HTTPRequest describing one /v1 call,
// testable without a daemon. No I/O, no trebi imports.
package request

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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
	Prompt         string         // full prompt override
	ExtraPrompt    string         // appended to the job body server-side
	Inputs         map[string]any // a string, a number, or a boolean per input
	Event          string         // a stored event id: a person starts the job with it
	RouteID        string         // with Event: the route to use
	Force          bool           // with Event: skip match
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
// A wake has no event.
func (r RunSpec) BuildAgentWake() HTTPRequest {
	r.Event, r.RouteID, r.Force = "", "", false
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
	if r.Event != "" {
		ev := map[string]any{"id": r.Event}
		if r.RouteID != "" {
			ev["route_id"] = r.RouteID
		}
		body["event"] = ev
	}
	if r.Force {
		body["force"] = true
	}
	return body
}

// JobSimulate is the POST /v1/jobs/simulate body. Set Job or Content.
type JobSimulate struct {
	Workspace string
	Job       string
	Content   string // draft JOB.md text
	RouteID   string
	EventIDs  []string
	Since     string
	Limit     int
	Inputs    map[string]any
	Preview   bool
}

// Build renders POST /v1/jobs/simulate.
func (j JobSimulate) Build() HTTPRequest {
	body := map[string]any{}
	for k, v := range map[string]string{"workspace": j.Workspace, "job": j.Job, "content": j.Content, "since": j.Since} {
		if v != "" {
			body[k] = v
		}
	}
	if j.RouteID != "" {
		body["route_id"] = j.RouteID
	}
	if len(j.EventIDs) > 0 {
		body["event_ids"] = j.EventIDs
	}
	if j.Limit > 0 {
		body["limit"] = j.Limit
	}
	if len(j.Inputs) > 0 {
		body["inputs"] = j.Inputs
	}
	if j.Preview {
		body["preview"] = true
	}
	return HTTPRequest{Method: "POST", Path: "/v1/jobs/simulate", Body: body}
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

// BuildRunMessage builds POST /v1/runs/{id}/message. Each attachment is an
// upload id or a workspace file path.
func BuildRunMessage(id, text string, attachments ...string) HTTPRequest {
	body := map[string]any{"text": text}
	if len(attachments) > 0 {
		body["attachments"] = attachments
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/runs/" + url.PathEscape(id) + "/message",
		Body:   body,
	}
}

// BuildUploadPut builds POST /v1/workspaces/{id}/uploads with the image bytes.
func BuildUploadPut(workspaceID string, content []byte) HTTPRequest {
	return HTTPRequest{
		Method:      "POST",
		Path:        "/v1/workspaces/" + url.PathEscape(workspaceID) + "/uploads",
		Raw:         content,
		ContentType: "application/octet-stream",
	}
}

// BuildUploadFromPath builds POST /v1/workspaces/{id}/uploads?path=, which
// stores one workspace file as an upload.
func BuildUploadFromPath(workspaceID, path string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/workspaces/"+url.PathEscape(workspaceID)+"/uploads", map[string]string{"path": path}),
	}
}

// BuildUploadGetPath builds GET /v1/workspaces/{id}/uploads/{uid}.
func BuildUploadGetPath(workspaceID, uploadID string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/uploads/" + url.PathEscape(uploadID)
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

// BuildRunChaptersPath builds GET /v1/runs/{id}/chapters.
func BuildRunChaptersPath(id string) string {
	return "/v1/runs/" + url.PathEscape(id) + "/chapters"
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

// BuildRunsActivity builds GET /v1/stats/activity for one range in a time
// zone. today is the local day in 15 minute bins, 7d is seven local days in
// 1 hour bins, and 30d is thirty local days in 4 hour bins. The window ends at
// the next local midnight. The instants are RFC3339 UTC.
func BuildRunsActivity(rng string, now time.Time, loc *time.Location, agent, workspace string) (HTTPRequest, error) {
	days, bin := 0, ""
	switch rng {
	case "", "7d":
		days, bin = 6, "1h"
	case "today":
		days, bin = 0, "15m"
	case "30d":
		days, bin = 29, "4h"
	default:
		return HTTPRequest{}, fmt.Errorf("range %q must be today, 7d, or 30d", rng)
	}
	local := now.In(loc)
	from := time.Date(local.Year(), local.Month(), local.Day()-days, 0, 0, 0, 0, loc)
	to := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
	return HTTPRequest{Method: "GET", Path: withQuery("/v1/stats/activity", map[string]string{
		"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339), "bin": bin,
		"group": "agent", "agent": agent, "workspace": workspace,
	})}, nil
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

// SkillRef names one skill. Scope and Owner pick the skill when a name is
// in more than one scope. Workspace finds an agent or job owner by name.
type SkillRef struct {
	Name      string
	Scope     string
	Owner     string
	Workspace string
}

// path builds /v1/skills/{name}{tail}?scope=&owner=&workspace= plus kv.
func (r SkillRef) path(tail string, kv map[string]string) string {
	q := map[string]string{"scope": r.Scope, "owner": r.Owner, "workspace": r.Workspace}
	for k, v := range kv {
		q[k] = v
	}
	return withQuery("/v1/skills/"+url.PathEscape(r.Name)+tail, q)
}

// BuildSkillGetPath builds GET /v1/skills/{name}.
func BuildSkillGetPath(r SkillRef) string {
	return r.path("", nil)
}

// SkillSpec is the POST /v1/skills body. Give SkillMD for a local skill, or
// SourceGit or SourceFolder to pull the bundle from a source. An empty
// Scope is global for a person and the own scope for a run.
type SkillSpec struct {
	Name         string
	SkillMD      string
	Provenance   string
	SourceGit    string
	SourceFolder string
	Scope        string
	Owner        string
	Workspace    string
	Note         string
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
	for k, v := range map[string]string{"scope": sp.Scope, "owner": sp.Owner, "workspace": sp.Workspace, "note": sp.Note} {
		if v != "" {
			body[k] = v
		}
	}
	return HTTPRequest{Method: "POST", Path: "/v1/skills", Body: body}
}

// BuildSkillRefresh builds POST /v1/skills/{name}/refresh, which pulls the
// skill source again.
func BuildSkillRefresh(r SkillRef) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: r.path("/refresh", nil)}
}

// BuildSkillDelete builds DELETE /v1/skills/{name}.
func BuildSkillDelete(r SkillRef, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: r.path("", confirmQ(confirm))}
}

// BuildSkillApprove builds POST /v1/skills/{name}/approve.
func BuildSkillApprove(r SkillRef) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: r.path("/approve", nil)}
}

// BuildSkillArchive builds POST /v1/skills/{name}/archive, or unarchive.
func BuildSkillArchive(r SkillRef, ifMatch string, archive bool) HTTPRequest {
	op := "/unarchive"
	if archive {
		op = "/archive"
	}
	return HTTPRequest{Method: "POST", Path: r.path(op, nil), IfMatch: ifMatch}
}

// SkillMove is the body of POST /v1/skills/{name}/move. Replace deletes the
// skill of the same name at the target; Rename gives the skill a new name.
type SkillMove struct {
	ToScope     string
	ToOwner     string
	ToWorkspace string
	Replace     bool
	Rename      string
}

// BuildSkillMove builds POST /v1/skills/{name}/move.
func BuildSkillMove(r SkillRef, m SkillMove) HTTPRequest {
	body := map[string]any{"to_scope": m.ToScope}
	for k, v := range map[string]string{"to_owner": m.ToOwner, "to_workspace": m.ToWorkspace, "rename": m.Rename} {
		if v != "" {
			body[k] = v
		}
	}
	if m.Replace {
		body["replace"] = true
	}
	return HTTPRequest{Method: "POST", Path: r.path("/move", nil), Body: body}
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
func BuildSkillPut(r SkillRef, skillMD string, harnesses map[string]string, ifMatch string) HTTPRequest {
	body := map[string]any{"skill_md": skillMD}
	if len(harnesses) > 0 {
		body["harnesses"] = harnesses
	}
	return HTTPRequest{
		Method:  "PUT",
		Path:    r.path("", nil),
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

// BuildProposalGet builds GET /v1/proposals/{id}.
func BuildProposalGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/proposals/" + url.PathEscape(id)}
}

// RequestCreateBody is the body of POST /v1/requests. ExpiresIn is a
// duration string such as 24h.
type RequestCreateBody struct {
	Title     string            `json:"title,omitempty"`
	Summary   string            `json:"summary,omitempty"`
	ExpiresIn string            `json:"expires_in,omitempty"`
	Forms     []RequestFormBody `json:"forms"`
}

// RequestFormBody is one form of a create body. Payload fields are flat.
type RequestFormBody struct {
	Type    string          `json:"type"`
	Title   string          `json:"title"`
	Summary string          `json:"summary,omitempty"`
	Target  string          `json:"target,omitempty"`
	Draft   any             `json:"draft,omitempty"`   // *RequestDraft or raw JSON
	Options any             `json:"options,omitempty"` // []RequestOption or raw JSON
	Multi   bool            `json:"multi,omitempty"`
	Schema  json.RawMessage `json:"schema,omitempty"`
	Allow   []string        `json:"allow,omitempty"`
	Report  string          `json:"report,omitempty"`
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

// RequestAnswerBody is the body of POST /v1/requests/{id}/answer: one
// answer for each form.
type RequestAnswerBody struct {
	Forms []RequestFormAnswer `json:"forms"`
	Via   string              `json:"via,omitempty"`
}

// RequestFormAnswer is the answer to the form at Idx.
type RequestFormAnswer struct {
	Idx      int             `json:"idx"`
	Decision string          `json:"decision"`
	Text     string          `json:"text,omitempty"`
	Message  string          `json:"message,omitempty"`
	Choices  []string        `json:"choices,omitempty"`
	Values   json.RawMessage `json:"values,omitempty"`
	Note     string          `json:"note,omitempty"`
}

// BuildRequestCreate builds POST /v1/requests. It needs a run token.
func BuildRequestCreate(b RequestCreateBody) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/requests", Body: b}
}

// BuildRequestGet builds GET /v1/requests/{id}.
func BuildRequestGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/requests/" + url.PathEscape(id)}
}

// BuildRequestListPath builds GET /v1/requests with its filters.
func BuildRequestListPath(status, run, job, workspace, cursor string, limit int) string {
	q := map[string]string{"status": status, "run": run, "job": job, "workspace": workspace, "cursor": cursor}
	if limit > 0 {
		q["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/requests", q)
}

// BuildRequestReport builds GET /v1/requests/{id}/forms/{idx}/report.
func BuildRequestReport(id string, idx int) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/requests/" + url.PathEscape(id) + "/forms/" + strconv.Itoa(idx) + "/report"}
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
// forWords filters by name, title, and description.
func BuildAgentListPath(workspaceID, forWords, archived string) string {
	return withQuery("/v1/workspaces/"+url.PathEscape(workspaceID)+"/agents", map[string]string{"for": forWords, "archived": archived})
}

// BuildAgentListAllPath builds GET /v1/agents across workspaces.
func BuildAgentListAllPath(workspaceFilter, forWords, archived string) string {
	return withQuery("/v1/agents", map[string]string{"workspace": workspaceFilter, "for": forWords, "archived": archived})
}

// BuildAgentArchive builds POST /v1/workspaces/{id}/agents/{name}/archive,
// or unarchive.
func BuildAgentArchive(workspaceID, name, ifMatch string, archive bool) HTTPRequest {
	op := "unarchive"
	if archive {
		op = "archive"
	}
	return HTTPRequest{Method: "POST", Path: BuildAgentGetPath(workspaceID, name) + "/" + op, IfMatch: ifMatch}
}

// BuildAgentGetPath builds GET /v1/workspaces/{id}/agents/{name}. name is the
// short name.
func BuildAgentGetPath(workspaceID, name string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/agents/" + url.PathEscape(name)
}

// BuildAgentRename builds POST /v1/workspaces/{id}/agents/{name}/rename.
// A dry run returns the reply without the move.
func BuildAgentRename(workspaceID, from, to string, dryRun bool) HTTPRequest {
	path := BuildAgentGetPath(workspaceID, from) + "/rename"
	if dryRun {
		path += "?dry_run=1"
	}
	return HTTPRequest{Method: "POST", Path: path, Body: map[string]string{"to": to}}
}

// BuildJobRename builds POST /v1/jobs/{name}/rename. to is the new last
// segment of the name.
func BuildJobRename(workspace, name, to string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name)+"/rename", workspaceQ(workspace)),
		Body:   map[string]string{"to": to},
	}
}

// JobMove is the body of a job move. Folder "" is no folder. A nil field
// keeps the current value. A job always moves to an agent.
type JobMove struct {
	Agent  *string `json:"agent,omitempty"`
	Folder *string `json:"folder,omitempty"`
	DryRun bool    `json:"dry_run,omitempty"`
}

// BuildJobMove builds POST /v1/jobs/{name}/move.
func BuildJobMove(workspace, name string, m JobMove) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name)+"/move", workspaceQ(workspace)),
		Body:   m,
	}
}

// BuildAgentFolderRename builds POST /v1/workspaces/{id}/agents/{name}/folders/rename.
// to "" takes the jobs out of the folder.
func BuildAgentFolderRename(workspaceID, agent, from, to string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   BuildAgentGetPath(workspaceID, agent) + "/folders/rename",
		Body:   map[string]string{"from": from, "to": to},
	}
}

// AgentAsk is the body of an ask or a hand-off.
type AgentAsk struct {
	Text           string `json:"text"`
	Context        string `json:"context,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	WaitMS         int    `json:"wait_ms,omitempty"`
}

// BuildAgentAsk builds POST /v1/workspaces/{id}/agents/{name}/ask.
func BuildAgentAsk(workspaceID, name string, b AgentAsk) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: BuildAgentGetPath(workspaceID, name) + "/ask", Body: b}
}

// BuildAgentHandoff builds POST /v1/workspaces/{id}/agents/{name}/handoff.
// A hand-off has no wait.
func BuildAgentHandoff(workspaceID, name string, b AgentAsk) HTTPRequest {
	b.WaitMS = 0
	return HTTPRequest{Method: "POST", Path: BuildAgentGetPath(workspaceID, name) + "/handoff", Body: b}
}

// ChainFilter is the query of GET /v1/chains.
type ChainFilter struct {
	Workspace, Status, Agent, Since, Cursor string
	Live                                    bool
	Limit                                   int
}

// BuildChainListPath builds GET /v1/chains.
func BuildChainListPath(f ChainFilter) string {
	q := map[string]string{"workspace": f.Workspace, "status": f.Status, "agent": f.Agent, "since": f.Since, "cursor": f.Cursor}
	if f.Live {
		q["live"] = "true"
	}
	if f.Limit > 0 {
		q["limit"] = strconv.Itoa(f.Limit)
	}
	return withQuery("/v1/chains", q)
}

// BuildChainGetPath builds GET /v1/chains/{trace_id}. A run ID also works.
func BuildChainGetPath(traceOrRunID string) string {
	return "/v1/chains/" + url.PathEscape(traceOrRunID)
}

// BuildHopGetPath builds GET /v1/hops/{id}.
func BuildHopGetPath(id string) string {
	return "/v1/hops/" + url.PathEscape(id)
}

// BuildChainStop builds POST /v1/chains/{trace_id}/stop.
func BuildChainStop(traceID, reason string, confirm bool) HTTPRequest {
	body := map[string]any{}
	if reason != "" {
		body["reason"] = reason
	}
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/chains/"+url.PathEscape(traceID)+"/stop", confirmQ(confirm)),
		Body:   body,
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

// AgentDelete holds the choices of an agent delete. An agent with jobs
// needs MoveJobsTo (another agent gets them) or Purge (they go too).
type AgentDelete struct {
	MoveJobsTo string
	Purge      bool
	Confirm    bool
	IfMatch    string
}

// BuildAgentDelete builds DELETE /v1/workspaces/{id}/agents/{name}. The
// whole agents/<name>/ folder goes; the call needs confirm.
func BuildAgentDelete(workspaceID, name string, d AgentDelete) HTTPRequest {
	kv := confirmQ(d.Confirm)
	if d.Purge {
		kv["purge"] = "1"
	}
	if d.MoveJobsTo != "" {
		kv["move_jobs_to"] = d.MoveJobsTo
	}
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery(BuildAgentGetPath(workspaceID, name), kv),
		IfMatch: d.IfMatch,
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

// BuildWorkspaceHandleCheckPath builds GET /v1/workspaces/handle-check.
// workspace is optional: it excludes that workspace from the "taken" test.
func BuildWorkspaceHandleCheckPath(handle, workspace string) string {
	q := url.Values{"handle": {handle}}
	if workspace != "" {
		q.Set("workspace", workspace)
	}
	return "/v1/workspaces/handle-check?" + q.Encode()
}

// BuildOnboardingGetPath builds GET /v1/onboarding.
func BuildOnboardingGetPath() string { return "/v1/onboarding" }

// BuildOnboardingPatch builds PATCH /v1/onboarding. fields holds step,
// completed, hide, unhide, first_job_id, and revision.
func BuildOnboardingPatch(fields map[string]any) HTTPRequest {
	return HTTPRequest{Method: "PATCH", Path: "/v1/onboarding", Body: fields}
}

// BuildOnboardingDefaults builds POST /v1/onboarding/defaults.
func BuildOnboardingDefaults() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/onboarding/defaults"}
}

// BuildOnboardingRestart builds POST /v1/onboarding/restart.
func BuildOnboardingRestart() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/onboarding/restart"}
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

// BuildEmailHeldPath builds GET /v1/email/held.
func BuildEmailHeldPath(workspace string) string {
	return withQuery("/v1/email/held", map[string]string{"workspace": workspace})
}

// BuildEmailRelease builds POST /v1/email/messages/{id}/release.
func BuildEmailRelease(id, scope string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery("/v1/email/messages/"+url.PathEscape(id)+"/release", confirmQ(confirm)),
		Body: map[string]any{"scope": scope}}
}

// BuildEmailIgnore builds POST /v1/email/messages/{id}/ignore.
func BuildEmailIgnore(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/email/messages/" + url.PathEscape(id) + "/ignore"}
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
	Artifact    *EmailArtifact
}

// EmailArtifact names the artifact version that a mail carries. Ref is an
// id, a name, or name@n.
type EmailArtifact struct {
	Ref     string `json:"ref"`
	Version int    `json:"version,omitempty"`
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
	body := map[string]any{"workspace": e.Workspace, "to": e.To}
	if e.Subject != "" {
		body["subject"] = e.Subject
	}
	if e.Artifact != nil {
		body["artifact"] = e.Artifact
	}
	if e.Text != "" {
		body["text"] = e.Text
	}
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

// BuildWorkspaceMoveToMachine builds POST /v1/workspaces/{id}/move with a
// machine: the move of the workspace to another machine.
func BuildWorkspaceMoveToMachine(id, machine string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/workspaces/"+url.PathEscape(id)+"/move", confirmQ(confirm)),
		Body:   map[string]any{"machine": machine},
	}
}

// BuildWorkspaceMoveGet builds GET /v1/workspaces/{id}/moves/{move}.
func BuildWorkspaceMoveGet(id, move string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/workspaces/" + url.PathEscape(id) + "/moves/" + url.PathEscape(move)}
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
func BuildSkillFileListPath(r SkillRef, prefix, afterPath string, limit int, recursive bool) string {
	return r.path("/files", fileListQ(prefix, afterPath, r.Workspace, limit, recursive))
}

// BuildSkillFileGetPath builds GET /v1/skills/{name}/files/{path}. stat adds ?stat=1.
func BuildSkillFileGetPath(r SkillRef, rel string, stat bool) string {
	if stat {
		return r.path("/files/"+escapeFileRel(rel), map[string]string{"stat": "1"})
	}
	return r.path("/files/"+escapeFileRel(rel), nil)
}

// BuildSkillFilePutPath builds PUT /v1/skills/{name}/files/{path}.
func BuildSkillFilePutPath(r SkillRef, rel string) string {
	return r.path("/files/"+escapeFileRel(rel), nil)
}

// BuildSkillFileDeletePath builds DELETE /v1/skills/{name}/files/{path}.
func BuildSkillFileDeletePath(r SkillRef, rel string) string {
	return r.path("/files/"+escapeFileRel(rel), nil)
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
func BuildSkillFilePut(r SkillRef, rel string, content []byte, contentType, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:      "PUT",
		Path:        BuildSkillFilePutPath(r, rel),
		Raw:         content,
		ContentType: contentType,
		IfMatch:     ifMatch,
	}
}

// BuildSkillFileDelete builds DELETE /v1/skills/{name}/files/{path} with optional If-Match.
func BuildSkillFileDelete(r SkillRef, rel, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    BuildSkillFileDeletePath(r, rel),
		IfMatch: ifMatch,
	}
}

// BuildFolderCreate builds POST /v1/workspaces/{id}/folders/{path}.
func BuildFolderCreate(workspaceID, rel string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/workspaces/" + url.PathEscape(workspaceID) + "/folders/" + escapeFileRel(rel)}
}

// BuildJobFolderCreate builds POST /v1/jobs/{name}/folders/{path}?workspace=.
func BuildJobFolderCreate(name, workspace, rel string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery("/v1/jobs/"+url.PathEscape(name)+"/folders/"+escapeFileRel(rel), workspaceQ(workspace))}
}

// BuildSkillFolderCreate builds POST /v1/skills/{name}/folders/{path}.
func BuildSkillFolderCreate(r SkillRef, rel string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: r.path("/folders/"+escapeFileRel(rel), nil)}
}

// BuildFolderDelete builds DELETE /v1/workspaces/{id}/folders/{path}.
func BuildFolderDelete(workspaceID, rel string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/workspaces/"+url.PathEscape(workspaceID)+"/folders/"+escapeFileRel(rel), confirmQ(confirm))}
}

// BuildJobFolderDelete builds DELETE /v1/jobs/{name}/folders/{path}?workspace=.
func BuildJobFolderDelete(name, workspace, rel string, confirm bool) HTTPRequest {
	q := map[string]string{"workspace": workspace, "confirm": confirmQ(confirm)["confirm"]}
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/jobs/"+url.PathEscape(name)+"/folders/"+escapeFileRel(rel), q)}
}

// BuildSkillFolderDelete builds DELETE /v1/skills/{name}/folders/{path}.
func BuildSkillFolderDelete(r SkillRef, rel string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: r.path("/folders/"+escapeFileRel(rel), confirmQ(confirm))}
}

// FileMove is the body of POST <owner>/files/move. IfMatch guards a file source.
type FileMove struct {
	From, To  string
	Overwrite bool
	IfMatch   string
}

func (m FileMove) request(path string) HTTPRequest {
	body := map[string]any{"from": m.From, "to": m.To}
	if m.Overwrite {
		body["overwrite"] = true
	}
	return HTTPRequest{Method: "POST", Path: path, Body: body, IfMatch: m.IfMatch}
}

// BuildFileMove builds POST /v1/workspaces/{id}/files/move.
func BuildFileMove(workspaceID string, m FileMove) HTTPRequest {
	return m.request("/v1/workspaces/" + url.PathEscape(workspaceID) + "/files/move")
}

// BuildJobFileMove builds POST /v1/jobs/{name}/files/move?workspace=.
func BuildJobFileMove(name, workspace string, m FileMove) HTTPRequest {
	return m.request(withQuery("/v1/jobs/"+url.PathEscape(name)+"/files/move", workspaceQ(workspace)))
}

// BuildSkillFileMove builds POST /v1/skills/{name}/files/move.
func BuildSkillFileMove(r SkillRef, m FileMove) HTTPRequest {
	return m.request(r.path("/files/move", nil))
}

// RuleTest is the trigger side of a rules test: the code and the scope.
// Replies is "continue" or "off". With Job, an event of an open conversation
// of that job is a follow-up. Batch is the batch window, such as 10m.
type RuleTest struct {
	Match     string
	Extract   string
	Rooms     []string
	Allowlist []string
	Contains  []string
	From      string
	Replies   string
	Batch     string
	Workspace string
	Job       string
	RouteID   string // with Job: the route that fills the inputs
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
func BuildRulesFire(connector, workspace, job, match, extract string, event any, routeID string) HTTPRequest {
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
			"route_id":  routeID,
		},
	}
}

// EventsQuery is the filter set of GET /v1/events. Cursor pages back from
// the newest event. After reads forward in id order; Follow keeps the
// response open as an NDJSON stream of new events.
type EventsQuery struct {
	Cursor    string
	After     string
	Follow    bool
	Connector string
	Workspace string
	Since     string
	Q         string
	Outcome   string // for example manual: events that a person can run a job with
	Accepts   string // <workspace>/<job>: events that a trigger of the job takes
	Limit     int
}

// Path renders the query as GET /v1/events.
func (e EventsQuery) Path() string {
	kv := map[string]string{
		"cursor": e.Cursor, "after": e.After, "connector": e.Connector,
		"workspace": e.Workspace, "since": e.Since, "q": e.Q,
		"outcome": e.Outcome, "accepts": e.Accepts,
	}
	if e.Follow {
		kv["follow"] = "1"
	}
	return withQuery("/v1/events", mergeQ(kv, limitQ(e.Limit)))
}

// BuildEventGet builds GET /v1/events/{id}.
func BuildEventGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/events/" + url.PathEscape(id)}
}

// BuildEventJobs builds GET /v1/events/{id}/jobs.
func BuildEventJobs(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/events/" + url.PathEscape(id) + "/jobs"}
}

// BuildEventsRetrigger builds POST /v1/events/{id}/retrigger.
func BuildEventsRetrigger(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/events/" + url.PathEscape(id) + "/retrigger",
	}
}

// EventReroute is the body of POST /v1/events/{id}/reroute. To is a route
// id, a target name, or nobody. Ignore applies only with To nobody.
type EventReroute struct {
	Workspace, To string
	Force, Ignore bool
}

// BuildEventReroute builds POST /v1/events/{id}/reroute.
func BuildEventReroute(id string, b EventReroute) HTTPRequest {
	body := map[string]any{"to": b.To}
	if b.Workspace != "" {
		body["workspace"] = b.Workspace
	}
	if b.Force {
		body["force"] = true
	}
	if b.Ignore {
		body["ignore"] = true
	}
	return HTTPRequest{Method: "POST", Path: "/v1/events/" + url.PathEscape(id) + "/reroute", Body: body}
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

// BuildNotificationsPath builds GET /v1/system/notifications.
func BuildNotificationsPath() string {
	return "/v1/system/notifications"
}

// BuildNotificationsRequest builds POST /v1/system/notifications/request.
func BuildNotificationsRequest() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/system/notifications/request"}
}

// BuildSystemOverviewPath builds GET /v1/system/overview. A set workspace
// scopes the inbox counts.
func BuildSystemOverviewPath(workspace string) string {
	return withQuery("/v1/system/overview", map[string]string{"workspace": workspace})
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

// BuildCloudLinkRequest builds POST /v1/cloud/link-request: link this
// machine by email. An empty name lets the daemon pick the machine name.
func BuildCloudLinkRequest(email, name string) HTTPRequest {
	body := map[string]any{"email": email}
	if name != "" {
		body["name"] = name
	}
	return HTTPRequest{Method: "POST", Path: "/v1/cloud/link-request", Body: body}
}

// BuildCloudLinkResend builds POST /v1/cloud/link-request/resend.
func BuildCloudLinkResend() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/cloud/link-request/resend"}
}

// BuildCloudLinkCancel builds DELETE /v1/cloud/link-request.
func BuildCloudLinkCancel() HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: "/v1/cloud/link-request"}
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

// MobilePair holds the optional pairing values. Zero means the default.
type MobilePair struct {
	Name string
	TTL  string // Go duration, for example 10m
	Uses int
}

// BuildMobilePair builds POST /v1/mobile/pairings.
func BuildMobilePair(p MobilePair) HTTPRequest {
	body := map[string]any{}
	if p.Name != "" {
		body["name"] = p.Name
	}
	if p.TTL != "" {
		body["ttl"] = p.TTL
	}
	if p.Uses != 0 {
		body["uses"] = p.Uses
	}
	return HTTPRequest{Method: "POST", Path: "/v1/mobile/pairings", Body: body}
}

// BuildMobilePairCancel builds DELETE /v1/mobile/pairings/{id}.
func BuildMobilePairCancel(id string) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: "/v1/mobile/pairings/" + url.PathEscape(id)}
}

// BuildMachinesPath builds GET /v1/machines.
func BuildMachinesPath() string { return "/v1/machines" }

// BuildMachineGet builds GET /v1/machines/{id}. The id "self" is this machine.
func BuildMachineGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/machines/" + url.PathEscape(id)}
}

// BuildMachineLink builds POST /v1/machines/{id}/link: connect to that
// machine with the pairing link that it made.
func BuildMachineLink(id, link string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/machines/" + url.PathEscape(id) + "/link", Body: map[string]any{"link": link}}
}

// BuildMachineUnlink builds DELETE /v1/machines/{id}/link.
func BuildMachineUnlink(id string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/machines/"+url.PathEscape(id)+"/link", confirmQ(confirm))}
}

// BuildMachineSessions builds GET /v1/machines/{id}/sessions: the agent
// sessions that started outside Trebi.
func BuildMachineSessions(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/machines/" + url.PathEscape(id) + "/sessions"}
}

// BuildMachineSession builds GET /v1/machines/{id}/sessions/{session}.
func BuildMachineSession(id, session string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/machines/" + url.PathEscape(id) + "/sessions/" + url.PathEscape(session)}
}

// BuildMobileClientsPath builds GET /v1/mobile/clients.
func BuildMobileClientsPath() string { return "/v1/mobile/clients" }

// BuildMobileUpdate builds PATCH /v1/mobile/clients/{id}. A nil field stays.
func BuildMobileUpdate(id string, name, scope *string) HTTPRequest {
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if scope != nil {
		body["scope"] = *scope
	}
	return HTTPRequest{Method: "PATCH", Path: "/v1/mobile/clients/" + url.PathEscape(id), Body: body}
}

// BuildMobileRemove builds DELETE /v1/mobile/clients/{id}.
func BuildMobileRemove(id string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE",
		Path: withQuery("/v1/mobile/clients/"+url.PathEscape(id), confirmQ(confirm))}
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

// SkillList filters GET /v1/skills. EffectiveFor names a job or agent and
// returns the skills its runs get, narrow first.
type SkillList struct {
	Scope        string
	Owner        string
	Workspace    string
	EffectiveFor string
	// Archived is false, true, or all. Empty is false.
	Archived string
	Limit    int
}

// Path builds GET /v1/skills?scope=&owner=&workspace=&effective_for=&archived=&limit=.
func (l SkillList) Path() string {
	return withQuery(listQuery("/v1/skills", "", l.Limit), map[string]string{
		"scope": l.Scope, "owner": l.Owner, "workspace": l.Workspace, "effective_for": l.EffectiveFor, "archived": l.Archived,
	})
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

// InboxQuery is the filter of GET /v1/inbox. Empty fields stay out.
type InboxQuery struct {
	View, Kind, Workspace, Query, Cursor string
	AlertKind, Connector                 string
	Unread                               bool
	Limit                                int
}

// BuildInboxListPath builds GET /v1/inbox with the filter of q.
func BuildInboxListPath(q InboxQuery) string {
	kv := map[string]string{"view": q.View, "kind": q.Kind, "workspace": q.Workspace, "q": q.Query, "cursor": q.Cursor,
		"alert_kind": q.AlertKind, "connector": q.Connector}
	if q.Unread {
		kv["unread"] = "1"
	}
	if q.Limit > 0 {
		kv["limit"] = strconv.Itoa(q.Limit)
	}
	return withQuery("/v1/inbox", kv)
}

// BuildInboxGet builds GET /v1/inbox/{id}.
func BuildInboxGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/inbox/" + url.PathEscape(id)}
}

// InboxApply is the body of POST /v1/inbox/apply. Set IDs or Filter.
type InboxApply struct {
	Op      string         `json:"op"`
	IDs     []string       `json:"ids,omitempty"`
	Filter  map[string]any `json:"filter"` // an empty map selects all; nil sends null
	Confirm bool           `json:"confirm,omitempty"`
}

// BuildInboxApply builds POST /v1/inbox/apply.
func BuildInboxApply(b InboxApply) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/inbox/apply", Body: b}
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
	WaitMS         int      // long-poll for the assistant reply
	Attachments    []string // upload ids or workspace file paths
}

// Build renders the ask as POST /v1/workspaces/{id}/assistant/messages.
func (a AskSpec) Build() HTTPRequest {
	body := map[string]any{"text": a.Text}
	if len(a.Attachments) > 0 {
		body["attachments"] = a.Attachments
	}
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
	Owner        string
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
		"owner":        r.Owner,
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
func BuildJobsListPath(workspace, q, harness, active, archived string) string {
	return withQuery("/v1/jobs", mergeQ(workspaceQ(workspace),
		map[string]string{"q": q, "harness": harness, "active": active, "archived": archived}))
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
	Folder         string // group label under the owner agent
	Cron           string
	Every          string
	IdempotencyKey string
	Agent          string // agent name; the file lands under agents/<agent>/
	Kind           string // validate only: job (default) or agent
	// Inputs is the inputs block: a list of input entries in order.
	Inputs []any
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
		"description": j.Description, "folder": j.Folder,
		"cron": j.Cron, "every": j.Every, "idempotency_key": j.IdempotencyKey,
		"agent": j.Agent, "kind": j.Kind,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if len(j.Inputs) > 0 {
		body["inputs"] = j.Inputs
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

// BuildJobArchive builds POST /v1/jobs/{name}/archive, or unarchive.
func BuildJobArchive(name, workspace, ifMatch string, archive bool) HTTPRequest {
	op := "unarchive"
	if archive {
		op = "archive"
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
// value is cron, comma-separated crons, or @every 30m.
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

	kept, err := timeTriggers(value)
	if err != nil {
		return "", err
	}
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

// BuildHarnessUsage builds GET /v1/harnesses/usage.
func BuildHarnessUsage() HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/harnesses/usage"}
}

// BuildHarnessUsageRefresh builds POST /v1/harnesses/usage/refresh.
func BuildHarnessUsageRefresh() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/harnesses/usage/refresh"}
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

// BuildHarnessHooks builds GET /v1/harnesses/{name}/hooks, or PUT with on
// set.
func BuildHarnessHooks(name string, on *bool) HTTPRequest {
	path := "/v1/harnesses/" + url.PathEscape(name) + "/hooks"
	if on == nil {
		return HTTPRequest{Method: "GET", Path: path}
	}
	return HTTPRequest{Method: "PUT", Path: path, Body: map[string]any{"on": *on}}
}

// BuildHarnessModels builds GET /v1/harnesses/{name}/models.
func BuildHarnessModels(name string) HTTPRequest {
	return HTTPRequest{
		Method: "GET",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/models",
	}
}

// BuildHarnessInstall builds POST /v1/harnesses/{name}/install. An empty
// version installs the newest one.
func BuildHarnessInstall(name, version string) HTTPRequest {
	req := HTTPRequest{Method: "POST", Path: "/v1/harnesses/" + url.PathEscape(name) + "/install"}
	if version != "" {
		req.Body = map[string]any{"version": version}
	}
	return req
}

// BuildHarnessUpdate builds POST /v1/harnesses/{name}/update.
func BuildHarnessUpdate(name string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/harnesses/" + url.PathEscape(name) + "/update"}
}

// BuildHarnessVersionsCheck builds POST /v1/harnesses/{name}/versions/check.
func BuildHarnessVersionsCheck(name string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/harnesses/" + url.PathEscape(name) + "/versions/check"}
}

// BuildHarnessPatch builds PATCH /v1/harnesses/{name}. body holds any of
// enabled, auth_mode, model, effort, updates, version, compat, label, env,
// and pause_at.
func BuildHarnessPatch(name string, body map[string]any) HTTPRequest {
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/harnesses/" + url.PathEscape(name),
		Body:   body,
	}
}

// BuildHarnessCreate builds POST /v1/harnesses. body holds name, base, and
// any of label, auth_mode, model, effort, and env.
func BuildHarnessCreate(body map[string]any) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/harnesses", Body: body}
}

// BuildHarnessDelete builds DELETE /v1/harnesses/{name}. moveTo names the
// harness that gets the jobs of the removed one.
func BuildHarnessDelete(name, moveTo string) HTTPRequest {
	path := "/v1/harnesses/" + url.PathEscape(name)
	if moveTo != "" {
		path += "?move_to=" + url.QueryEscape(moveTo)
	}
	return HTTPRequest{Method: "DELETE", Path: path}
}

func harnessFilePath(name, key string) string {
	return "/v1/harnesses/" + url.PathEscape(name) + "/files/" + url.PathEscape(key)
}

// BuildHarnessFiles builds GET /v1/harnesses/{name}/files.
func BuildHarnessFiles(name string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/harnesses/" + url.PathEscape(name) + "/files"}
}

// BuildHarnessFileRead builds GET /v1/harnesses/{name}/files/{key}. side is
// user, trebi, or empty (the side that a run uses).
func BuildHarnessFileRead(name, key, side string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: withQuery(harnessFilePath(name, key), map[string]string{"side": side})}
}

// BuildHarnessFileOwn builds POST /v1/harnesses/{name}/files/{key}/own.
func BuildHarnessFileOwn(name, key, start string) HTTPRequest {
	body := map[string]any{}
	if start != "" {
		body["start"] = start
	}
	return HTTPRequest{Method: "POST", Path: harnessFilePath(name, key) + "/own", Body: body}
}

// BuildHarnessFileWrite builds PUT /v1/harnesses/{name}/files/{key}.
func BuildHarnessFileWrite(name, key, content, ifMatch string) HTTPRequest {
	return HTTPRequest{Method: "PUT", Path: harnessFilePath(name, key), Body: map[string]any{"content": content}, IfMatch: ifMatch}
}

// BuildHarnessFileReset builds DELETE /v1/harnesses/{name}/files/{key}/own.
func BuildHarnessFileReset(name, key string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery(harnessFilePath(name, key)+"/own", confirmQ(confirm))}
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
// provider is for OpenCode only.
func BuildHarnessLoginStart(name, mode, provider string) HTTPRequest {
	body := map[string]any{}
	if mode != "" {
		body["mode"] = mode
	}
	if provider != "" {
		body["provider"] = provider
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/login",
		Body:   body,
	}
}

// BuildHarnessLoginToken builds POST /v1/harnesses/{name}/login with an API
// key. provider is for OpenCode only.
func BuildHarnessLoginToken(name, token, provider string) HTTPRequest {
	body := map[string]any{"token": token}
	if provider != "" {
		body["provider"] = provider
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/login",
		Body:   body,
	}
}

// BuildHarnessProviders builds GET /v1/harnesses/{name}/providers.
func BuildHarnessProviders(name string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/harnesses/" + url.PathEscape(name) + "/providers"}
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
	Scope                       []string       // nil leaves the scope; ["global"] or workspace names or ids
	Receive                     map[string]any // nil leaves the events to receive; {mode, types, other}
}

// ConnectorOAuthSet is the PATCH /v1/connectors/{ref}/oauth body. A nil
// field stays. ClientSecretName names a stored secret that holds the
// secret of the own app.
type ConnectorOAuthSet struct {
	Ref                                  ConnectorRef
	Mode, Return, Registration, ClientID *string
	Scopes                               []string // nil leaves the scopes
	ClientSecret, ClientSecretName       string
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
	if i.Scope != nil {
		body["scope"] = i.Scope
	}
	if i.Receive != nil {
		body["receive"] = i.Receive
	}
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/connectors/" + url.PathEscape(i.Ref),
		Body:   body,
	}
}

// Build renders the change as PATCH /v1/connectors/{ref}/oauth.
func (i ConnectorOAuthSet) Build() HTTPRequest {
	body := map[string]any{}
	for k, v := range map[string]*string{"mode": i.Mode, "return": i.Return, "registration": i.Registration, "client_id": i.ClientID} {
		if v != nil {
			body[k] = *v
		}
	}
	if i.Scopes != nil {
		body["scopes"] = i.Scopes
	}
	if i.ClientSecret != "" {
		body["client_secret"] = i.ClientSecret
	}
	if i.ClientSecretName != "" {
		body["client_secret_name"] = i.ClientSecretName
	}
	return HTTPRequest{Method: "PATCH", Path: i.Ref.path("/oauth", nil), Body: body}
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

// RoutingPatch is the body of PATCH .../connectors/{ref}/routing. A nil
// field leaves its value alone.
type RoutingPatch struct {
	Mode, Notes, Unsure, NoFit *string
	SureAt                     *float64
	Routes                     []map[string]any // merge by id; no id adds a route
	RemoveRoutes               []string
	Rules                      *[]map[string]any // replaces the list
}

// BuildRoutingListPath builds GET /v1/workspaces/{id}/routing.
func BuildRoutingListPath(workspace string) string {
	return "/v1/workspaces/" + url.PathEscape(workspace) + "/routing"
}

// BuildRoutingPath builds the path of GET and PATCH
// /v1/workspaces/{id}/connectors/{ref}/routing.
func BuildRoutingPath(workspace, connector string) string {
	return connectorOverridePath(workspace, connector) + "/routing"
}

// BuildRoutingGet builds GET /v1/workspaces/{id}/connectors/{ref}/routing.
func BuildRoutingGet(workspace, connector string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: BuildRoutingPath(workspace, connector)}
}

// BuildRoutingSet builds PATCH /v1/workspaces/{id}/connectors/{ref}/routing.
// The daemon checks the values. A stale ifMatch gives a 412 answer.
func BuildRoutingSet(workspace, connector string, p RoutingPatch, ifMatch string) HTTPRequest {
	return HTTPRequest{Method: "PATCH", Path: BuildRoutingPath(workspace, connector), Body: p.body(), IfMatch: ifMatch}
}

// ConnectorRouteTest is the body of POST .../connectors/{ref}/routing/test.
// It names stored events or gives a text. Routing is a draft that replaces
// the stored file for this test only; build it with BuildRoutingPatch.
type ConnectorRouteTest struct {
	EventIDs                  []string
	Text, From, Subject, Room string
	Routing                   map[string]any
}

// BuildConnectorRouteTest builds POST /v1/workspaces/{id}/connectors/{ref}/routing/test.
func BuildConnectorRouteTest(workspace, connector string, t ConnectorRouteTest) HTTPRequest {
	body := map[string]any{}
	if len(t.EventIDs) > 0 {
		body["event_ids"] = t.EventIDs
	}
	for k, v := range map[string]string{"text": t.Text, "from": t.From, "subject": t.Subject, "room": t.Room} {
		if v != "" {
			body[k] = v
		}
	}
	if t.Routing != nil {
		body["routing"] = t.Routing
	}
	return HTTPRequest{Method: "POST", Path: BuildRoutingPath(workspace, connector) + "/test", Body: body}
}

// BuildRoutingPatch builds the body of a routing patch. It refuses a value
// outside the fixed lists, so a caller can fail before the request.
func BuildRoutingPatch(p RoutingPatch) (map[string]any, error) {
	for _, c := range []struct {
		key     string
		v       *string
		allowed []string
	}{
		{"mode", p.Mode, []string{"on", "off"}},
		{"unsure", p.Unsure, []string{"ask", "guess"}},
		{"no_fit", p.NoFit, []string{"drop", "ask"}},
	} {
		if c.v != nil && !slices.Contains(c.allowed, *c.v) {
			return nil, fmt.Errorf("%s %q: use %s", c.key, *c.v, strings.Join(c.allowed, " or "))
		}
	}
	if p.SureAt != nil && (*p.SureAt < 0 || *p.SureAt > 1) {
		return nil, fmt.Errorf("sure_at %v: use a number from 0 to 1", *p.SureAt)
	}
	return p.body(), nil
}

func (p RoutingPatch) body() map[string]any {
	body := map[string]any{}
	for key, v := range map[string]*string{"mode": p.Mode, "notes": p.Notes, "unsure": p.Unsure, "no_fit": p.NoFit} {
		if v != nil {
			body[key] = *v
		}
	}
	if p.SureAt != nil {
		body["sure_at"] = *p.SureAt
	}
	if len(p.Routes) > 0 {
		body["routes"] = p.Routes
	}
	if len(p.RemoveRoutes) > 0 {
		body["remove_routes"] = p.RemoveRoutes
	}
	if p.Rules != nil {
		body["rules"] = *p.Rules
	}
	return body
}

// BuildConnectorLoginMethods builds GET /v1/connectors/{ref}/login/methods.
func BuildConnectorLoginMethods(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: c.path("/login/methods", nil)}
}

// BuildConnectorLogin builds POST /v1/connectors/{ref}/login. An empty
// method picks the first available one.
func BuildConnectorLogin(c ConnectorRef, method string) HTTPRequest {
	body := map[string]any{}
	if method != "" {
		body["method"] = method
	}
	return HTTPRequest{Method: "POST", Path: c.path("/login", nil), Body: body}
}

// BuildConnectorLoginStreamPath builds GET /v1/connectors/{ref}/login/{flow_id}.
func BuildConnectorLoginStreamPath(ref, flowID string) string {
	return "/v1/connectors/" + url.PathEscape(ref) + "/login/" + url.PathEscape(flowID)
}

// BuildConnectorLoginSubmit builds POST /v1/connectors/{ref}/login/{flow_id}.
// A value is a string, or a list of strings for a field with multiple.
func BuildConnectorLoginSubmit(name, flowID string, fields map[string]any) HTTPRequest {
	if fields == nil {
		fields = map[string]any{}
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

// BuildConnectorLoginStatus builds GET /v1/connectors/{ref}/login/{flow_id}
// as JSON. With wait, the daemon waits up to that many seconds for the end
// of the login.
func BuildConnectorLoginStatus(name, flowID string, wait int) HTTPRequest {
	p := BuildConnectorLoginStreamPath(name, flowID)
	if wait > 0 {
		p += "?wait=" + strconv.Itoa(wait)
	}
	return HTTPRequest{Method: "GET", Path: p}
}

// BuildConnectorOAuthGet builds GET /v1/connectors/{ref}/oauth.
func BuildConnectorOAuthGet(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: c.path("/oauth", nil)}
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

// BuildConnectorSubscriptionsPath builds GET /v1/connectors/{ref}/subscriptions.
func BuildConnectorSubscriptionsPath(c ConnectorRef) string {
	return c.path("/subscriptions", nil)
}

// BuildConnectorSubscriptionPath builds /v1/connectors/{ref}/subscriptions/{id}.
func BuildConnectorSubscriptionPath(c ConnectorRef, id string) string {
	return c.path("/subscriptions/"+url.PathEscape(id), nil)
}

// subscriptionBody holds the present values and types. An empty, non-nil
// types list clears the types.
func subscriptionBody(values map[string]any, types []string) map[string]any {
	body := map[string]any{}
	if values != nil {
		body["values"] = values
	}
	if types != nil {
		body["types"] = types
	}
	return body
}

// BuildConnectorSubscribe builds POST /v1/connectors/{ref}/subscriptions.
func BuildConnectorSubscribe(c ConnectorRef, values map[string]any, types []string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: BuildConnectorSubscriptionsPath(c), Body: subscriptionBody(values, types)}
}

// BuildConnectorSubscriptionSet builds PATCH /v1/connectors/{ref}/subscriptions/{id}.
func BuildConnectorSubscriptionSet(c ConnectorRef, id string, values map[string]any, types []string) HTTPRequest {
	return HTTPRequest{Method: "PATCH", Path: BuildConnectorSubscriptionPath(c, id), Body: subscriptionBody(values, types)}
}

// BuildConnectorUnsubscribe builds DELETE /v1/connectors/{ref}/subscriptions/{id}.
func BuildConnectorUnsubscribe(c ConnectorRef, id string) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: BuildConnectorSubscriptionPath(c, id)}
}

// BuildConnectorSubscriptionSubmit builds POST /v1/connectors/{ref}/subscriptions/{id}/submit.
func BuildConnectorSubscriptionSubmit(c ConnectorRef, id string, fields map[string]string) HTTPRequest {
	if fields == nil {
		fields = map[string]string{}
	}
	return HTTPRequest{Method: "POST", Path: BuildConnectorSubscriptionPath(c, id) + "/submit", Body: map[string]any{"fields": fields}}
}

// BuildConnectorSubscriptionsSync builds POST /v1/connectors/{ref}/subscriptions/sync.
func BuildConnectorSubscriptionsSync(c ConnectorRef) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: c.path("/subscriptions/sync", nil), Body: map[string]any{}}
}

// BuildConnectorSubscriptionOptions builds POST /v1/connectors/{ref}/subscriptions/options.
func BuildConnectorSubscriptionOptions(c ConnectorRef, field, q string, values map[string]any, cursor string, limit int) HTTPRequest {
	body := map[string]any{"field": field}
	if q != "" {
		body["q"] = q
	}
	if values != nil {
		body["values"] = values
	}
	if cursor != "" {
		body["cursor"] = cursor
	}
	if limit > 0 {
		body["limit"] = limit
	}
	return HTTPRequest{Method: "POST", Path: c.path("/subscriptions/options", nil), Body: body}
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
// The operator is "assistant" or an agent name; the workspace is required.
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
// catalog entry only. Slug and Title name the new connection. Source
// installs from a GitHub link in place of a registry entry.
type ConnectorInstall struct {
	Registry, Name, Version, Track, Slug, Title string
	Package, Remote                             int
	Inputs                                      map[string]string
	Source                                      *ConnectorSource
	Force                                       bool
	Scope                                       []string // empty: the default scope
	Workspace                                   string   // the workspace of the caller; the default scope of an account
	Receive                                     map[string]any
}

// ConnectorSource is the link of an install. CheckID reuses a check of it.
type ConnectorSource struct {
	URL     string `json:"url"`
	CheckID string `json:"check_id,omitempty"`
}

// Build returns the install request.
func (i ConnectorInstall) Build() HTTPRequest {
	body := map[string]any{"registry": i.Registry, "name": i.Name}
	if i.Source != nil {
		body = map[string]any{"source": i.Source}
	}
	if i.Force {
		body["force"] = true
	}
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
	if len(i.Scope) > 0 {
		body["scope"] = i.Scope
	}
	if i.Workspace != "" {
		body["workspace"] = i.Workspace
	}
	if i.Receive != nil {
		body["receive"] = i.Receive
	}
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/install", Body: body}
}

// BuildConnectorSourceCheck builds POST /v1/connectors/sources/check.
func BuildConnectorSourceCheck(link string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/sources/check", Body: map[string]any{"url": link}}
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

// ConnectorPackageSource is the source of a package install: URL (and
// CheckID) for a link, or Catalog (and Version) for a catalog type.
type ConnectorPackageSource struct {
	URL     string `json:"url,omitempty"`
	CheckID string `json:"check_id,omitempty"`
	Catalog string `json:"catalog,omitempty"`
	Version string `json:"version,omitempty"`
}

// BuildConnectorPackageInstall builds POST /v1/connectors/packages.
func BuildConnectorPackageInstall(src ConnectorPackageSource) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/packages", Body: map[string]any{"source": src}}
}

// BuildConnectorPackages builds GET /v1/connectors/packages.
func BuildConnectorPackages() HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/connectors/packages"}
}

// BuildConnectorPackagesKeep builds PUT /v1/connectors/packages/keep.
func BuildConnectorPackagesKeep(holder string, packages []string) HTTPRequest {
	if packages == nil {
		packages = []string{}
	}
	return HTTPRequest{Method: "PUT", Path: "/v1/connectors/packages/keep", Body: map[string]any{"holder": holder, "packages": packages}}
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

// StateQuery is the body of a state query. It matches state.Query.
type StateQuery struct {
	Keys   []string `json:"keys,omitempty"`
	Prefix string   `json:"prefix,omitempty"`
	Q      string   `json:"q,omitempty"`
	Where  string   `json:"where,omitempty"`
	Since  string   `json:"since,omitempty"`
	Until  string   `json:"until,omitempty"`
	By     string   `json:"by,omitempty"`
	Sort   string   `json:"sort,omitempty"`
	Fields []string `json:"fields,omitempty"`
	Limit  int      `json:"limit,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
}

// StatePath is /v1/state/{owner}[/{collection}][/{verb}]. owner is an ID,
// "self", or a name; a name needs workspace.
func StatePath(owner, collection, verb string) string {
	p := "/v1/state/" + url.PathEscape(owner)
	if collection != "" {
		p += "/" + url.PathEscape(collection)
	}
	if verb != "" {
		p += "/" + verb
	}
	return p
}

// BuildStateSummary builds GET /v1/state/{owner}.
func BuildStateSummary(owner, workspace string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: withQuery(StatePath(owner, "", ""), workspaceQ(workspace))}
}

// BuildStateQuery builds POST /v1/state/{owner}/{collection}/query.
func BuildStateQuery(owner, workspace, collection string, q StateQuery) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery(StatePath(owner, collection, "query"), workspaceQ(workspace)), Body: q}
}

// BuildStateGet builds POST /v1/state/{owner}/{collection}/get.
func BuildStateGet(owner, workspace, collection string, keys []string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery(StatePath(owner, collection, "get"), workspaceQ(workspace)),
		Body: map[string]any{"keys": keys}}
}

// BuildStateHas builds POST /v1/state/{owner}/{collection}/has.
func BuildStateHas(owner, workspace, collection string, keys []string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery(StatePath(owner, collection, "has"), workspaceQ(workspace)),
		Body: map[string]any{"keys": keys}}
}

// BuildStatePut builds POST /v1/state/{owner}/{collection}/rows. Each row is
// {key, at, value, inc}. An empty mode is upsert.
func BuildStatePut(owner, workspace, collection string, rows []any, mode string) HTTPRequest {
	body := map[string]any{"rows": rows}
	if mode != "" {
		body["mode"] = mode
	}
	return HTTPRequest{Method: "POST", Path: withQuery(StatePath(owner, collection, "rows"), workspaceQ(workspace)), Body: body}
}

// BuildStateDelete builds POST /v1/state/{owner}/{collection}/delete. A
// caller that is not a run needs confirm.
func BuildStateDelete(owner, workspace, collection string, keys []string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: withQuery(StatePath(owner, collection, "delete"), mergeQ(workspaceQ(workspace), confirmQ(confirm))),
		Body: map[string]any{"keys": keys}}
}

// BuildStatePrune builds POST /v1/state/{owner}/{collection}/prune. Since or
// until must be set. A caller that is not a run needs confirm.
func BuildStatePrune(owner, workspace, collection, since, until, by string, confirm bool) HTTPRequest {
	body := map[string]any{}
	for k, v := range map[string]string{"since": since, "until": until, "by": by} {
		if v != "" {
			body[k] = v
		}
	}
	return HTTPRequest{Method: "POST", Path: withQuery(StatePath(owner, collection, "prune"), mergeQ(workspaceQ(workspace), confirmQ(confirm))), Body: body}
}

// BuildStateDrop builds DELETE /v1/state/{owner}/{collection}. A caller that
// is not a run needs confirm.
func BuildStateDrop(owner, workspace, collection string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery(StatePath(owner, collection, ""), mergeQ(workspaceQ(workspace), confirmQ(confirm)))}
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

// ArtifactFilter is the query of GET /v1/artifacts. Owner is assistant,
// job:<name>, or agent:<name>.
type ArtifactFilter struct {
	Workspace, Owner, Kind, Query, Cursor, Conversation string
	Live                                                *bool
	Limit                                               int
}

// BuildArtifactListPath builds GET /v1/artifacts.
func BuildArtifactListPath(f ArtifactFilter) string {
	q := map[string]string{"workspace": f.Workspace, "owner": f.Owner, "kind": f.Kind, "q": f.Query, "cursor": f.Cursor,
		"conversation": f.Conversation}
	if f.Live != nil {
		q["live"] = strconv.FormatBool(*f.Live)
	}
	if f.Limit > 0 {
		q["limit"] = strconv.Itoa(f.Limit)
	}
	return withQuery("/v1/artifacts", q)
}

// SplitArtifactRef splits name@n into the name and the version. The version
// is "latest" when ref has none.
func SplitArtifactRef(ref string) (name, version string) {
	if at := strings.LastIndex(ref, "@"); at > 0 {
		return ref[:at], ref[at+1:]
	}
	return ref, "latest"
}

// ArtifactRef names one artifact: an id, or a name in Workspace.
type ArtifactRef struct {
	Ref       string
	Workspace string
}

// GetPath builds GET /v1/artifacts/{id}.
func (a ArtifactRef) GetPath() string { return a.path("", nil) }

// VersionsPath builds GET /v1/artifacts/{id}/versions.
func (a ArtifactRef) VersionsPath() string { return a.path("/versions", nil) }

// VersionPath builds GET /v1/artifacts/{id}/versions/{n}; n is a number or latest.
func (a ArtifactRef) VersionPath(n string) string { return a.path("/versions/"+url.PathEscape(n), nil) }

// ContentOpts are the query of the content route. A live version binds
// Snapshot (default the newest); Raw gives the template.
type ContentOpts struct {
	Download bool
	Raw      bool
	Snapshot string
}

// ContentPath builds GET /v1/artifacts/{id}/versions/{n}/content.
func (a ArtifactRef) ContentPath(n string, o ContentOpts) string {
	q := map[string]string{"snapshot": o.Snapshot}
	if o.Download {
		q["download"] = "1"
	}
	if o.Raw {
		q["raw"] = "1"
	}
	return a.path("/versions/"+url.PathEscape(n)+"/content", q)
}

// Decide builds POST /v1/artifacts/{id}/versions/{n}/decide. decision is
// approve or deny.
func (a ArtifactRef) Decide(n, decision string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: a.path("/versions/"+url.PathEscape(n)+"/decide", nil),
		Body: map[string]any{"decision": decision}}
}

// Refresh builds POST /v1/artifacts/{id}/refresh.
func (a ArtifactRef) Refresh(inputs map[string]string) HTTPRequest {
	body := map[string]any{}
	if len(inputs) > 0 {
		body["inputs"] = inputs
	}
	return HTTPRequest{Method: "POST", Path: a.path("/refresh", nil), Body: body}
}

// SnapshotsPath builds GET /v1/artifacts/{id}/snapshots.
func (a ArtifactRef) SnapshotsPath(limit int) string {
	q := map[string]string{}
	if limit > 0 {
		q["limit"] = strconv.Itoa(limit)
	}
	return a.path("/snapshots", q)
}

// SnapshotPath builds GET /v1/artifacts/{id}/snapshots/{sid}, or its /data
// route.
func (a ArtifactRef) SnapshotPath(sid string, data bool) string {
	tail := "/snapshots/" + url.PathEscape(sid)
	if data {
		tail += "/data"
	}
	return a.path(tail, nil)
}

// ArtifactSchedule is the body of PUT /v1/artifacts/{id}/schedule.
type ArtifactSchedule struct {
	Schedule string
	Timezone string
	EmailTo  []string
	Subject  string
}

// Schedule builds PUT /v1/artifacts/{id}/schedule, or DELETE for an empty
// Schedule.
func (a ArtifactRef) Schedule(s ArtifactSchedule) HTTPRequest {
	if s.Schedule == "" {
		return HTTPRequest{Method: "DELETE", Path: a.path("/schedule", nil)}
	}
	body := map[string]any{"schedule": s.Schedule}
	if s.Timezone != "" {
		body["timezone"] = s.Timezone
	}
	if len(s.EmailTo) > 0 {
		email := map[string]any{"to": s.EmailTo}
		if s.Subject != "" {
			email["subject"] = s.Subject
		}
		body["deliver"] = map[string]any{"email": email}
	}
	return HTTPRequest{Method: "PUT", Path: a.path("/schedule", nil), Body: body}
}

// DeliveriesPath builds GET /v1/artifacts/{id}/deliveries.
func (a ArtifactRef) DeliveriesPath() string { return a.path("/deliveries", nil) }

// Patch builds PATCH /v1/artifacts/{id}. ifRevision 0 sends no If-Match.
func (a ArtifactRef) Patch(title *string, keepVersions *int, ifRevision int) HTTPRequest {
	body := map[string]any{}
	if title != nil {
		body["title"] = *title
	}
	if keepVersions != nil {
		body["keep_versions"] = *keepVersions
	}
	req := HTTPRequest{Method: "PATCH", Path: a.path("", nil), Body: body}
	if ifRevision > 0 {
		req.IfMatch = strconv.Itoa(ifRevision)
	}
	return req
}

// Delete builds DELETE /v1/artifacts/{id}.
func (a ArtifactRef) Delete(confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: a.path("", confirmQ(confirm))}
}

// Pin builds POST or DELETE /v1/artifacts/{id}/versions/{n}/pin.
func (a ArtifactRef) Pin(n string, on bool) HTTPRequest {
	method := "POST"
	if !on {
		method = "DELETE"
	}
	return HTTPRequest{Method: method, Path: a.path("/versions/"+url.PathEscape(n)+"/pin", nil)}
}

// Send builds POST /v1/artifacts/{id}/versions/{n}/send. mode is send or draft.
func (a ArtifactRef) Send(n, mode string) HTTPRequest {
	body := map[string]any{}
	if mode != "" {
		body["mode"] = mode
	}
	return HTTPRequest{Method: "POST", Path: a.path("/versions/"+url.PathEscape(n)+"/send", nil), Body: body}
}

// Ask builds POST /v1/artifacts/{id}/ask.
func (a ArtifactRef) Ask(text, selection string, version int) HTTPRequest {
	body := map[string]any{"text": text}
	if selection != "" {
		body["selection"] = selection
	}
	if version > 0 {
		body["version"] = version
	}
	return HTTPRequest{Method: "POST", Path: a.path("/ask", nil), Body: body}
}

// Chat builds POST /v1/artifacts/{id}/chat.
func (a ArtifactRef) Chat(version int) HTTPRequest {
	body := map[string]any{}
	if version > 0 {
		body["version"] = version
	}
	return HTTPRequest{Method: "POST", Path: a.path("/chat", nil), Body: body}
}

func (a ArtifactRef) path(tail string, q map[string]string) string {
	if q == nil {
		q = map[string]string{}
	}
	q["workspace"] = a.Workspace
	return withQuery("/v1/artifacts/"+url.PathEscape(a.Ref)+tail, q)
}

// ArtifactPutSpec is the body of POST /v1/artifacts. Give Content,
// ContentBase64, or FromPath (a path in the working dir of a run).
type ArtifactPutSpec struct {
	Workspace     string
	Name          string
	ArtifactID    string
	Title         string
	Kind          string
	FileName      string
	Content       string
	ContentBase64 string
	FromPath      string
	Note          string
	Owner         string
	Base          *int
	Live          any // {params, sources} of a live template
}

// Build renders the spec as POST /v1/artifacts.
func (p ArtifactPutSpec) Build() HTTPRequest {
	body := map[string]any{}
	for k, v := range map[string]string{
		"workspace": p.Workspace, "name": p.Name, "artifact_id": p.ArtifactID, "title": p.Title, "kind": p.Kind,
		"file_name": p.FileName, "content": p.Content, "content_base64": p.ContentBase64, "from_path": p.FromPath,
		"note": p.Note, "owner": p.Owner,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if p.Base != nil {
		body["base"] = *p.Base
	}
	if p.Live != nil {
		body["live"] = p.Live
	}
	return HTTPRequest{Method: "POST", Path: "/v1/artifacts", Body: body}
}

// BuildRunArtifactsPath builds GET /v1/runs/{id}/artifacts.
func BuildRunArtifactsPath(runID string) string {
	return "/v1/runs/" + url.PathEscape(runID) + "/artifacts"
}

// BuildOperationsListPath builds GET /v1/connectors/{ref}/operations.
func BuildOperationsListPath(ref, workspace string) string {
	return withQuery("/v1/connectors/"+url.PathEscape(ref)+"/operations", map[string]string{"workspace": workspace})
}

// BuildOperationCall builds POST /v1/connectors/{ref}/operations/{op}/call.
func BuildOperationCall(ref, op, workspace string, input map[string]any) HTTPRequest {
	body := map[string]any{"input": input}
	if input == nil {
		body["input"] = map[string]any{}
	}
	if workspace != "" {
		body["workspace"] = workspace
	}
	return HTTPRequest{Method: "POST", Path: "/v1/connectors/" + url.PathEscape(ref) + "/operations/" + url.PathEscape(op) + "/call", Body: body}
}

// BuildOperationState builds PUT /v1/connectors/{ref}/operations/{op}.
func BuildOperationState(ref, op, state string) HTTPRequest {
	return HTTPRequest{Method: "PUT", Path: "/v1/connectors/" + url.PathEscape(ref) + "/operations/" + url.PathEscape(op),
		Body: map[string]any{"state": state}}
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
	Kind, Status, Purpose, Workspace, Conversation, Inbox, Run, Cursor string
	Limit                                                              int
}

// BuildOutboxListPath builds GET /v1/outbox with the filter of q.
func BuildOutboxListPath(q OutboxQuery) string {
	kv := map[string]string{
		"kind": q.Kind, "status": q.Status, "purpose": q.Purpose, "workspace": q.Workspace,
		"conversation": q.Conversation, "inbox": q.Inbox, "run": q.Run, "cursor": q.Cursor,
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
	for k, v := range map[string]string{"from": t.From, "replies": t.Replies, "batch": t.Batch, "workspace": t.Workspace, "job": t.Job} {
		if v != "" {
			body[k] = v
		}
	}
	if t.RouteID != "" {
		body["route_id"] = t.RouteID
	}
	return body
}

// DecisionFilter is the query of GET /v1/decisions.
type DecisionFilter struct {
	Workspace, Kind, Outcome, Status, SubjectKind, SubjectID, Cursor string
	Scope, Feedback                                                  string // connection; agree, disagree, or none
	Waiting                                                          bool
	Limit                                                            int
}

// BuildDecisionListPath builds GET /v1/decisions.
func BuildDecisionListPath(f DecisionFilter) string {
	q := map[string]string{"workspace": f.Workspace, "kind": f.Kind, "outcome": f.Outcome, "status": f.Status,
		"subject_kind": f.SubjectKind, "subject_id": f.SubjectID, "cursor": f.Cursor,
		"scope": f.Scope, "feedback": f.Feedback}
	if f.Waiting {
		q["waiting"] = "1"
	}
	if f.Limit > 0 {
		q["limit"] = strconv.Itoa(f.Limit)
	}
	return withQuery("/v1/decisions", q)
}

// BuildDecisionPath builds GET /v1/decisions/{id}.
func BuildDecisionPath(id string) string { return "/v1/decisions/" + url.PathEscape(id) }

// BuildDecisionRawPath builds GET /v1/decisions/{id}/raw.
func BuildDecisionRawPath(id string) string { return BuildDecisionPath(id) + "/raw" }

// BuildDecisionKindsPath builds GET /v1/decisions/kinds.
func BuildDecisionKindsPath() string { return "/v1/decisions/kinds" }

// BuildDecisionStatsPath builds GET /v1/decisions/stats.
func BuildDecisionStatsPath(workspace, kind, scope, since string) string {
	return withQuery("/v1/decisions/stats", map[string]string{"workspace": workspace, "kind": kind, "scope": scope, "since": since})
}

// BuildDecisionFeedback builds POST /v1/decisions/{id}/feedback.
func BuildDecisionFeedback(id, value string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: BuildDecisionPath(id) + "/feedback", Body: map[string]any{"value": value}}
}

// BuildDecisionTest builds POST /v1/decisions/test.
func BuildDecisionTest(kind, text string, facts map[string]string) HTTPRequest {
	body := map[string]any{"kind": kind, "text": text}
	if len(facts) > 0 {
		body["facts"] = facts
	}
	return HTTPRequest{Method: "POST", Path: "/v1/decisions/test", Body: body}
}

// PauseSpec is the body of POST /v1/pauses and PATCH /v1/pauses/{id}. An
// empty field is not sent. Until is RFC 3339 or "reset"; NoEnd sends
// until "" (no end); For is a length such as "3h" or "2d".
type PauseSpec struct {
	Scope     string
	Target    string
	Workspace string
	Until     string
	NoEnd     bool
	For       string
	Missed    string
	Note      *string
}

func (p PauseSpec) body() map[string]any {
	b := map[string]any{}
	for k, v := range map[string]string{"scope": p.Scope, "target": p.Target, "workspace": p.Workspace,
		"until": p.Until, "for": p.For, "missed": p.Missed} {
		if v != "" {
			b[k] = v
		}
	}
	if p.NoEnd && p.Until == "" && p.For == "" {
		b["until"] = ""
	}
	if p.Note != nil {
		b["note"] = *p.Note
	}
	return b
}

// BuildPauseList builds GET /v1/pauses; all adds the ended pauses.
func BuildPauseList(all bool) HTTPRequest {
	path := "/v1/pauses"
	if all {
		path += "?all=true"
	}
	return HTTPRequest{Method: "GET", Path: path}
}

// BuildPauseGet builds GET /v1/pauses/{id}.
func BuildPauseGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/pauses/" + url.PathEscape(id)}
}

// BuildPauseSet builds POST /v1/pauses.
func BuildPauseSet(p PauseSpec) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/pauses", Body: p.body()}
}

// BuildPauseUpdate builds PATCH /v1/pauses/{id}.
func BuildPauseUpdate(id string, p PauseSpec) HTTPRequest {
	p.Scope, p.Target, p.Workspace = "", "", ""
	return HTTPRequest{Method: "PATCH", Path: "/v1/pauses/" + url.PathEscape(id), Body: p.body()}
}

// BuildPauseResume builds POST /v1/pauses/{id}/resume.
func BuildPauseResume(id string) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/pauses/" + url.PathEscape(id) + "/resume"}
}
