// Package tools is the transport-neutral trebi tool catalog. /mcp, the CLI,
// and the native assistant loop all call the same actions; none of them own
// a private copy of the table.
package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/trebi-ai/trebi/internal/request"
)

// Input is the flat input object of one tool call: path params, query
// params, and body fields together in one map.
type Input map[string]any

// str reads one optional string field. An absent field reads as "".
func (in Input) str(key string) (string, error) {
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
func (in Input) strs(keys ...string) ([]string, error) {
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
func (in Input) reqStr(key string) (string, error) {
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
func (in Input) reqStrs(keys ...string) ([]string, error) {
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
func (in Input) head(req string, opts ...string) ([]string, error) {
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
func (in Input) optStr(key string) (*string, error) {
	s, err := in.str(key)
	if err != nil || s == "" {
		return nil, err
	}
	return &s, nil
}

// presentStr reads one string field with presence: nil only when the key is
// absent. An explicit "" clears the field it patches.
func (in Input) presentStr(key string) (*string, error) {
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
func (in Input) num(key string) (n int, ok bool, err error) {
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
func (in Input) flag(key string) (bool, error) {
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
func (in Input) optBool(key string) (*bool, error) {
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
func (in Input) obj(key string) (map[string]any, error) {
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
func (in Input) strMap(key string) (map[string]string, error) {
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
func (in Input) strList(key string) ([]string, error) {
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
func (in Input) pick(keys ...string) map[string]any {
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
func (in Input) scalarStr(key string) (string, error) {
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
func (in Input) value(key string) any { return in[key] }

// owner reads the file-plane owner selector: workspace (default), job, or skill.
func (in Input) owner() (string, error) {
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
func (in Input) buildFileList() (request.HTTPRequest, error) {
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
func (in Input) buildFileGet() (request.HTTPRequest, error) {
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
func (in Input) buildFilePut() (request.HTTPRequest, error) {
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
func (in Input) decodeContent(content string) ([]byte, error) {
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
func (in Input) buildFileDelete() (request.HTTPRequest, error) {
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
func (in Input) jobSpec() (request.JobSpec, error) {
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
func (in Input) runSpec() (request.RunSpec, error) {
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

// Catalog is the tool → action table. One action per CLI verb.
type Catalog map[string]map[string]Action

// Action maps one tool action onto /v1 routes.
type Action struct {
	Routes      []string                                                // registered patterns this action can hit
	Sample      Input                                                   // valid example input (docs, parity checks)
	Samples     []Input                                                 // extra examples; with sample they cover every route
	Build       func(in Input) (request.HTTPRequest, error)             // input → pure request
	Transform   func(in Input, status int, body []byte) ([]byte, error) // optional response rewrite
	Destructive bool                                                    // sets the MCP destructiveHint annotation
	Bounded     bool                                                    // honors max_bytes; a long body arrives truncated
	Tail        bool                                                    // bounded from the end, not the start (logs)
}

// bound marks one entry as size-bounded. tail keeps the end of the body.
func bound(e Action, tail bool) Action {
	e.Bounded, e.Tail = true, tail
	return e
}

// act starts one catalog entry on a single route.
func act(route string, sample Input, build func(Input) (request.HTTPRequest, error)) Action {
	return Action{Routes: []string{route}, Sample: sample, Build: build}
}

// actSamples is act with extra examples, for a field pair that only changes
// the request when both halves are given.
func actSamples(route string, sample Input, extra []Input, build func(Input) (request.HTTPRequest, error)) Action {
	e := act(route, sample, build)
	e.Samples = extra
	return e
}

// multiAct starts one entry that spans several patterns (owner-scoped routes).
func multiAct(sample Input, build func(Input) (request.HTTPRequest, error), routes ...string) Action {
	return Action{Routes: routes, Sample: sample, Build: build}
}

// del is act plus the destructive annotation.
func del(route string, sample Input, build func(Input) (request.HTTPRequest, error)) Action {
	e := act(route, sample, build)
	e.Destructive = true
	return e
}

// fileEntry builds one file-plane entry. The owner-variant samples make the
// parity check cover all three owner routes.
func fileEntry(sample Input, build func(Input) (request.HTTPRequest, error), routes ...string) Action {
	e := multiAct(sample, build, routes...)
	e.Samples = []Input{
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
func idGet(route, key, sampleVal string, path func(string) string) Action {
	return act(route, Input{key: sampleVal}, func(in Input) (request.HTTPRequest, error) {
		id, err := in.reqStr(key)
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return mcpGet(path(id)), nil
	})
}

// getReq adapts one required field to a GET request builder.
func getReq(route, key, sampleVal string, build func(string) request.HTTPRequest) Action {
	return act(route, Input{key: sampleVal}, func(in Input) (request.HTTPRequest, error) {
		id, err := in.reqStr(key)
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return build(id), nil
	})
}

// idPost adapts one required field to a POST request builder.
func idPost(route, key, sampleVal string, build func(string) request.HTTPRequest) Action {
	return act(route, Input{key: sampleVal}, func(in Input) (request.HTTPRequest, error) {
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
	many func([]string) request.HTTPRequest) Action {
	e := actSamples(route, Input{"id": sampleVal}, []Input{{"ids": []any{sampleVal}}},
		func(in Input) (request.HTTPRequest, error) {
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
	e.Routes = []string{route, bulkRoute}
	return e
}

// idDel is idPost plus the destructive annotation.
func idDel(route, key, sampleVal string, build func(string) request.HTTPRequest) Action {
	e := idPost(route, key, sampleVal, build)
	e.Destructive = true
	return e
}

// confirmDel adapts one required field plus the confirm gate to a destructive
// request builder. The API answers `confirm_required` without it.
func confirmDel(route, key, sampleVal string, build func(string, bool) request.HTTPRequest) Action {
	e := act(route, Input{key: sampleVal, "confirm": true}, func(in Input) (request.HTTPRequest, error) {
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
	e.Destructive = true
	return e
}

// NewCatalog builds the tool→action table. One action per CLI verb; every
// entry names the /v1 pattern it hits so the parity test can check coverage.
func NewCatalog() Catalog { //nolint:gocyclo,funlen // static action table, one literal per action
	return Catalog{
		"run": {
			"list": act("GET /v1/runs", Input{}, func(in Input) (request.HTTPRequest, error) {
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
			"show": act("GET /v1/runs/{id}", Input{"id": "01ABC"}, func(in Input) (request.HTTPRequest, error) {
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
			"transcript": bound(act("GET /v1/runs/{id}/transcript", Input{"id": "01ABC"}, func(in Input) (request.HTTPRequest, error) {
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
			"kill": func() Action {
				e := bulkAct("POST /v1/runs/{id}/kill", "POST /v1/runs/kill", "01ABC",
					request.BuildRunsKill, request.BuildRunsKillBulk)
				e.Destructive = true
				return e
			}(),
			"resume": idPost("POST /v1/runs/{id}/resume", "id", "01ABC", request.BuildRunResume),
			"replay": idPost("POST /v1/runs/{id}/replay", "id", "01ABC", request.BuildRunReplay),
			"retry":  idPost("POST /v1/runs/{id}/retry", "id", "01ABC", request.BuildRunsRetry),
			"message": act("POST /v1/runs/{id}/message", Input{"id": "01ABC", "text": "ship it"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.reqStrs("id", "text")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunMessage(v[0], v[1]), nil
			}),
			"approve": act("POST /v1/runs/{id}/approve", Input{"id": "01ABC", "decision": "allow"}, func(in Input) (request.HTTPRequest, error) {
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
			"approvals": act("GET /v1/approvals", Input{}, func(in Input) (request.HTTPRequest, error) {
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
			"stats": act("GET /v1/stats/runs", Input{"range": "mtd"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.strs("workspace", "job", "range")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunsStats(v[0], v[1], v[2]), nil
			}),
			"agents": act("GET /v1/agents/state", Input{}, func(Input) (request.HTTPRequest, error) { return request.BuildAgentsState(), nil }),
		},

		"job": {
			"list": act("GET /v1/jobs", Input{}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.strs("workspace", "q", "harness", "active")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobsListPath(v[0], v[1], v[2], v[3])), nil
			}),
			"show": act("GET /v1/jobs/{name}", Input{"name": "nightly-sync"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobGetPath(v[0], v[1])), nil
			}),
			"create": actSamples("POST /v1/jobs",
				Input{"name": "nightly-sync", "workspace": "ws1", "prompt": "Run the sync.", "cron": "0 3 * * *"},
				[]Input{{"name": "on-mail", "workspace": "ws1", "prompt": "Read it.", "event": "mail"}},
				func(in Input) (request.HTTPRequest, error) {
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
				Input{"prompt": "Run the sync.", "cron": "0 3 * * *"},
				[]Input{{"prompt": "Read it.", "event": "mail"}},
				func(in Input) (request.HTTPRequest, error) {
					spec, err := in.jobSpec()
					if err != nil {
						return request.HTTPRequest{}, err
					}
					return spec.BuildValidate(), nil
				}),
			"set": act("PUT /v1/jobs/{name}", Input{"name": "nightly-sync", "content": "---\nharness: claude\n---\n\nRun the sync.\n"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildJobPutMarkdown(v[0], v[1], v[2], v[3]), nil
			}),
			"patch": act("PATCH /v1/jobs/{name}", Input{"name": "nightly-sync", "disabled": true}, func(in Input) (request.HTTPRequest, error) {
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
			"delete": del("DELETE /v1/jobs/{name}", Input{"name": "nightly-sync", "confirm": true}, func(in Input) (request.HTTPRequest, error) {
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
			"run": act("POST /v1/jobs/{name}/run", Input{"name": "nightly-sync"}, func(in Input) (request.HTTPRequest, error) {
				spec, err := in.runSpec()
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if spec.Job == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
				}
				return spec.Build(), nil
			}),
			"pause":   act("POST /v1/jobs/{name}/pause", Input{"name": "nightly-sync"}, jobPauseAction(true)),
			"unpause": act("POST /v1/jobs/{name}/unpause", Input{"name": "nightly-sync"}, jobPauseAction(false)),
			"validate": act("POST /v1/jobs/{name}/validate", Input{"name": "nightly-sync"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpPost(request.BuildJobValidatePath(v[0], v[1])), nil
			}),
			"schedules": act("GET /v1/schedules", Input{}, func(in Input) (request.HTTPRequest, error) {
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
			"memory_show": act("GET /v1/jobs/{name}/memory", Input{"name": "nightly-sync"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobMemoryGetPath(v[0], v[1])), nil
			}),
			"memory_set": act("PUT /v1/jobs/{name}/memory", Input{"name": "nightly-sync", "content": "# MEMORY\n"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildJobMemoryPut(v[0], v[1], v[2], v[3]), nil
			}),
			"memory_add": act("POST /v1/jobs/{name}/memory/entries", Input{"name": "nightly-sync", "text": "learned the API"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "date", "text", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[3] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildJobMemoryEntryAdd(v[0], v[1], v[2], v[3], v[4]), nil
			}),
			"memory_delete": del("DELETE /v1/jobs/{name}/memory/entries/{id}", Input{"name": "nightly-sync", "entry_id": "01ABC"}, func(in Input) (request.HTTPRequest, error) {
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
			"memory_clear": del("DELETE /v1/jobs/{name}/memory", Input{"name": "nightly-sync", "confirm": true}, func(in Input) (request.HTTPRequest, error) {
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
			"list": act("GET /v1/workspaces", Input{}, func(in Input) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildWorkspacesListPath(limit)), nil
			}),
			"show": idGet("GET /v1/workspaces/{id}", "id", "ws1", request.BuildWorkspaceGetPath),
			"create": act("POST /v1/workspaces", Input{"path": "/tmp/reports", "name": "reports"}, func(in Input) (request.HTTPRequest, error) {
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
			"patch": act("PATCH /v1/workspaces/{id}", Input{"id": "ws1"}, func(in Input) (request.HTTPRequest, error) {
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
			"show": act("GET /v1/workspaces/{id}/assistant", Input{"workspace": "ws1"}, func(in Input) (request.HTTPRequest, error) {
				ws, err := in.reqStr("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildAssistantGetPath(ws)), nil
			}),
			"set": act("PUT /v1/workspaces/{id}/assistant", Input{"workspace": "ws1", "content": "# TREBI\n"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildAssistantPut(v[0], v[1], v[2]), nil
			}),
			"patch": act("PATCH /v1/workspaces/{id}/assistant", Input{"workspace": "ws1", "disabled": false}, func(in Input) (request.HTTPRequest, error) {
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
			"delete": del("DELETE /v1/workspaces/{id}/assistant", Input{"workspace": "ws1"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantDelete(v[0], v[1]), nil
			}),
			"wake": act("POST /v1/workspaces/{id}/assistant/wake", Input{"workspace": "ws1"}, func(in Input) (request.HTTPRequest, error) {
				spec, err := in.runSpec()
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if spec.Workspace == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "workspace")
				}
				return spec.BuildWake(), nil
			}),
			"say": act("POST /v1/workspaces/{id}/assistant/messages", Input{"workspace": "ws1", "text": "status?"}, func(in Input) (request.HTTPRequest, error) {
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
			"memory_show": act("GET /v1/workspaces/{id}/assistant/memory", Input{"workspace": "ws1"}, func(in Input) (request.HTTPRequest, error) {
				ws, err := in.reqStr("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildAssistantMemoryGetPath(ws)), nil
			}),
			"memory_set": act("PUT /v1/workspaces/{id}/assistant/memory", Input{"workspace": "ws1", "content": "# MEMORY\n"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "content", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildAssistantMemoryPut(v[0], v[1], v[2]), nil
			}),
			"memory_add": act("POST /v1/workspaces/{id}/assistant/memory/entries", Input{"workspace": "ws1", "text": "learned the API"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "date", "text", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildAssistantMemoryEntryAdd(v[0], v[1], v[2], v[3]), nil
			}),
			"memory_delete": del("DELETE /v1/workspaces/{id}/assistant/memory/entries/{eid}", Input{"workspace": "ws1", "entry_id": "01ABC"}, func(in Input) (request.HTTPRequest, error) {
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
			"memory_clear": del("DELETE /v1/workspaces/{id}/assistant/memory", Input{"workspace": "ws1", "confirm": true}, func(in Input) (request.HTTPRequest, error) {
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
			"list": fileEntry(Input{"owner": "workspace", "workspace": "ws1", "prefix": "reports/"}, Input.buildFileList, "GET /v1/workspaces/{id}/files", "GET /v1/jobs/{name}/files", "GET /v1/skills/{name}/files"),
			"get": func() Action {
				e := fileEntry(
					Input{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md"},
					Input.buildFileGet,
					"GET /v1/workspaces/{id}/files/{path...}", "GET /v1/jobs/{name}/files/{path...}", "GET /v1/skills/{name}/files/{path...}",
				)
				e.Transform = fileGetTransform
				return bound(e, false)
			}(),
			"put": fileEntry(
				Input{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md", "content": "# hi\n"},
				Input.buildFilePut,
				"PUT /v1/workspaces/{id}/files/{path...}", "PUT /v1/jobs/{name}/files/{path...}", "PUT /v1/skills/{name}/files/{path...}",
			),
			"delete": func() Action {
				e := fileEntry(
					Input{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md", "confirm": true},
					Input.buildFileDelete,
					"DELETE /v1/workspaces/{id}/files/{path...}", "DELETE /v1/jobs/{name}/files/{path...}", "DELETE /v1/skills/{name}/files/{path...}",
				)
				e.Destructive = true
				return e
			}(),
		},

		"harness": {
			"list": act("GET /v1/harnesses", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildHarnessesListPath()), nil
			}),
			"patch": act("PATCH /v1/harnesses/{name}", Input{"name": "claude", "auth_mode": "auto"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "auth_mode")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildHarnessPatch(v[0], v[1]), nil
			}),
			"ensure": idPost("POST /v1/harnesses/{name}/ensure", "name", "claude", request.BuildHarnessEnsure),
			"auth": act("GET /v1/harnesses/{name}/auth", Input{"name": "claude"}, func(in Input) (request.HTTPRequest, error) {
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
			"login_start": act("POST /v1/harnesses/{name}/login", Input{"name": "claude", "mode": "uri"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "mode")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildHarnessLoginStart(v[0], v[1]), nil
			}),
			"login_token": act("POST /v1/harnesses/{name}/login", Input{"name": "claude", "token": "sk-..."}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "token")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "token")
				}
				return request.BuildHarnessLoginToken(v[0], v[1]), nil
			}),
			"login_code": act("POST /v1/harnesses/{name}/login", Input{"name": "claude", "code": "1234"}, func(in Input) (request.HTTPRequest, error) {
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
			"logout": act("POST /v1/harnesses/{name}/logout", Input{"name": "claude"}, func(in Input) (request.HTTPRequest, error) {
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

		"connector": {
			"list": act("GET /v1/connectors", Input{}, func(in Input) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildConnectorsListPath(limit)), nil
			}),
			"show": idGet("GET /v1/connectors/{name}", "name", "mail", request.BuildConnectorGetPath),
			"create": act("POST /v1/connectors", Input{"definition": map[string]any{"name": "mail", "command": "fetch-mail.sh"}}, func(in Input) (request.HTTPRequest, error) {
				def, err := in.obj("definition")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if def == nil {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "definition")
				}
				return request.BuildConnectorCreate(def), nil
			}),
			"set": act("PUT /v1/connectors/{name}", Input{"name": "mail", "command": "fetch-mail.sh"}, func(in Input) (request.HTTPRequest, error) {
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
				return request.ConnectorSpec{
					Name: v[0], Command: v[1], Format: v[2], Type: v[3], Cwd: v[4],
					Env: env, Facets: facets,
				}.Build(), nil
			}),
			"delete":     confirmDel("DELETE /v1/connectors/{name}", "name", "mail", request.BuildConnectorDelete),
			"connect":    idPost("POST /v1/connectors/{name}/connect", "name", "mail", request.BuildConnectorConnect),
			"disconnect": idPost("POST /v1/connectors/{name}/disconnect", "name", "mail", request.BuildConnectorDisconnect),
			"restart":    idPost("POST /v1/connectors/{name}/restart", "name", "mail", request.BuildConnectorRestart),
			"test": act("POST /v1/connectors/{name}/test", Input{"name": "mail", "event": map[string]any{"kind": "mail"}}, func(in Input) (request.HTTPRequest, error) {
				name, err := in.reqStr("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildConnectorTest(name, in.value("event")), nil
			}),
			"logs": func() Action {
				e := act("GET /v1/connectors/{name}/logs", Input{"name": "mail"}, func(in Input) (request.HTTPRequest, error) {
					name, err := in.reqStr("name")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					return mcpGet(request.BuildConnectorLogsPath(name, false)), nil // follow is not available over MCP
				})
				e.Transform = logLinesTransform
				return bound(e, true)
			}(),
			"cursor_show": getReq("GET /v1/connectors/{name}/cursor", "name", "mail", request.BuildConnectorCursorGet),
			"cursor_set": act("PUT /v1/connectors/{name}/cursor", Input{"name": "mail", "last_event_id": "42"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("name", "last_event_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "last_event_id")
				}
				return request.BuildConnectorCursorSet(v[0], v[1]), nil
			}),
			"cursor_reset": idPost("PUT /v1/connectors/{name}/cursor", "name", "mail", request.BuildConnectorCursorReset),
		},

		"event": {
			"list": act("GET /v1/events", Input{}, func(in Input) (request.HTTPRequest, error) {
				cursor, err := in.str("cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				v, err := in.strs("connector", "since", "q")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildEventsListPath(cursor, limit, v[0], v[1], v[2])), nil
			}),
			"show":      getReq("GET /v1/events/{id}", "id", "01ABC", request.BuildEventGet),
			"retrigger": idPost("POST /v1/events/{id}/retrigger", "id", "01ABC", request.BuildEventsRetrigger),
			"rules_test": act("POST /v1/rules/test", Input{"match": "kind = 'mail'", "event": map[string]any{"kind": "mail"}}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("match", "extract")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRulesTest(v[0], v[1], in.value("event")), nil
			}),
			"rules_fire": act("POST /v1/rules/fire", Input{"connector": "mail", "job": "nightly-sync", "match": "kind = 'mail'"}, func(in Input) (request.HTTPRequest, error) {
				req, err := in.reqStrs("connector", "job", "match")
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
			"list": act("GET /v1/queue", Input{}, func(in Input) (request.HTTPRequest, error) {
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
			"list": act("GET /v1/inbox", Input{}, func(in Input) (request.HTTPRequest, error) {
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
			"read_all": act("POST /v1/inbox/read-all", Input{}, func(Input) (request.HTTPRequest, error) { return request.BuildInboxReadAll(), nil }),
			"resolve":  idPost("POST /v1/inbox/{id}/resolve", "id", "01ABC", request.BuildInboxResolve),
			"resolve_all": act("POST /v1/inbox/resolve-all", Input{"kind": "approval"}, func(in Input) (request.HTTPRequest, error) {
				kind, err := in.str("kind")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildInboxResolveAll(kind), nil
			}),
			"alerts": act("GET /v1/alerts", Input{}, func(in Input) (request.HTTPRequest, error) {
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
			"list": act("GET /v1/conversations", Input{}, func(in Input) (request.HTTPRequest, error) {
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
			"messages": act("GET /v1/conversations/{id}/messages", Input{"id": "01ABC"}, func(in Input) (request.HTTPRequest, error) {
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
				Input{"workspace": "ws1", "title": "sync check"},
				[]Input{{"workspace": "ws1", "channel_type": "cli", "channel_ref": "fritz"}},
				func(in Input) (request.HTTPRequest, error) {
					v, err := in.head("workspace", "title", "origin_channel", "channel_type", "channel_ref")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					if v[2] == "" {
						v[2] = "mcp" // the service rejects an empty origin channel
					}
					return request.BuildConversationCreate(v[0], v[1], v[2], v[3], v[4]), nil
				}),
			"patch": act("PATCH /v1/conversations/{id}", Input{"id": "01ABC"}, func(in Input) (request.HTTPRequest, error) {
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
			"send": act("POST /v1/conversations/{id}/messages", Input{"id": "01ABC", "text": "run the sync"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.head("id", "text", "channel_type", "channel_ref", "idempotency_key")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildConversationSend(v[0], v[1], v[2], v[3], v[4]), nil
			}),
			"pointer_show": act("GET /v1/channel-pointers/{type}/{ref}", Input{"channel_type": "cli", "channel_ref": "fritz"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.reqStrs("channel_type", "channel_ref")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildPointerGet(v[0], v[1]), nil
			}),
			"pointer_set": act("PUT /v1/channel-pointers/{type}/{ref}", Input{"channel_type": "cli", "channel_ref": "fritz", "conversation_id": "01ABC"}, func(in Input) (request.HTTPRequest, error) {
				v, err := in.reqStrs("channel_type", "channel_ref", "conversation_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildPointerPut(v[0], v[1], v[2]), nil
			}),
		},

		"skill": {
			"list": act("GET /v1/skills", Input{}, func(in Input) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildSkillsListPath(limit)), nil
			}),
			"show": idGet("GET /v1/skills/{name}", "name", "deploy", request.BuildSkillGetPath),
			"create": actSamples("POST /v1/skills",
				Input{"name": "deploy", "content": "# SKILL.md\n", "provenance": "local"},
				[]Input{
					{"name": "deploy", "source_git": "https://example.com/deploy.git"},
					{"name": "deploy", "source_folder": "/tmp/deploy"},
				},
				func(in Input) (request.HTTPRequest, error) {
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
			"set": act("PUT /v1/skills/{name}", Input{"name": "deploy", "content": "# SKILL.md\n"}, func(in Input) (request.HTTPRequest, error) {
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
			"list": act("GET /v1/secrets", Input{}, func(in Input) (request.HTTPRequest, error) {
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildSecretsListPath(limit)), nil
			}),
			// value_file is resolved before dispatch (native registry and the
			// CLI `trebi mcp serve` bridge share ResolveValueFile). It keeps
			// the secret out of the client transcript, the way --from-file does.
			"set": act("PUT /v1/secrets/{name}", Input{"name": "API_KEY", "value": "s3cr3t", "value_file": "./api-key.txt"}, func(in Input) (request.HTTPRequest, error) {
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
			"show": act("GET /v1/config", Input{}, func(Input) (request.HTTPRequest, error) { return request.BuildConfigGet(), nil }),
			"set": act("PATCH /v1/config", Input{"key": "daemon.log_level", "value": "debug"}, func(in Input) (request.HTTPRequest, error) {
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
			"unset": act("PATCH /v1/config", Input{"key": "assistant.api_key"}, func(in Input) (request.HTTPRequest, error) {
				key, err := in.reqStr("key")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildConfigUnset(key), nil
			}),
			"validate": act("GET /v1/config/validate", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildConfigValidatePath()), nil
			}),
		},

		"system": {
			"health": act("GET /v1/system/health", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemHealthPath()), nil
			}),
			"doctor": act("GET /v1/system/doctor", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemDoctorPath()), nil
			}),
			"doctor_fix": idPost("POST /v1/system/doctor/{finding_id}/fix", "finding_id", "stale-api-json", request.BuildDoctorFix),
			"logs": bound(act("GET /v1/system/logs", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemLogsPath()), nil
			}), true),
			"version": act("GET /v1/system/update", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemUpdatePath()), nil
			}),
			"drain":   act("POST /v1/system/drain", Input{}, func(Input) (request.HTTPRequest, error) { return request.BuildSystemDrain(), nil }),
			"undrain": act("POST /v1/system/undrain", Input{}, func(Input) (request.HTTPRequest, error) { return request.BuildSystemUndrain(), nil }),
			"shutdown": del("POST /v1/system/shutdown", Input{"confirm": true}, func(in Input) (request.HTTPRequest, error) {
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildServiceShutdown(confirm), nil
			}),
			"overview": act("GET /v1/system/overview", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemOverviewPath()), nil
			}),
			"cloud_status": act("GET /v1/cloud/status", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildCloudStatusPath()), nil
			}),
			"cloud_link": act("POST /v1/cloud/link", Input{}, func(Input) (request.HTTPRequest, error) { return request.BuildCloudLink(), nil }),
			"cloud_unlink": del("DELETE /v1/cloud/link", Input{"confirm": true}, func(in Input) (request.HTTPRequest, error) {
				confirm, err := in.flag("confirm")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildCloudUnlink(confirm), nil
			}),
			"bus_poll": func() Action {
				e := act("GET /v1/stream", Input{}, func(in Input) (request.HTTPRequest, error) {
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
				e.Transform = busPollTransform
				return e
			}(),
			"guides": act("GET /v1/system/guides", Input{}, func(Input) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemGuidesPath()), nil
			}),
			"guide": act("GET /v1/system/guides/{topic}", Input{"topic": "getting-started"}, func(in Input) (request.HTTPRequest, error) {
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
func jobPauseAction(pause bool) func(Input) (request.HTTPRequest, error) {
	return func(in Input) (request.HTTPRequest, error) {
		v, err := in.head("name", "workspace", "if_match")
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return request.BuildJobPause(v[0], v[1], v[2], pause), nil
	}
}

// busPollTransform parses one /v1/stream SSE dump into a JSON object:
// {"events": […], "next_cursor": N}. The model carries next_cursor back on
// the next poll.
func busPollTransform(_ Input, _ int, body []byte) ([]byte, error) {
	events := []map[string]any{}
	next := int64(0)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // keepalive comments and blank separators
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			return nil, fmt.Errorf("parse stream event: %w", err)
		}
		events = append(events, ev)
		if seq, ok := ev["seq"].(float64); ok && int64(seq) > next {
			next = int64(seq)
		}
	}
	return json.Marshal(map[string]any{"events": events, "next_cursor": next})
}

// fileGetTransform wraps a non-text file body as base64 so a binary read is
// not mangled into replacement characters. Text passes through unchanged.
func fileGetTransform(_ Input, _ int, body []byte) ([]byte, error) {
	if utf8.Valid(body) && !bytes.ContainsRune(body, 0) {
		return body, nil
	}
	return json.Marshal(map[string]any{
		"content":  base64.StdEncoding.EncodeToString(body),
		"encoding": "base64",
	})
}

// sseLines pulls the `data:` payloads out of one SSE dump, newest last.
func sseLines(body []byte) []string {
	out := []string{}
	for _, line := range strings.Split(string(body), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue // keepalive comments and blank separators
		}
		if text := strings.TrimSpace(data); text != "" && text != "{}" {
			out = append(out, text)
		}
	}
	return out
}

// logLinesTransform rewrites one SSE log dump into {"lines": [...]}. Without
// it the model reads raw `data: {...}` text. The `lines` input bounds the
// tail; absent it keeps everything the handler sent.
func logLinesTransform(in Input, _ int, body []byte) ([]byte, error) {
	lines := sseLines(body)
	n, ok, err := in.num("lines")
	if err != nil {
		return nil, err
	}
	res := map[string]any{"lines": lines}
	if ok && n > 0 && len(lines) > n {
		res["lines"] = lines[len(lines)-n:]
		res["truncated"] = true
	}
	return json.Marshal(res)
}
