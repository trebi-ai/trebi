// Package request holds the pure CLI/MCP → API request catalog.
// Every Build* function returns an HTTPRequest describing one /v1 call,
// testable without a daemon. No I/O, no trebi imports.
package request

import (
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

// BuildWake renders the spec as POST /v1/workspaces/{id}/assistant/wake. The
// daemon pins job, kind, and workspace; the rest of the spec passes through.
func (r RunSpec) BuildWake() HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/workspaces/"+url.PathEscape(r.Workspace)+"/assistant/wake", waitQ(r.WaitMS)),
		Body:   r.body(),
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

// BuildAgentsState builds GET /v1/agents/state.
func BuildAgentsState() HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/agents/state"}
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

// BuildIntegrationConnect builds POST /v1/integrations/{name}/connect.
func BuildIntegrationConnect(name string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/connect",
	}
}

// BuildIntegrationDisconnect builds POST /v1/integrations/{name}/disconnect.
func BuildIntegrationDisconnect(name string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/disconnect",
	}
}

// BuildIntegrationCursorGet builds GET /v1/integrations/{name}/cursor.
func BuildIntegrationCursorGet(name string) HTTPRequest {
	return HTTPRequest{
		Method: "GET",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/cursor",
	}
}

// BuildIntegrationCursorSet builds PUT /v1/integrations/{name}/cursor with last_event_id.
func BuildIntegrationCursorSet(name, lastEventID string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/cursor",
		Body:   map[string]any{"last_event_id": lastEventID},
	}
}

// BuildIntegrationCursorReset builds PUT /v1/integrations/{name}/cursor with an empty body.
func BuildIntegrationCursorReset(name string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/cursor",
	}
}

// BuildJobMemoryPut builds PUT /v1/jobs/{name}/memory.
func BuildJobMemoryPut(name, workspace, content, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PUT",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace)),
		Body:    map[string]any{"content": content},
		IfMatch: ifMatch,
	}
}

// BuildJobMemoryClear builds DELETE /v1/jobs/{name}/memory.
func BuildJobMemoryClear(name, workspace, ifMatch string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory", mergeQ(workspaceQ(workspace), confirmQ(confirm))),
		IfMatch: ifMatch,
	}
}

// BuildAssistantMemoryPut builds PUT /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryPut(workspaceID, content, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PUT",
		Path:    "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory",
		Body:    map[string]any{"content": content},
		IfMatch: ifMatch,
	}
}

// BuildAssistantMemoryClear builds DELETE /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryClear(workspaceID, ifMatch string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery("/v1/workspaces/"+url.PathEscape(workspaceID)+"/assistant/memory", confirmQ(confirm)),
		IfMatch: ifMatch,
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

// BuildAssistantPut builds PUT /v1/workspaces/{id}/assistant with TREBI.md content.
// Empty content lets the server write its default template.
func BuildAssistantPut(workspaceID, content, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PUT",
		Path:    "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant",
		Body:    map[string]string{"content": content},
		IfMatch: ifMatch,
	}
}

// BuildAssistantPatch builds PATCH /v1/workspaces/{id}/assistant (disabled flag).
// disabled nil leaves the flag alone.
func BuildAssistantPatch(workspaceID string, disabled *bool, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "PATCH",
		Path:    "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant",
		Body:    map[string]any{"disabled": disabled},
		IfMatch: ifMatch,
	}
}

// BuildAssistantDelete builds DELETE /v1/workspaces/{id}/assistant.
func BuildAssistantDelete(workspaceID, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant",
		IfMatch: ifMatch,
	}
}

// BuildAssistantMemoryGetPath builds GET /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryGetPath(workspaceID string) string {
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory"
}

// BuildWorkspaceDelete builds DELETE /v1/workspaces/{id}.
func BuildWorkspaceDelete(id string, confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/workspaces/"+url.PathEscape(id), confirmQ(confirm))}
}

// BuildWorkspaceAdd builds POST /v1/workspaces.
func BuildWorkspaceAdd(path, name string, create bool) HTTPRequest {
	body := map[string]any{}
	if name != "" {
		body["name"] = name
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
func BuildWorkspacePatch(id string, name *string, gitEnabled *bool) HTTPRequest {
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if gitEnabled != nil {
		body["git_enabled"] = *gitEnabled
	}
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/workspaces/" + url.PathEscape(id),
		Body:   body,
	}
}

// BuildWorkspaceRename builds PATCH /v1/workspaces/{id} with name.
func BuildWorkspaceRename(id, name string) HTTPRequest {
	return BuildWorkspacePatch(id, &name, nil)
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

// BuildRulesTest builds POST /v1/rules/test.
func BuildRulesTest(match, extract string, event any) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/rules/test",
		Body:   map[string]any{"match": match, "extract": extract, "event": event},
	}
}

