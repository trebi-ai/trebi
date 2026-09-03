package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/request"
)

// mcpInput is the flat input object of one tool call: path params, query
// params, and body fields together in one map.
type mcpInput map[string]any

// str reads one optional string field. An absent field reads as "".
func (in mcpInput) str(key string) (string, error) {
	v, ok := in[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("input field %q must be a string", key)
	}
	return s, nil
}

// strs reads optional string fields in order.
func (in mcpInput) strs(keys ...string) ([]string, error) {
	vals := make([]string, len(keys))
	for i, k := range keys {
		v, err := in.str(k)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

// reqStr reads one required non-empty string field.
func (in mcpInput) reqStr(key string) (string, error) {
	s, err := in.str(key)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", fmt.Errorf("input field %q is required", key)
	}
	return s, nil
}

// reqStrs reads several required non-empty string fields in order.
func (in mcpInput) reqStrs(keys ...string) ([]string, error) {
	vals := make([]string, len(keys))
	for i, k := range keys {
		v, err := in.reqStr(k)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

// head reads one required field followed by optional fields.
func (in mcpInput) head(req string, opts ...string) ([]string, error) {
	first, err := in.reqStr(req)
	if err != nil {
		return nil, err
	}
	rest, err := in.strs(opts...)
	if err != nil {
		return nil, err
	}
	return append([]string{first}, rest...), nil
}

// optStr reads one string field with presence: nil when absent or empty.
func (in mcpInput) optStr(key string) (*string, error) {
	s, err := in.str(key)
	if err != nil || s == "" {
		return nil, err
	}
	return &s, nil
}

// presentStr reads one string field with presence: nil only when the key is
// absent. An explicit "" clears the field it patches.
func (in mcpInput) presentStr(key string) (*string, error) {
	if _, ok := in[key]; !ok {
		return nil, nil
	}
	s, err := in.str(key)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// num reads one optional integer field. ok is false when the field is absent.
func (in mcpInput) num(key string) (n int, ok bool, err error) {
	v, exists := in[key]
	if !exists || v == nil {
		return 0, false, nil
	}
	f, isNum := v.(float64)
	if !isNum {
		return 0, false, fmt.Errorf("input field %q must be a number", key)
	}
	return int(f), true, nil
}

// flag reads one optional boolean field. An absent field reads as false.
func (in mcpInput) flag(key string) (bool, error) {
	v, ok := in[key]
	if !ok || v == nil {
		return false, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, fmt.Errorf("input field %q must be a boolean", key)
	}
	return b, nil
}

// optBool reads one boolean field with presence: nil when the field is absent.
func (in mcpInput) optBool(key string) (*bool, error) {
	v, exists := in[key]
	if !exists || v == nil {
		return nil, nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("input field %q must be a boolean", key)
	}
	return &b, nil
}

// obj reads one object field: nil when the field is absent.
func (in mcpInput) obj(key string) (map[string]any, error) {
	v, ok := in[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("input field %q must be an object", key)
	}
	return m, nil
}

// strMap reads one string-to-string object field: nil when the field is absent.
func (in mcpInput) strMap(key string) (map[string]string, error) {
	m, err := in.obj(key)
	if err != nil || m == nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("input field %q must map strings to strings", key)
		}
		out[k] = s
	}
	return out, nil
}

// strList reads one array-of-strings field: nil when the field is absent.
func (in mcpInput) strList(key string) ([]string, error) {
	v, ok := in[key]
	if !ok || v == nil {
		return nil, nil
	}
	arr, isArr := v.([]any)
	if !isArr {
		return nil, fmt.Errorf("input field %q must be an array of strings", key)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, isStr := e.(string)
		if !isStr {
			return nil, fmt.Errorf("input field %q must be an array of strings", key)
		}
		out[i] = s
	}
	return out, nil
}

// pick collects the named fields that are present, keeping their JSON types.
// An empty result means the caller gave none of them.
func (in mcpInput) pick(keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := in[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// scalarStr reads one field that may arrive as a string or a number. A large
// cursor loses precision as a float64, so a string spelling is accepted.
func (in mcpInput) scalarStr(key string) (string, error) {
	v, ok := in[key]
	if !ok || v == nil {
		return "", nil
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case float64:
		return strconv.FormatInt(int64(t), 10), nil
	case json.Number:
		return t.String(), nil
	default:
		return "", fmt.Errorf("input field %q must be a string or a number", key)
	}
}

// value reads one field as a raw JSON value: nil when the field is absent.
func (in mcpInput) value(key string) any { return in[key] }

// owner reads the file-plane owner selector: workspace (default), job, or skill.
func (in mcpInput) owner() (string, error) {
	owner, err := in.str("owner")
	if err != nil {
		return "", err
	}
	switch owner {
	case "", "workspace":
		return "workspace", nil
	case "job", "skill":
		return owner, nil
	default:
		return "", fmt.Errorf("input field %q must be workspace, job, or skill", "owner")
	}
}

// buildFileList lists files for one owner: workspace, job, or skill.
func (in mcpInput) buildFileList() (request.HTTPRequest, error) {
	v, err := in.strs("workspace", "name", "prefix", "cursor")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	limit, _, err := in.num("limit")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	recursive, err := in.flag("recursive")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	owner, err := in.owner()
	if err != nil {
		return request.HTTPRequest{}, err
	}
	switch owner {
	case "workspace":
		if v[0] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "workspace")
		}
		return mcpGet(request.BuildFileListPath(v[0], v[2], v[3], limit, recursive)), nil
	case "job":
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return mcpGet(request.BuildJobFileListPath(v[1], v[0], v[2], v[3], limit, recursive)), nil
	default:
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return mcpGet(request.BuildSkillFileListPath(v[1], v[2], v[3], limit, recursive)), nil
	}
}

// buildFileGet reads one file for one owner. stat returns JSON metadata only.
func (in mcpInput) buildFileGet() (request.HTTPRequest, error) {
	v, err := in.strs("workspace", "name", "path")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	stat, err := in.flag("stat")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	owner, err := in.owner()
	if err != nil {
		return request.HTTPRequest{}, err
	}
	if v[2] == "" {
		return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "path")
	}
	switch owner {
	case "workspace":
		if v[0] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "workspace")
		}
		return mcpGet(request.BuildFileGetPath(v[0], v[2], stat)), nil
	case "job":
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return mcpGet(request.BuildJobFileGetPath(v[1], v[0], v[2], stat)), nil
	default:
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return mcpGet(request.BuildSkillFileGetPath(v[1], v[2], stat)), nil
	}
}

