package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// HTTPRequest is a pure description of a CLI→API call (testable without a daemon).
type HTTPRequest struct {
	Method string
	Path   string
	Body   any // nil when no body
}

// BuildRunsCreate builds POST /v1/runs from CLI flags.
// promptOverride is the full prompt sent as prompt_override (caller appends job body if needed).
func BuildRunsCreate(job, workspace, promptOverride string, dryRun bool) HTTPRequest {
	body := map[string]any{
		"job":    job,
		"reason": "manual",
	}
	if workspace != "" {
		body["workspace"] = workspace
	}
	if promptOverride != "" {
		body["prompt_override"] = promptOverride
	}
	if dryRun {
		body["dry_run"] = true
	}
	return HTTPRequest{Method: "POST", Path: "/v1/runs", Body: body}
}

// BuildRunsKill builds POST /v1/runs/{id}/kill.
func BuildRunsKill(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/runs/" + url.PathEscape(id) + "/kill",
	}
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

// BuildRunsStats builds GET /v1/stats/runs.
func BuildRunsStats() HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/stats/runs"}
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

// BuildDoctorFix builds POST /v1/system/doctor/{id}/fix.
func BuildDoctorFix(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/system/doctor/" + url.PathEscape(id) + "/fix",
	}
}

// BuildSkillPut builds PUT /v1/skills/{name}.
func BuildSkillPut(name, skillMD string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/skills/" + url.PathEscape(name),
		Body:   map[string]any{"skill_md": skillMD},
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
func BuildJobMemoryPut(name, workspace, content string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace)),
		Body:   map[string]any{"content": content},
	}
}

// BuildJobMemoryClear builds DELETE /v1/jobs/{name}/memory.
func BuildJobMemoryClear(name, workspace string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory", workspaceQ(workspace)),
	}
}

// BuildAssistantMemoryPut builds PUT /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryPut(workspaceID, content string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory",
		Body:   map[string]any{"content": content},
	}
}

// BuildAssistantMemoryClear builds DELETE /v1/workspaces/{id}/assistant/memory.
func BuildAssistantMemoryClear(workspaceID string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory",
	}
}

// BuildWorkspaceDelete builds DELETE /v1/workspaces/{id}.
func BuildWorkspaceDelete(id string) HTTPRequest {
	return HTTPRequest{Method: "DELETE", Path: "/v1/workspaces/" + url.PathEscape(id)}
}

// BuildWorkspaceRename builds PATCH /v1/workspaces/{id} with name.
func BuildWorkspaceRename(id, name string) HTTPRequest {
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/workspaces/" + url.PathEscape(id),
		Body:   map[string]any{"name": name},
	}
}

// BuildFileListPath builds GET /v1/workspaces/{id}/files?prefix=&cursor=&limit=.
func BuildFileListPath(workspaceID, prefix, afterPath string, limit int) string {
	kv := map[string]string{"prefix": prefix, "cursor": afterPath}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/workspaces/"+url.PathEscape(workspaceID)+"/files", kv)
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

// BuildEventsListPath builds GET /v1/events?cursor=&limit=.
func BuildEventsListPath(cursor string, limit int) string {
	return listQuery("/v1/events", cursor, limit)
}

// BuildEventsRetrigger builds POST /v1/events/{id}/retrigger.
func BuildEventsRetrigger(id string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/events/" + url.PathEscape(id) + "/retrigger",
	}
}

// BuildConversationsListPath builds GET /v1/conversations?cursor=&limit=.
func BuildConversationsListPath(cursor string, limit int) string {
	return listQuery("/v1/conversations", cursor, limit)
}

// BuildConversationGet builds GET /v1/conversations/{id}.
func BuildConversationGet(id string) HTTPRequest {
	return HTTPRequest{Method: "GET", Path: "/v1/conversations/" + url.PathEscape(id)}
}

// BuildApprovalsListPath builds GET /v1/approvals?cursor=&limit=.
func BuildApprovalsListPath(cursor string, limit int) string {
	return listQuery("/v1/approvals", cursor, limit)
}

// BuildServiceShutdown builds POST /v1/system/shutdown.
func BuildServiceShutdown() HTTPRequest {
	return HTTPRequest{Method: "POST", Path: "/v1/system/shutdown"}
}

// BuildJobMemoryEntryAdd builds POST /v1/jobs/{name}/memory/entries.
func BuildJobMemoryEntryAdd(name, workspace, date, text string) HTTPRequest {
	body := map[string]any{"text": text}
	if date != "" {
		body["date"] = date
	}
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory/entries", workspaceQ(workspace)),
		Body:   body,
	}
}

// BuildJobMemoryEntryDelete builds DELETE /v1/jobs/{name}/memory/entries/{id}.
func BuildJobMemoryEntryDelete(name, workspace, entryID string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name)+"/memory/entries/"+url.PathEscape(entryID), workspaceQ(workspace)),
	}
}

// BuildAssistantMemoryEntryAdd builds POST /v1/workspaces/{id}/assistant/memory/entries.
func BuildAssistantMemoryEntryAdd(workspaceID, date, text string) HTTPRequest {
	body := map[string]any{"text": text}
	if date != "" {
		body["date"] = date
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory/entries",
		Body:   body,
	}
}

// BuildAssistantMemoryEntryDelete builds DELETE /v1/workspaces/{id}/assistant/memory/entries/{eid}.
func BuildAssistantMemoryEntryDelete(workspaceID, entryID string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/memory/entries/" + url.PathEscape(entryID),
	}
}