// BuildRulesFire builds POST /v1/rules/fire.
func BuildRulesFire(integration, workspace, job, match, extract string, event any, onIndex int) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/rules/fire",
		Body: map[string]any{
			"integration": integration,
			"workspace":   workspace,
			"job":         job,
			"match":       match,
			"extract":     extract,
			"event":       event,
			"on_index":    onIndex,
		},
	}
}

// BuildEventsListPath builds GET /v1/events?cursor=&limit=&integration=&since=&q=.
func BuildEventsListPath(cursor string, limit int, integration, since, q string) string {
	return withQuery("/v1/events", mergeQ(
		map[string]string{"cursor": cursor, "integration": integration, "since": since, "q": q},
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
func BuildConversationsListPath(workspace, channel, cursor string, limit int) string {
	kv := map[string]string{"workspace": workspace, "channel": channel, "cursor": cursor}
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
func BuildConversationCreate(workspace, title, originChannel, channelType, channelRef string) HTTPRequest {
	body := map[string]any{"workspace": workspace, "origin_channel": originChannel}
	if title != "" {
		body["title"] = title
	}
	if channelType != "" && channelRef != "" {
		body["activate"] = map[string]any{"channel_type": channelType, "channel_ref": channelRef}
	}
	return HTTPRequest{Method: "POST", Path: "/v1/conversations", Body: body}
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
func BuildConversationSend(id, text, channelType, channelRef, idempotencyKey string) HTTPRequest {
	body := map[string]any{"text": text, "channel_type": channelType, "channel_ref": channelRef}
	if idempotencyKey != "" {
		body["idempotency_key"] = idempotencyKey
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/conversations/" + url.PathEscape(id) + "/messages",
		Body:   body,
	}
}

// BuildPointerGet builds GET /v1/channel-pointers/{type}/{ref}.
func BuildPointerGet(channelType, channelRef string) HTTPRequest {
	return HTTPRequest{
		Method: "GET",
		Path:   "/v1/channel-pointers/" + url.PathEscape(channelType) + "/" + url.PathEscape(channelRef),
	}
}

// BuildPointerPut builds PUT /v1/channel-pointers/{type}/{ref}.
func BuildPointerPut(channelType, channelRef, conversationID string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/channel-pointers/" + url.PathEscape(channelType) + "/" + url.PathEscape(channelRef),
		Body:   map[string]any{"conversation_id": conversationID},
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

// BuildSystemOverviewPath builds GET /v1/system/overview.
func BuildSystemOverviewPath() string {
	return "/v1/system/overview"
}

// BuildCloudStatusPath builds GET /v1/cloud/status.
func BuildCloudStatusPath() string {
	return "/v1/cloud/status"
}

// BuildCloudLink builds POST /v1/cloud/link.
func BuildCloudLink() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/cloud/link"}
}

// BuildCloudUnlink builds DELETE /v1/cloud/link.
func BuildCloudUnlink(confirm bool) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: withQuery("/v1/cloud/link", confirmQ(confirm))}
}

// BuildJobMemoryEntryAdd builds POST /v1/jobs/{name}/memory/entries.
func BuildJobMemoryEntryAdd(name, workspace, date, text, ifMatch string) HTTPRequest {
	body := map[string]any{"text": text}
	if date != "" {
		body["date"] = date
	}
	return HTTPRequest{
		Method:  "POST",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory/entries", workspaceQ(workspace)),
		Body:    body,
		IfMatch: ifMatch,
	}
}

// BuildJobMemoryEntryDelete builds DELETE /v1/jobs/{name}/memory/entries/{id}.
func BuildJobMemoryEntryDelete(name, workspace, entryID, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory/entries/"+url.PathEscape(entryID), workspaceQ(workspace)),
		IfMatch: ifMatch,
	}
}

// BuildAssistantMemoryEntryAdd builds POST /v1/workspaces/{id}/assistant/memory/entries.
func BuildAssistantMemoryEntryAdd(workspaceID, date, text, ifMatch string) HTTPRequest {
	body := map[string]any{"text": text}
	if date != "" {
		body["date"] = date
	}
	return HTTPRequest{
		Method:  "POST",
		Path:    "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory/entries",
		Body:    body,
		IfMatch: ifMatch,
	}
}

// BuildAssistantMemoryEntryDelete builds DELETE /v1/workspaces/{id}/assistant/memory/entries/{eid}.
func BuildAssistantMemoryEntryDelete(workspaceID, entryID, ifMatch string) HTTPRequest {
	return HTTPRequest{
		Method:  "DELETE",
		Path:    "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory/entries/" + url.PathEscape(entryID),
		IfMatch: ifMatch,
	}
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

// BuildSkillsListPath builds GET /v1/skills?limit=.
func BuildSkillsListPath(limit int) string {
	return listQuery("/v1/skills", "", limit)
}

// BuildIntegrationsListPath builds GET /v1/integrations?limit=.
func BuildIntegrationsListPath(limit int) string {
	return listQuery("/v1/integrations", "", limit)
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
	ChannelType    string // defaults to cli server-side
	ChannelRef     string // defaults to the workspace id server-side
	IdempotencyKey string
	WaitMS         int // long-poll for the assistant reply
}

// Build renders the ask as POST /v1/workspaces/{id}/assistant/messages.
func (a AskSpec) Build() HTTPRequest {
	body := map[string]any{"text": a.Text}
	for k, v := range map[string]string{
		"channel_type":    a.ChannelType,
		"channel_ref":     a.ChannelRef,
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
	Description    string
	Cron           string
	Every          string
	Event          string
	Match          string
	Extract        string
	IdempotencyKey string
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
		"prompt": j.Prompt, "harness": j.Harness, "description": j.Description,
		"cron": j.Cron, "every": j.Every, "idempotency_key": j.IdempotencyKey,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if j.Event != "" {
		on := map[string]any{"event": j.Event}
		if j.Match != "" {
			on["match"] = j.Match
		}
		if j.Extract != "" {
			on["extract"] = j.Extract
		}
		body["on"] = on
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

// BuildJobMemoryGetPath builds GET /v1/jobs/{name}/memory?workspace=.
func BuildJobMemoryGetPath(name, workspace string) string {
	return withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace))
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

// BuildIntegrationGetPath builds GET /v1/integrations/{name}.
func BuildIntegrationGetPath(name string) string {
	return "/v1/integrations/" + url.PathEscape(name)
}

// BuildIntegrationCreate builds POST /v1/integrations with a full definition.
func BuildIntegrationCreate(def map[string]any) HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/integrations", Body: def}
}

// BuildIntegrationRestart builds POST /v1/integrations/{name}/restart.
func BuildIntegrationRestart(name string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/restart",
	}
}

// BuildIntegrationStop builds POST /v1/integrations/{name}/stop.
func BuildIntegrationStop(name string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/stop",
	}
}

// BuildIntegrationDelete builds DELETE /v1/integrations/{name}.
func BuildIntegrationDelete(name string, confirm bool) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   withQuery("/v1/integrations/"+url.PathEscape(name), confirmQ(confirm)),
	}
}