// buildFilePut writes one text file for one owner with optional If-Match.
func (in mcpInput) buildFilePut() (request.HTTPRequest, error) {
	v, err := in.strs("workspace", "name", "path", "content", "if_match")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	owner, err := in.owner()
	if err != nil {
		return request.HTTPRequest{}, err
	}
	if v[2] == "" {
		return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "path")
	}
	if v[3] == "" {
		return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
	}
	content, err := in.decodeContent(v[3])
	if err != nil {
		return request.HTTPRequest{}, err
	}
	switch owner {
	case "workspace":
		if v[0] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "workspace")
		}
		return request.BuildFilePut(v[0], v[2], content, "", v[4]), nil
	case "job":
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return request.BuildJobFilePut(v[1], v[0], v[2], content, "", v[4]), nil
	default:
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return request.BuildSkillFilePut(v[1], v[2], content, "", v[4]), nil
	}
}

// decodeContent reads the content field. encoding "base64" carries binary; the
// default "text" is passed through as written.
func (in mcpInput) decodeContent(content string) ([]byte, error) {
	encoding, err := in.str("encoding")
	if err != nil {
		return nil, err
	}
	switch encoding {
	case "", "text":
		return []byte(content), nil
	case "base64":
		raw, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("input field %q is not valid base64: %w", "content", err)
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("input field %q must be text or base64", "encoding")
	}
}

// buildFileDelete removes one file for one owner with optional If-Match.
func (in mcpInput) buildFileDelete() (request.HTTPRequest, error) {
	v, err := in.strs("workspace", "name", "path", "if_match")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	owner, err := in.owner()
	if err != nil {
		return request.HTTPRequest{}, err
	}
	confirm, err := in.flag("confirm")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	if v[2] == "" {
		return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "path")
	}
	switch owner {
	case "workspace":
		if v[0] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "workspace")
		}
		return request.BuildFileDelete(v[0], v[2], v[3], confirm), nil
	case "job":
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return request.BuildJobFileDelete(v[1], v[0], v[2], v[3]), nil
	default:
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return request.BuildSkillFileDelete(v[1], v[2], v[3]), nil
	}
}

// jobSpec reads the authoring body of job.create and job.validate_text.
func (in mcpInput) jobSpec() (request.JobSpec, error) {
	v, err := in.strs("name", "workspace", "content", "prompt", "harness", "description")
	if err != nil {
		return request.JobSpec{}, err
	}
	t, err := in.strs("cron", "every", "event", "match", "extract", "idempotency_key")
	if err != nil {
		return request.JobSpec{}, err
	}
	return request.JobSpec{
		Name: v[0], Workspace: v[1], Content: v[2], Prompt: v[3], Harness: v[4], Description: v[5],
		Cron: t[0], Every: t[1], Event: t[2], Match: t[3], Extract: t[4], IdempotencyKey: t[5],
	}, nil
}

// runSpec reads the launch body of job.run. It is the one launch shape on
// every surface.
func (in mcpInput) runSpec() (request.RunSpec, error) {
	v, err := in.strs("name", "workspace", "prompt", "extra_prompt", "harness", "kind", "reason", "conversation_id", "idempotency_key")
	if err != nil {
		return request.RunSpec{}, err
	}
	inputs, err := in.strMap("inputs")
	if err != nil {
		return request.RunSpec{}, err
	}
	dryRun, err := in.flag("dry_run")
	if err != nil {
		return request.RunSpec{}, err
	}
	wait, _, err := in.num("wait_ms")
	if err != nil {
		return request.RunSpec{}, err
	}
	return request.RunSpec{
		Job: v[0], Workspace: v[1], Prompt: v[2], ExtraPrompt: v[3],
		Harness: v[4], Kind: v[5], Reason: v[6], ConversationID: v[7],
		IdempotencyKey: v[8], Inputs: inputs, DryRun: dryRun, WaitMS: wait,
	}, nil
}

// mcpAction maps one tool action onto /v1 routes.
type mcpAction struct {
	routes      []string                                                   // registered patterns this action can hit
	sample      mcpInput                                                   // valid example input (docs, parity checks)
	samples     []mcpInput                                                 // extra examples; with sample they cover every route
	build       func(in mcpInput) (request.HTTPRequest, error)             // input → pure request
	transform   func(in mcpInput, status int, body []byte) ([]byte, error) // optional response rewrite
	destructive bool                                                       // sets the MCP destructiveHint annotation
	bounded     bool                                                       // honors max_bytes; a long body arrives truncated
	tail        bool                                                       // bounded from the end, not the start (logs)
}

// defaultMaxBytes caps a bounded body when the caller names no max_bytes. A
// whole log file in the model context is never the useful answer.
const defaultMaxBytes = 64 << 10