// BuildQueueListPath builds GET /v1/queue?status=&cursor=&limit=.
func BuildQueueListPath(status, cursor string, limit int) string {
	kv := map[string]string{"status": status, "cursor": cursor}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
	}
	return withQuery("/v1/queue", kv)
}

// BuildSchedulesListPath builds GET /v1/schedules?limit=.
func BuildSchedulesListPath(limit int) string {
	return listQuery("/v1/schedules", "", limit)
}

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

// BuildAlertsPath builds GET /v1/alerts?since=.
func BuildAlertsPath(since string) string {
	return withQuery("/v1/alerts", map[string]string{"since": since})
}

// BuildAssistantWake builds POST /v1/workspaces/{id}/assistant/wake.
// Optional promptOverride becomes prompt_override; dryRun sets dry_run.
func BuildAssistantWake(workspaceID, promptOverride string, dryRun bool) HTTPRequest {
	body := map[string]any{
		"reason": "manual",
	}
	if promptOverride != "" {
		body["prompt_override"] = promptOverride
	}
	if dryRun {
		body["dry_run"] = true
	}
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/workspaces/" + url.PathEscape(workspaceID) + "/assistant/wake",
		Body:   body,
	}
}

// AppendExtraPrompt joins the job base prompt with extra text (jobi run -e semantics).
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

// BuildRunsListPath builds GET /v1/runs query from list flags.
// conversation is not an API filter — callers apply it client-side.
// harness filters runs.harness; model filters tokens_model.
func BuildRunsListPath(status, job, workspace, kind, cursor, harness, model string, limit int) string {
	kv := map[string]string{
		"status":    status,
		"job":       job,
		"workspace": workspace,
		"kind":      kind,
		"cursor":    cursor,
		"harness":   harness,
		"model":     model,
	}
	if limit > 0 {
		kv["limit"] = strconv.Itoa(limit)
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

// BuildJobDelete builds DELETE /v1/jobs/{name}.
func BuildJobDelete(name, workspace string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name), workspaceQ(workspace)),
	}
}

// BuildJobPause builds POST pause/unpause.
func BuildJobPause(name, workspace string, pause bool) HTTPRequest {
	op := "unpause"
	if pause {
		op = "pause"
	}
	return HTTPRequest{
		Method: "POST",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name)+"/"+op, workspaceQ(workspace)),
	}
}

// BuildJobSchedule describes how to apply a jobi-style schedule value in trebi.
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
		return JobSchedulePlan{Mode: "unpause", HTTP: BuildJobPause(name, workspace, false)}, nil
	case "disable":
		return JobSchedulePlan{Mode: "pause", HTTP: BuildJobPause(name, workspace, true)}, nil
	case "":
		return JobSchedulePlan{}, fmt.Errorf("schedule value required (enable|disable|cron|@every …)")
	default:
		return JobSchedulePlan{Mode: "put_markdown"}, nil
	}
}

// BuildJobPutMarkdown builds PUT /v1/jobs/{name} with full markdown body.
func BuildJobPutMarkdown(name, workspace, markdown string) HTTPRequest {
	return HTTPRequest{
		Method: "PUT",
		Path:   withQuery("/v1/jobs/"+url.PathEscape(name), workspaceQ(workspace)),
		Body:   map[string]any{"markdown": markdown},
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

// BuildHarnessEnsure builds POST /v1/harnesses/{name}/ensure.
func BuildHarnessEnsure(name string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/ensure",
	}
}

// BuildHarnessAuthGet builds GET /v1/harnesses/{name}/auth.
func BuildHarnessAuthGet(name string) HTTPRequest {
	return HTTPRequest{
		Method: "GET",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/auth",
	}
}

// BuildHarnessAuthPost builds POST /v1/harnesses/{name}/auth (marks auth_ok in daemon state).
// Full interactive vendor login is not available over the HTTP API (see docs).
func BuildHarnessAuthPost(name string) HTTPRequest {
	return HTTPRequest{
		Method: "POST",
		Path:   "/v1/harnesses/" + url.PathEscape(name) + "/auth",
	}
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
func BuildIntegrationDelete(name string) HTTPRequest {
	return HTTPRequest{
		Method: "DELETE",
		Path:   "/v1/integrations/" + url.PathEscape(name),
	}
}

// BuildIntegrationSet builds PUT /v1/integrations/{name} body (upsert).
func BuildIntegrationSet(name, command, format, typ, cwd string, env map[string]string) HTTPRequest {
	if format == "" {
		format = "line"
	}
	if typ == "" {
		typ = "stream"
	}
	body := map[string]any{
		"name":    name,
		"command": command,
		"format":  format,
		"type":    typ,
	}
	if cwd != "" {
		body["cwd"] = cwd
	}
	if len(env) > 0 {
		body["env"] = env
	}
	return HTTPRequest{
		Method: "PUT",
		Path:   "/v1/integrations/" + url.PathEscape(name),
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

// BuildConfigSet builds PATCH /v1/config with dotted key set.
func BuildConfigSet(key, value string) HTTPRequest {
	return HTTPRequest{
		Method: "PATCH",
		Path:   "/v1/config",
		Body: map[string]any{
			"set": map[string]any{key: value},
		},
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
		if keepJobKind(kind, mapStr(j, "kind")) {
			out = append(out, j)
		}
	}
	return out
}

func parseJobListKind(s string) (string, error) {
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
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/v1/workspaces/" + url.PathEscape(workspaceID) + "/files/" + strings.Join(parts, "/")
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
	// Multiple schedules use ", " (comma+space), matching jobi multi-cron examples.
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