// IntegrationSpec is the PUT /v1/integrations/{name} body. Empty fields are
// omitted, and the daemon keeps the current value: PUT is an upsert-merge.
type IntegrationSpec struct {
	Name    string
	Command string
	Format  string // json|line
	Type    string // stream|cli|mcp
	Cwd     string
	Env     map[string]string
	Facets  []string
}

// Build renders the spec as PUT /v1/integrations/{name}.
func (i IntegrationSpec) Build() HTTPRequest {
	body := map[string]any{"name": i.Name}
	for k, v := range map[string]string{
		"command": i.Command,
		"format":  i.Format,
		"type":    i.Type,
		"cwd":     i.Cwd,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if len(i.Env) > 0 {
		body["env"] = i.Env
	}
	if len(i.Facets) > 0 {
		body["facets"] = i.Facets
	}
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/integrations/" + url.PathEscape(i.Name),
		Body:   body,
	}
}

// BuildIntegrationTest builds POST /v1/integrations/{name}/test.
func BuildIntegrationTest(name string, event any) HTTPRequest {
	body := map[string]any{}
	if event != nil {
		body["event"] = event
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/integrations/" + url.PathEscape(name) + "/test",
		Body:   body,
	}
}

// BuildIntegrationLogsPath builds GET logs path.
func BuildIntegrationLogsPath(name string, follow bool) string {
	kv := map[string]string{}
	if follow {
		kv["follow"] = "1"
	}
	return withQuery("/v1/integrations/"+url.PathEscape(name)+"/logs", kv)
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

// FilterJobsByKindFlag keeps rows matching --kind (job|assistant|all).
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

// ParseJobListKind validates a --kind flag value (job|assistant|all).
func ParseJobListKind(s string) (string, error) {
	switch strings.TrimSpace(s) {
	case "", "job":
		return "job", nil
	case "assistant", "all":
		return s, nil
	default:
		return "", fmt.Errorf("invalid --kind %q (want job|assistant|all)", s)
	}
}

func keepJobKind(want, rowKind string) bool {
	switch want {
	case "all":
		return true
	case "assistant":
		return rowKind == "assistant"
	default:
		return rowKind != "assistant"
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

// keepEventTriggers keeps event integrations from a triggers list; time triggers drop.
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