// bound marks one entry as size-bounded. tail keeps the end of the body.
func bound(e mcpAction, tail bool) mcpAction {
	e.bounded, e.tail = true, tail
	return e
}

// clampBody cuts body to max_bytes and reports whether it cut anything.
func (in mcpInput) clampBody(body []byte, tail bool) ([]byte, bool, error) {
	max, ok, err := in.num("max_bytes")
	if err != nil {
		return nil, false, err
	}
	if !ok || max <= 0 {
		max = defaultMaxBytes
	}
	if len(body) <= max {
		return body, false, nil
	}
	if tail {
		return body[len(body)-max:], true, nil
	}
	return body[:max], true, nil
}

// act starts one catalog entry on a single route.
func act(route string, sample mcpInput, build func(mcpInput) (request.HTTPRequest, error)) mcpAction {
	return mcpAction{routes: []string{route}, sample: sample, build: build}
}

// actSamples is act with extra examples, for a field pair that only changes
// the request when both halves are given.
func actSamples(route string, sample mcpInput, extra []mcpInput, build func(mcpInput) (request.HTTPRequest, error)) mcpAction {
	e := act(route, sample, build)
	e.samples = extra
	return e
}

// multiAct starts one entry that spans several patterns (owner-scoped routes).
func multiAct(sample mcpInput, build func(mcpInput) (request.HTTPRequest, error), routes ...string) mcpAction {
	return mcpAction{routes: routes, sample: sample, build: build}
}

// del is act plus the destructive annotation.
func del(route string, sample mcpInput, build func(mcpInput) (request.HTTPRequest, error)) mcpAction {
	e := act(route, sample, build)
	e.destructive = true
	return e
}

// fileEntry builds one file-plane entry. The owner-variant samples make the
// parity check cover all three owner routes.
func fileEntry(sample mcpInput, build func(mcpInput) (request.HTTPRequest, error), routes ...string) mcpAction {
	e := multiAct(sample, build, routes...)
	e.samples = []mcpInput{
		{"owner": "job", "name": "nightly-sync", "workspace": "ws1", "path": "notes/a.md", "prefix": "notes/", "content": "# hi\n"},
		{"owner": "skill", "name": "deploy", "path": "notes/a.md", "prefix": "notes/", "content": "# hi\n"},
	}
	return e
}

// mcpGet wraps one built path into a body-less GET.
func mcpGet(path string) request.HTTPRequest {
	return request.HTTPRequest{Method: http.MethodGet, Path: path}
}

// mcpPost wraps one built path into a body-less POST.
func mcpPost(path string) request.HTTPRequest {
	return request.HTTPRequest{Method: http.MethodPost, Path: path}
}

// idGet adapts one required field to a GET path builder.
func idGet(route, key, sampleVal string, path func(string) string) mcpAction {
	return act(route, mcpInput{key: sampleVal}, func(in mcpInput) (request.HTTPRequest, error) {
		id, err := in.reqStr(key)
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return mcpGet(path(id)), nil
	})
}

// getReq adapts one required field to a GET request builder.
func getReq(route, key, sampleVal string, build func(string) request.HTTPRequest) mcpAction {
	return act(route, mcpInput{key: sampleVal}, func(in mcpInput) (request.HTTPRequest, error) {
		id, err := in.reqStr(key)
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return build(id), nil
	})
}

// idPost adapts one required field to a POST request builder.
func idPost(route, key, sampleVal string, build func(string) request.HTTPRequest) mcpAction {
	return act(route, mcpInput{key: sampleVal}, func(in mcpInput) (request.HTTPRequest, error) {
		id, err := in.reqStr(key)
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return build(id), nil
	})
}

// bulkAct takes one id or an ids list and picks the matching route. The bulk
// reply names the ids that failed, so a partial run is visible.
func bulkAct(route, bulkRoute, sampleVal string, one func(string) request.HTTPRequest,
	many func([]string) request.HTTPRequest) mcpAction {
	e := actSamples(route, mcpInput{"id": sampleVal}, []mcpInput{{"ids": []any{sampleVal}}},
		func(in mcpInput) (request.HTTPRequest, error) {
			ids, err := in.strList("ids")
			if err != nil {
				return request.HTTPRequest{}, err
			}
			if len(ids) > 0 {
				return many(ids), nil
			}
			id, err := in.reqStr("id")
			if err != nil {
				return request.HTTPRequest{}, err
			}
			return one(id), nil
		})
	e.routes = []string{route, bulkRoute}
	return e
}

// idDel is idPost plus the destructive annotation.
func idDel(route, key, sampleVal string, build func(string) request.HTTPRequest) mcpAction {
	e := idPost(route, key, sampleVal, build)
	e.destructive = true
	return e
}

// confirmDel adapts one required field plus the confirm gate to a destructive
// request builder. The API answers `confirm_required` without it.
func confirmDel(route, key, sampleVal string, build func(string, bool) request.HTTPRequest) mcpAction {
	e := act(route, mcpInput{key: sampleVal, "confirm": true}, func(in mcpInput) (request.HTTPRequest, error) {
		id, err := in.reqStr(key)
		if err != nil {
			return request.HTTPRequest{}, err
		}
		confirm, err := in.flag("confirm")
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return build(id, confirm), nil
	})
	e.destructive = true
	return e
}

// newMCPCatalog builds the tool→action table. One action per CLI verb; every
// entry names the /v1 pattern it hits so the parity test can check coverage.
func newMCPCatalog() map[string]map[string]mcpAction { //nolint:gocyclo,funlen // static action table, one literal per action
	return map[string]map[string]mcpAction{
		"run": {
			"list": act("GET /v1/runs", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.strs("status", "job", "workspace", "kind", "cursor", "harness", "model")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				f, err := in.strs("conversation", "end_reason", "q", "since", "until")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.RunsQuery{
					Status: v[0], Job: v[1], Workspace: v[2], Kind: v[3], Cursor: v[4],
					Harness: v[5], Model: v[6], Conversation: f[0], EndReason: f[1],
					Q: f[2], Since: f[3], Until: f[4], Limit: limit,
				}.Path()), nil
			}),
			"show": act("GET /v1/runs/{id}", mcpInput{"id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStr("id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				wait, _, err := in.num("wait_ms")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildRunGetPath(id, wait)), nil
			}),
			"timeline": idGet("GET /v1/runs/{id}/timeline", "id", "01ABC", request.BuildRunTimelinePath),
			"transcript": bound(act("GET /v1/runs/{id}/transcript", mcpInput{"id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStr("id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				raw, err := in.flag("raw")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunTranscript(id, raw), nil
			}), true),
			"logs": bound(idGet("GET /v1/runs/{id}/log", "id", "01ABC", func(id string) string { return request.BuildRunLogPath(id, false) }), true),
			"kill": func() mcpAction {
				e := bulkAct("POST /v1/runs/{id}/kill", "POST /v1/runs/kill", "01ABC",
					request.BuildRunsKill, request.BuildRunsKillBulk)
				e.destructive = true
				return e
			}(),
			"resume": idPost("POST /v1/runs/{id}/resume", "id", "01ABC", request.BuildRunResume),
			"replay": idPost("POST /v1/runs/{id}/replay", "id", "01ABC", request.BuildRunReplay),
			"retry":  idPost("POST /v1/runs/{id}/retry", "id", "01ABC", request.BuildRunsRetry),
			"message": act("POST /v1/runs/{id}/message", mcpInput{"id": "01ABC", "text": "ship it"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.reqStrs("id", "text")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunMessage(v[0], v[1]), nil
			}),
			"approve": act("POST /v1/runs/{id}/approve", mcpInput{"id": "01ABC", "decision": "allow"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.reqStrs("id", "decision")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				approvalID, err := in.str("approval_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunApprove(v[0], v[1], approvalID), nil
			}),
			"approvals": act("GET /v1/approvals", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				cursor, err := in.str("cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				state, err := in.str("state")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildApprovalsListPath(cursor, limit, state)), nil
			}),
			"stats": act("GET /v1/stats/runs", mcpInput{"range": "mtd"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.strs("workspace", "job", "range")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunsStats(v[0], v[1], v[2]), nil
			}),
			"agents": act("GET /v1/agents/state", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildAgentsState(), nil }),
		},

		"job": {
			"list": act("GET /v1/jobs", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.strs("workspace", "q", "harness", "active")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobsListPath(v[0], v[1], v[2], v[3])), nil
			}),
			"show": act("GET /v1/jobs/{name}", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobGetPath(v[0], v[1])), nil
			}),
			"create": actSamples("POST /v1/jobs",
				mcpInput{"name": "nightly-sync", "workspace": "ws1", "prompt": "Run the sync.", "cron": "0 3 * * *"},
				[]mcpInput{{"name": "on-mail", "workspace": "ws1", "prompt": "Read it.", "event": "mail"}},
				func(in mcpInput) (request.HTTPRequest, error) {
					spec, err := in.jobSpec()
					if err != nil {
						return request.HTTPRequest{}, err
					}
					for field, v := range map[string]string{"name": spec.Name, "workspace": spec.Workspace} {
						if v == "" {
							return request.HTTPRequest{}, fmt.Errorf("input field %q is required", field)
						}
					}
					if spec.Content == "" && spec.Prompt == "" {
						return request.HTTPRequest{}, fmt.Errorf("input field %q or %q is required", "prompt", "content")
					}
					return spec.Build(), nil
				}),
			"validate_text": actSamples("POST /v1/jobs/validate",
				mcpInput{"prompt": "Run the sync.", "cron": "0 3 * * *"},
				[]mcpInput{{"prompt": "Read it.", "event": "mail"}},
				func(in mcpInput) (request.HTTPRequest, error) {
					spec, err := in.jobSpec()
					if err != nil {
						return request.HTTPRequest{}, err
					}
					return spec.BuildValidate(), nil
				}),
			"set": act("PUT /v1/jobs/{name}", mcpInput{"name": "nightly-sync", "content": "---\nharness: claude\n---\n\nRun the sync.\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildJobPutMarkdown(v[0], v[1], v[2], v[3]), nil
			}),
			"patch": act("PATCH /v1/jobs/{name}", mcpInput{"name": "nightly-sync", "disabled": true}, func(in mcpInput) (request.HTTPRequest, error) {
				name, err := in.reqStr("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				ws, err := in.str("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				patch := in.pick(jobPatchFields...)
				if len(patch) == 0 {
					return request.HTTPRequest{}, fmt.Errorf("give at least one of %s", strings.Join(jobPatchFields, ", "))
				}
				ifMatch, err := in.str("if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildJobPatch(name, ws, patch, ifMatch), nil
			}),
			"delete": del("DELETE /v1/jobs/{name}", mcpInput{"name": "nightly-sync", "confirm": true}, func(in mcpInput) (request.HTTPRequest, error) {
				name, err := in.reqStr("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				ws, err := in.str("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				purge, err := in.flag("purge")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				ifMatch, err := in.str("if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildJobDelete(name, ws, ifMatch, purge, confirm), nil
			}),
			"run": act("POST /v1/jobs/{name}/run", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				spec, err := in.runSpec()
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if spec.Job == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
				}
				return spec.Build(), nil
			}),
			"pause":   act("POST /v1/jobs/{name}/pause", mcpInput{"name": "nightly-sync"}, jobPauseAction(true)),
			"unpause": act("POST /v1/jobs/{name}/unpause", mcpInput{"name": "nightly-sync"}, jobPauseAction(false)),
			"validate": act("POST /v1/jobs/{name}/validate", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpPost(request.BuildJobValidatePath(v[0], v[1])), nil
			}),
			"schedules": act("GET /v1/schedules", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				v, err := in.strs("workspace", "due_before")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildSchedulesListPath(limit, v[0], v[1])), nil
			}),
			"memory_show": act("GET /v1/jobs/{name}/memory", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobMemoryGetPath(v[0], v[1])), nil
			}),
			"memory_set": act("PUT /v1/jobs/{name}/memory", mcpInput{"name": "nightly-sync", "content": "# MEMORY\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildJobMemoryPut(v[0], v[1], v[2], v[3]), nil
			}),
			"memory_add": act("POST /v1/jobs/{name}/memory/entries", mcpInput{"name": "nightly-sync", "text": "learned the API"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "date", "text", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[3] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildJobMemoryEntryAdd(v[0], v[1], v[2], v[3], v[4]), nil
			}),
			"memory_delete": del("DELETE /v1/jobs/{name}/memory/entries/{id}", mcpInput{"name": "nightly-sync", "entry_id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStrs("name", "entry_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				v, err := in.strs("workspace", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildJobMemoryEntryDelete(id[0], v[0], id[1], v[1]), nil
			}),
			"memory_clear": del("DELETE /v1/jobs/{name}/memory", mcpInput{"name": "nightly-sync", "confirm": true}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				ifMatch, err := in.str("if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildJobMemoryClear(v[0], v[1], ifMatch, confirm), nil
			}),
		},

		"workspace": {
			"list": act("GET /v1/workspaces", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildWorkspacesListPath(limit)), nil
			}),
			"show": idGet("GET /v1/workspaces/{id}", "id", "ws1", request.BuildWorkspaceGetPath),
			"create": act("POST /v1/workspaces", mcpInput{"path": "/tmp/reports", "name": "reports"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.strs("path", "name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				mkdir, err := in.flag("mkdir")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				// A pathless create mints a folder under the trebi home; it
				// needs mkdir plus a name.
				if v[0] == "" && (!mkdir || v[1] == "") {
					return request.HTTPRequest{}, fmt.Errorf(`input field "path" is required (or pass "mkdir": true with "name" to mint one)`)
				}
				return request.BuildWorkspaceAdd(v[0], v[1], mkdir), nil
			}),
			"patch": act("PATCH /v1/workspaces/{id}", mcpInput{"id": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStr("id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				name, err := in.optStr("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				gitEnabled, err := in.optBool("git_enabled")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildWorkspacePatch(id, name, gitEnabled), nil
			}),
			"delete": confirmDel("DELETE /v1/workspaces/{id}", "id", "ws1", request.BuildWorkspaceDelete),
		},

		"assistant": {
			"show": act("GET /v1/workspaces/{id}/assistant", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				ws, err := in.reqStr("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildAssistantGetPath(ws)), nil
			}),
			"set": act("PUT /v1/workspaces/{id}/assistant", mcpInput{"workspace": "ws1", "content": "# TREBI\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildAssistantPut(v[0], v[1], v[2]), nil
			}),
			"patch": act("PATCH /v1/workspaces/{id}/assistant", mcpInput{"workspace": "ws1", "disabled": false}, func(in mcpInput) (request.HTTPRequest, error) {
				ws, err := in.reqStr("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				disabled, err := in.optBool("disabled")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				ifMatch, err := in.str("if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantPatch(ws, disabled, ifMatch), nil
			}),
			"delete": del("DELETE /v1/workspaces/{id}/assistant", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantDelete(v[0], v[1]), nil
			}),
			"wake": act("POST /v1/workspaces/{id}/assistant/wake", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				spec, err := in.runSpec()
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if spec.Workspace == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "workspace")
				}
				return spec.BuildWake(), nil
			}),
			"say": act("POST /v1/workspaces/{id}/assistant/messages", mcpInput{"workspace": "ws1", "text": "status?"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.reqStrs("workspace", "text")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				rest, err := in.strs("channel_type", "channel_ref", "idempotency_key")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				wait, _, err := in.num("wait_ms")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.AskSpec{
					Workspace: v[0], Text: v[1], ChannelType: rest[0], ChannelRef: rest[1],
					IdempotencyKey: rest[2], WaitMS: wait,
				}.Build(), nil
			}),
			"memory_show": act("GET /v1/workspaces/{id}/assistant/memory", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				ws, err := in.reqStr("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildAssistantMemoryGetPath(ws)), nil
			}),
			"memory_set": act("PUT /v1/workspaces/{id}/assistant/memory", mcpInput{"workspace": "ws1", "content": "# MEMORY\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildAssistantMemoryPut(v[0], v[1], v[2]), nil
			}),
			"memory_add": act("POST /v1/workspaces/{id}/assistant/memory/entries", mcpInput{"workspace": "ws1", "text": "learned the API"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "date", "text", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildAssistantMemoryEntryAdd(v[0], v[1], v[2], v[3]), nil
			}),
			"memory_delete": del("DELETE /v1/workspaces/{id}/assistant/memory/entries/{eid}", mcpInput{"workspace": "ws1", "entry_id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStrs("workspace", "entry_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				ifMatch, err := in.str("if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantMemoryEntryDelete(id[0], id[1], ifMatch), nil
			}),
			"memory_clear": del("DELETE /v1/workspaces/{id}/assistant/memory", mcpInput{"workspace": "ws1", "confirm": true}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantMemoryClear(v[0], v[1], confirm), nil
			}),
		},

		"file": {
			"list": fileEntry(mcpInput{"owner": "workspace", "workspace": "ws1", "prefix": "reports/"}, mcpInput.buildFileList, "GET /v1/workspaces/{id}/files", "GET /v1/jobs/{name}/files", "GET /v1/skills/{name}/files"),
			"get": func() mcpAction {
				e := fileEntry(
					mcpInput{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md"},
					mcpInput.buildFileGet,
					"GET /v1/workspaces/{id}/files/{path...}", "GET /v1/jobs/{name}/files/{path...}", "GET /v1/skills/{name}/files/{path...}",
				)
				e.transform = fileGetTransform
				return bound(e, false)
			}(),
			"put": fileEntry(
				mcpInput{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md", "content": "# hi\n"},
				mcpInput.buildFilePut,
				"PUT /v1/workspaces/{id}/files/{path...}", "PUT /v1/jobs/{name}/files/{path...}", "PUT /v1/skills/{name}/files/{path...}",
			),
			"delete": func() mcpAction {
				e := fileEntry(
					mcpInput{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md", "confirm": true},
					mcpInput.buildFileDelete,
					"DELETE /v1/workspaces/{id}/files/{path...}", "DELETE /v1/jobs/{name}/files/{path...}", "DELETE /v1/skills/{name}/files/{path...}",
				)
				e.destructive = true
				return e
			}(),
		},

		"harness": {
			"list": act("GET /v1/harnesses", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildHarnessesListPath()), nil
			}),
			"patch": act("PATCH /v1/harnesses/{name}", mcpInput{"name": "claude", "auth_mode": "auto"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "auth_mode")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildHarnessPatch(v[0], v[1]), nil
			}),
			"ensure": idPost("POST /v1/harnesses/{name}/ensure", "name", "claude", request.BuildHarnessEnsure),
			"auth": act("GET /v1/harnesses/{name}/auth", mcpInput{"name": "claude"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				live, err := in.flag("live")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildHarnessAuthGet(v[0], live), nil
			}),
			"login_start": act("POST /v1/harnesses/{name}/login", mcpInput{"name": "claude", "mode": "uri"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "mode")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildHarnessLoginStart(v[0], v[1]), nil
			}),
			"login_token": act("POST /v1/harnesses/{name}/login", mcpInput{"name": "claude", "token": "sk-..."}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "token")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "token")
				}
				return request.BuildHarnessLoginToken(v[0], v[1]), nil
			}),
			"login_code": act("POST /v1/harnesses/{name}/login", mcpInput{"name": "claude", "code": "1234"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "code")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "code")
				}
				return request.BuildHarnessLoginCode(v[0], v[1]), nil
			}),
			"login_status": getReq("GET /v1/harnesses/{name}/login", "name", "claude", request.BuildHarnessLoginGet),
			"login_cancel": idDel("DELETE /v1/harnesses/{name}/login", "name", "claude", request.BuildHarnessLoginCancel),
			"logout": act("POST /v1/harnesses/{name}/logout", mcpInput{"name": "claude"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "mode")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildHarnessLogout(v[0], v[1], confirm), nil
			}),
		},

		"integration": {
			"list": act("GET /v1/integrations", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildIntegrationsListPath(limit)), nil
			}),
			"show": idGet("GET /v1/integrations/{name}", "name", "mail", request.BuildIntegrationGetPath),
			"create": act("POST /v1/integrations", mcpInput{"definition": map[string]any{"name": "mail", "command": "fetch-mail.sh"}}, func(in mcpInput) (request.HTTPRequest, error) {
				def, err := in.obj("definition")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if def == nil {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "definition")
				}
				return request.BuildIntegrationCreate(def), nil
			}),
			"set": act("PUT /v1/integrations/{name}", mcpInput{"name": "mail", "command": "fetch-mail.sh"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "command", "format", "type", "cwd")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				env, err := in.strMap("env")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				facets, err := in.strList("facets")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.IntegrationSpec{
					Name: v[0], Command: v[1], Format: v[2], Type: v[3], Cwd: v[4],
					Env: env, Facets: facets,
				}.Build(), nil
			}),
			"delete":     confirmDel("DELETE /v1/integrations/{name}", "name", "mail", request.BuildIntegrationDelete),
			"connect":    idPost("POST /v1/integrations/{name}/connect", "name", "mail", request.BuildIntegrationConnect),
			"disconnect": idPost("POST /v1/integrations/{name}/disconnect", "name", "mail", request.BuildIntegrationDisconnect),
			"restart":    idPost("POST /v1/integrations/{name}/restart", "name", "mail", request.BuildIntegrationRestart),
			"test": act("POST /v1/integrations/{name}/test", mcpInput{"name": "mail", "event": map[string]any{"kind": "mail"}}, func(in mcpInput) (request.HTTPRequest, error) {
				name, err := in.reqStr("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildIntegrationTest(name, in.value("event")), nil
			}),
			"logs": func() mcpAction {
				e := act("GET /v1/integrations/{name}/logs", mcpInput{"name": "mail"}, func(in mcpInput) (request.HTTPRequest, error) {
					name, err := in.reqStr("name")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					return mcpGet(request.BuildIntegrationLogsPath(name, false)), nil // follow is not available over MCP
				})
				e.transform = logLinesTransform
				return bound(e, true)
			}(),
			"cursor_show": getReq("GET /v1/integrations/{name}/cursor", "name", "mail", request.BuildIntegrationCursorGet),
			"cursor_set": act("PUT /v1/integrations/{name}/cursor", mcpInput{"name": "mail", "last_event_id": "42"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "last_event_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "last_event_id")
				}
				return request.BuildIntegrationCursorSet(v[0], v[1]), nil
			}),
			"cursor_reset": idPost("PUT /v1/integrations/{name}/cursor", "name", "mail", request.BuildIntegrationCursorReset),
		},

		"event": {
			"list": act("GET /v1/events", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				cursor, err := in.str("cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				v, err := in.strs("integration", "since", "q")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildEventsListPath(cursor, limit, v[0], v[1], v[2])), nil
			}),
			"show":      getReq("GET /v1/events/{id}", "id", "01ABC", request.BuildEventGet),
			"retrigger": idPost("POST /v1/events/{id}/retrigger", "id", "01ABC", request.BuildEventsRetrigger),
			"rules_test": act("POST /v1/rules/test", mcpInput{"match": "kind = 'mail'", "event": map[string]any{"kind": "mail"}}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("match", "extract")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRulesTest(v[0], v[1], in.value("event")), nil
			}),
			"rules_fire": act("POST /v1/rules/fire", mcpInput{"integration": "mail", "job": "nightly-sync", "match": "kind = 'mail'"}, func(in mcpInput) (request.HTTPRequest, error) {
				req, err := in.reqStrs("integration", "job", "match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				v, err := in.strs("workspace", "extract")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				onIndex, _, err := in.num("on_index")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRulesFire(req[0], v[0], req[1], req[2], v[1], in.value("event"), onIndex), nil
			}),
		},

		"queue": {
			"list": act("GET /v1/queue", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.strs("status", "cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildQueueListPath(v[0], v[1], limit)), nil
			}),
			"requeue": idPost("POST /v1/queue/{id}/requeue", "id", "01ABC", request.BuildQueueRequeue),
			"cancel": bulkAct("POST /v1/queue/{id}/cancel", "POST /v1/queue/cancel", "01ABC",
				request.BuildQueueCancel, request.BuildQueueCancelBulk),
		},

		"inbox": {
			"list": act("GET /v1/inbox", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.strs("status", "kind", "cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildInboxListPath(v[0], v[1], v[2], limit)), nil
			}),
			"show":     getReq("GET /v1/inbox/{id}", "id", "01ABC", request.BuildInboxGet),
			"read":     idPost("POST /v1/inbox/{id}/read", "id", "01ABC", request.BuildInboxRead),
			"read_all": act("POST /v1/inbox/read-all", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildInboxReadAll(), nil }),
			"resolve":  idPost("POST /v1/inbox/{id}/resolve", "id", "01ABC", request.BuildInboxResolve),
			"resolve_all": act("POST /v1/inbox/resolve-all", mcpInput{"kind": "approval"}, func(in mcpInput) (request.HTTPRequest, error) {
				kind, err := in.str("kind")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildInboxResolveAll(kind), nil
			}),
			"alerts": act("GET /v1/alerts", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				since, err := in.str("since")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildAlertsPath(since, limit)), nil
			}),
		},

		"conversation": {
			"list": act("GET /v1/conversations", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.strs("workspace", "channel", "cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildConversationsListPath(v[0], v[1], v[2], limit)), nil
			}),
			"show": getReq("GET /v1/conversations/{id}", "id", "01ABC", request.BuildConversationGet),
			"messages": act("GET /v1/conversations/{id}/messages", mcpInput{"id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("id", "cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildConversationMessagesPath(v[0], v[1], limit)), nil
			}),
			"create": actSamples("POST /v1/conversations",
				mcpInput{"workspace": "ws1", "title": "sync check"},
				[]mcpInput{{"workspace": "ws1", "channel_type": "cli", "channel_ref": "fritz"}},
				func(in mcpInput) (request.HTTPRequest, error) {
					v, err := in.head("workspace", "title", "origin_channel", "channel_type", "channel_ref")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					if v[2] == "" {
						v[2] = "mcp" // the service rejects an empty origin channel
					}
					return request.BuildConversationCreate(v[0], v[1], v[2], v[3], v[4]), nil
				}),
			"patch": act("PATCH /v1/conversations/{id}", mcpInput{"id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStr("id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				title, err := in.presentStr("title")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				summary, err := in.presentStr("summary")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				revision, _, err := in.num("revision")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildConversationPatch(id, title, summary, int64(revision)), nil
			}),
			"send": act("POST /v1/conversations/{id}/messages", mcpInput{"id": "01ABC", "text": "run the sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("id", "text", "channel_type", "channel_ref", "idempotency_key")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildConversationSend(v[0], v[1], v[2], v[3], v[4]), nil
			}),
			"pointer_show": act("GET /v1/channel-pointers/{type}/{ref}", mcpInput{"channel_type": "cli", "channel_ref": "fritz"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.reqStrs("channel_type", "channel_ref")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildPointerGet(v[0], v[1]), nil
			}),
			"pointer_set": act("PUT /v1/channel-pointers/{type}/{ref}", mcpInput{"channel_type": "cli", "channel_ref": "fritz", "conversation_id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.reqStrs("channel_type", "channel_ref", "conversation_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildPointerPut(v[0], v[1], v[2]), nil
			}),
		},

		"skill": {
			"list": act("GET /v1/skills", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildSkillsListPath(limit)), nil
			}),
			"show": idGet("GET /v1/skills/{name}", "name", "deploy", request.BuildSkillGetPath),
			"create": actSamples("POST /v1/skills",
				mcpInput{"name": "deploy", "content": "# SKILL.md\n", "provenance": "local"},
				[]mcpInput{
					{"name": "deploy", "source_git": "https://example.com/deploy.git"},
					{"name": "deploy", "source_folder": "/tmp/deploy"},
				},
				func(in mcpInput) (request.HTTPRequest, error) {
					v, err := in.head("name", "content", "provenance", "source_git", "source_folder")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					spec := request.SkillSpec{
						Name: v[0], SkillMD: v[1], Provenance: v[2],
						SourceGit: v[3], SourceFolder: v[4],
					}
					if spec.SkillMD == "" && spec.SourceGit == "" && spec.SourceFolder == "" {
						return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
					}
					return spec.Build(), nil
				}),
			"set": act("PUT /v1/skills/{name}", mcpInput{"name": "deploy", "content": "# SKILL.md\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				harnesses, err := in.strMap("harnesses")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildSkillPut(v[0], v[1], harnesses, v[2]), nil
			}),
			"refresh": idPost("POST /v1/skills/{name}/refresh", "name", "deploy", request.BuildSkillRefresh),
			"delete":  confirmDel("DELETE /v1/skills/{name}", "name", "deploy", request.BuildSkillDelete),
		},

		"secret": {
			"list": act("GET /v1/secrets", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildSecretsListPath(limit)), nil
			}),
			// value_file is read by the local `trebi mcp serve` bridge, which
			// replaces it with value. It keeps the secret out of the client
			// transcript, the way the CLI --from-file does.
			"set": act("PUT /v1/secrets/{name}", mcpInput{"name": "API_KEY", "value": "s3cr3t", "value_file": "./api-key.txt"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "value")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required (or value_file through `trebi mcp serve`)", "value")
				}
				return request.BuildSecretPut(v[0], v[1]), nil
			}),
			"delete": confirmDel("DELETE /v1/secrets/{name}", "name", "API_KEY", request.BuildSecretDelete),
		},

		"config": {
			"show": act("GET /v1/config", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildConfigGet(), nil }),
			"set": act("PATCH /v1/config", mcpInput{"key": "daemon.log_level", "value": "debug"}, func(in mcpInput) (request.HTTPRequest, error) {
				values, err := in.obj("values")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if len(values) > 0 {
					return request.BuildConfigSetValues(values), nil
				}
				key, err := in.reqStr("key")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				value := in.value("value")
				if value == nil {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "value")
				}
				return request.BuildConfigSetValues(map[string]any{key: value}), nil
			}),
			"validate": act("GET /v1/config/validate", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildConfigValidatePath()), nil
			}),
		},

		"system": {
			"health": act("GET /v1/system/health", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemHealthPath()), nil
			}),
			"doctor": act("GET /v1/system/doctor", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemDoctorPath()), nil
			}),
			"doctor_fix": idPost("POST /v1/system/doctor/{finding_id}/fix", "finding_id", "stale-api-json", request.BuildDoctorFix),
			"logs": bound(act("GET /v1/system/logs", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemLogsPath()), nil
			}), true),
			"version": act("GET /v1/system/update", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemUpdatePath()), nil
			}),
			"drain":   act("POST /v1/system/drain", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildSystemDrain(), nil }),
			"undrain": act("POST /v1/system/undrain", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildSystemUndrain(), nil }),
			"shutdown": del("POST /v1/system/shutdown", mcpInput{"confirm": true}, func(in mcpInput) (request.HTTPRequest, error) {
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildServiceShutdown(confirm), nil
			}),
			"overview": act("GET /v1/system/overview", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemOverviewPath()), nil
			}),
			"cloud_status": act("GET /v1/cloud/status", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildCloudStatusPath()), nil
			}),
			"cloud_link": act("POST /v1/cloud/link", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildCloudLink(), nil }),
			"cloud_unlink": del("DELETE /v1/cloud/link", mcpInput{"confirm": true}, func(in mcpInput) (request.HTTPRequest, error) {
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildCloudUnlink(confirm), nil
			}),
			"bus_poll": func() mcpAction {
				e := act("GET /v1/stream", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
					topics, err := in.str("topics")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					cursor, err := in.scalarStr("cursor")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					return mcpGet(request.BuildStreamDumpPath(topics, cursor)), nil
				})
				e.transform = busPollTransform
				return e
			}(),
			"guides": act("GET /v1/system/guides", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemGuidesPath()), nil
			}),
			"guide": act("GET /v1/system/guides/{topic}", mcpInput{"topic": "getting-started"}, func(in mcpInput) (request.HTTPRequest, error) {
				topic, err := in.reqStr("topic")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildSystemGuidePath(topic)), nil
			}),
		},
	}
}

// jobPatchFields are the flat fields job.patch accepts. They mirror the keys
// job.MergePatchFrontmatter honors; an unknown key is a validation error.
var jobPatchFields = []string{ //nolint:gochecknoglobals // static field list, never mutated
	"disabled", "harness", "title", "description", "working_dir", "envset", "followup_prompt", "session",
}

// jobPauseAction adapts the shared pause/unpause builder.
func jobPauseAction(pause bool) func(mcpInput) (request.HTTPRequest, error) {
	return func(in mcpInput) (request.HTTPRequest, error) {
		v, err := in.head("name", "workspace", "if_match")
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return request.BuildJobPause(v[0], v[1], v[2], pause), nil
	}
}
