package api

import (
	"fmt"
	"net/http"
	"strconv"

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

// value reads one field as a raw JSON value: nil when the field is absent.
func (in mcpInput) value(key string) any { return in[key] }

// mcpAction maps one tool action onto /v1 routes.
type mcpAction struct {
	routes      []string                                       // registered patterns this action can hit
	sample      mcpInput                                       // valid example input (docs, parity checks)
	samples     []mcpInput                                     // extra examples; with sample they cover every route
	build       func(in mcpInput) (request.HTTPRequest, error) // input → pure request
	transform   func(status int, body []byte) ([]byte, error)  // optional response rewrite
	destructive bool                                           // sets the MCP destructiveHint annotation
}

// act starts one catalog entry on a single route.
func act(route string, sample mcpInput, build func(mcpInput) (request.HTTPRequest, error)) mcpAction {
	return mcpAction{routes: []string{route}, sample: sample, build: build}
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

// idDel is idPost plus the destructive annotation.
func idDel(route, key, sampleVal string, build func(string) request.HTTPRequest) mcpAction {
	e := idPost(route, key, sampleVal, build)
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
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildRunsListPath(v[0], v[1], v[2], v[3], v[4], v[5], v[6], limit)), nil
			}),
			"create": act("POST /v1/runs", mcpInput{"job": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("job", "workspace", "prompt")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				dryRun, err := in.flag("dry_run")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunsCreate(v[0], v[1], v[2], dryRun), nil
			}),
			"show":     idGet("GET /v1/runs/{id}", "id", "01ABC", request.BuildRunGetPath),
			"timeline": idGet("GET /v1/runs/{id}/timeline", "id", "01ABC", request.BuildRunTimelinePath),
			"transcript": act("GET /v1/runs/{id}/transcript", mcpInput{"id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStr("id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				raw, err := in.flag("raw")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunTranscript(id, raw), nil
			}),
			"log":    idGet("GET /v1/runs/{id}/log", "id", "01ABC", func(id string) string { return request.BuildRunLogPath(id, false) }),
			"kill":   idDel("POST /v1/runs/{id}/kill", "id", "01ABC", request.BuildRunsKill),
			"resume": idPost("POST /v1/runs/{id}/resume", "id", "01ABC", request.BuildRunResume),
			"replay": idPost("POST /v1/runs/{id}/replay", "id", "01ABC", request.BuildRunReplay),
			"retry":  idPost("POST /v1/runs/{id}/retry", "id", "01ABC", request.BuildRunsRetry),
			"message": act("POST /v1/runs/{id}/message", mcpInput{"id": "01ABC", "text": "ship it"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("id", "text")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunMessage(v[0], v[1]), nil
			}),
			"approve": act("POST /v1/runs/{id}/approve", mcpInput{"id": "01ABC", "decision": "allow"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("id", "decision")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRunApprove(v[0], v[1]), nil
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
				return mcpGet(request.BuildApprovalsListPath(cursor, limit)), nil
			}),
			"stats":  act("GET /v1/stats/runs", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildRunsStats(), nil }),
			"agents": act("GET /v1/agents/state", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildAgentsState(), nil }),
		},

		"job": {
			"list": act("GET /v1/jobs", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				ws, err := in.str("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobsListPath(ws)), nil
			}),
			"show": act("GET /v1/jobs/{name}", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobGetPath(v[0], v[1])), nil
			}),
			"create": act("POST /v1/jobs", mcpInput{"name": "nightly-sync", "content": "---\nharness: claude\n---\n\nRun the sync.\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "content")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildJobCreate(v[0], v[1], v[2]), nil
			}),
			"set": act("PUT /v1/jobs/{name}", mcpInput{"name": "nightly-sync", "markdown": "---\nharness: claude\n---\n\nRun the sync.\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "markdown")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "markdown")
				}
				return request.BuildJobPutMarkdown(v[0], v[1], v[2]), nil
			}),
			"patch": act("PATCH /v1/jobs/{name}", mcpInput{"name": "nightly-sync", "patch": map[string]any{"active": false}}, func(in mcpInput) (request.HTTPRequest, error) {
				name, err := in.reqStr("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				ws, err := in.str("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				patch, err := in.obj("patch")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if patch == nil {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "patch")
				}
				ifMatch, err := in.str("if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildJobPatch(name, ws, patch, ifMatch), nil
			}),
			"delete": del("DELETE /v1/jobs/{name}", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
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
				return request.BuildJobDelete(name, ws, purge), nil
			}),
			"run": act("POST /v1/jobs/{name}/run", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpPost(request.BuildJobRunPath(v[0], v[1])), nil
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
				return mcpGet(request.BuildSchedulesListPath(limit)), nil
			}),
			"memory_show": act("GET /v1/jobs/{name}/memory", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildJobMemoryGetPath(v[0], v[1])), nil
			}),
			"memory_set": act("PUT /v1/jobs/{name}/memory", mcpInput{"name": "nightly-sync", "content": "# MEMORY\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "content")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildJobMemoryPut(v[0], v[1], v[2]), nil
			}),
			"memory_add": act("POST /v1/jobs/{name}/memory/entries", mcpInput{"name": "nightly-sync", "text": "learned the API"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "date", "text")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[3] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildJobMemoryEntryAdd(v[0], v[1], v[2], v[3]), nil
			}),
			"memory_remove": del("DELETE /v1/jobs/{name}/memory/entries/{id}", mcpInput{"name": "nightly-sync", "entry_id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace", "entry_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildJobMemoryEntryDelete(v[0], v[1], v[2]), nil
			}),
			"memory_clear": del("DELETE /v1/jobs/{name}/memory", mcpInput{"name": "nightly-sync"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildJobMemoryClear(v[0], v[1]), nil
			}),
		},

		"workspace": {
			"list": act("GET /v1/workspaces", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildWorkspacesListPath()), nil
			}),
			"show": idGet("GET /v1/workspaces/{id}", "id", "ws1", request.BuildWorkspaceGetPath),
			"add": act("POST /v1/workspaces", mcpInput{"path": "/tmp/reports", "name": "reports"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("path", "name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				create, err := in.flag("create")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildWorkspaceAdd(v[0], v[1], create), nil
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
			"remove": idDel("DELETE /v1/workspaces/{id}", "id", "ws1", request.BuildWorkspaceDelete),
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
				v, err := in.head("workspace", "content")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildAssistantPut(v[0], v[1]), nil
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
			"remove": del("DELETE /v1/workspaces/{id}/assistant", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "if_match")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantDelete(v[0], v[1]), nil
			}),
			"wake": act("POST /v1/workspaces/{id}/assistant/wake", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "prompt")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				dryRun, err := in.flag("dry_run")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantWake(v[0], v[1], dryRun), nil
			}),
			"say": act("POST /v1/conversations/{id}/messages", mcpInput{"conversation": "01ABC", "text": "status?", "workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("conversation", "text", "workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildAssistantSay(v[0], v[1], v[2]), nil
			}),
			"memory_show": act("GET /v1/workspaces/{id}/assistant/memory", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				ws, err := in.reqStr("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildAssistantMemoryGetPath(ws)), nil
			}),
			"memory_set": act("PUT /v1/workspaces/{id}/assistant/memory", mcpInput{"workspace": "ws1", "content": "# MEMORY\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "content")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildAssistantMemoryPut(v[0], v[1]), nil
			}),
			"memory_add": act("POST /v1/workspaces/{id}/assistant/memory/entries", mcpInput{"workspace": "ws1", "text": "learned the API"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "date", "text")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "text")
				}
				return request.BuildAssistantMemoryEntryAdd(v[0], v[1], v[2]), nil
			}),
			"memory_remove": del("DELETE /v1/workspaces/{id}/assistant/memory/entries/{eid}", mcpInput{"workspace": "ws1", "entry_id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "entry_id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantMemoryEntryDelete(v[0], v[1]), nil
			}),
			"memory_clear": del("DELETE /v1/workspaces/{id}/assistant/memory", mcpInput{"workspace": "ws1"}, func(in mcpInput) (request.HTTPRequest, error) {
				ws, err := in.reqStr("workspace")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildAssistantMemoryClear(ws), nil
			}),
		},

		"file": {
			"list": fileEntry(mcpInput{"owner": "workspace", "workspace": "ws1", "prefix": "reports/"}, fileListAdapter, "GET /v1/workspaces/{id}/files", "GET /v1/jobs/{name}/files", "GET /v1/skills/{name}/files"),
			"get": fileEntry(
				mcpInput{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md"},
				fileGetAdapter,
				"GET /v1/workspaces/{id}/files/{path...}", "GET /v1/jobs/{name}/files/{path...}", "GET /v1/skills/{name}/files/{path...}",
			),
			"put": fileEntry(
				mcpInput{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md", "content": "# hi\n"},
				filePutAdapter,
				"PUT /v1/workspaces/{id}/files/{path...}", "PUT /v1/jobs/{name}/files/{path...}", "PUT /v1/skills/{name}/files/{path...}",
			),
			"remove": func() mcpAction {
				e := fileEntry(
					mcpInput{"owner": "workspace", "workspace": "ws1", "path": "reports/a.md"},
					fileDeleteAdapter,
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
				return request.BuildIntegrationSet(v[0], v[1], v[2], v[3], v[4], env), nil
			}),
			"remove":     idDel("DELETE /v1/integrations/{name}", "name", "mail", request.BuildIntegrationDelete),
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
			"logs": act("GET /v1/integrations/{name}/logs", mcpInput{"name": "mail"}, func(in mcpInput) (request.HTTPRequest, error) {
				name, err := in.reqStr("name")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildIntegrationLogsPath(name, false)), nil // follow is not available over MCP
			}),
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

		"events": {
			"list": act("GET /v1/events", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				cursor, err := in.str("cursor")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				limit, _, err := in.num("limit")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildEventsListPath(cursor, limit)), nil
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
				v, err := in.head("integration", "workspace", "job", "match", "extract")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				onIndex, _, err := in.num("on_index")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildRulesFire(v[0], v[1], v[2], v[3], v[4], in.value("event"), onIndex), nil
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
			"cancel":  idPost("POST /v1/queue/{id}/cancel", "id", "01ABC", request.BuildQueueCancel),
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
			"alerts": act("GET /v1/alerts", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
				since, err := in.str("since")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return mcpGet(request.BuildAlertsPath(since)), nil
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
			"new": act("POST /v1/conversations", mcpInput{"workspace": "ws1", "title": "sync check"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("workspace", "title", "origin_channel", "channel_type", "channel_ref")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildConversationCreate(v[0], v[1], v[2], v[3], v[4]), nil
			}),
			"patch": act("PATCH /v1/conversations/{id}", mcpInput{"id": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				id, err := in.reqStr("id")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				v, err := in.strs("title", "summary")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				revision, _, err := in.num("revision")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildConversationPatch(id, v[0], v[1], int64(revision)), nil
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
				v, err := in.head("channel_type", "channel_ref")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildPointerGet(v[0], v[1]), nil
			}),
			"pointer_set": act("PUT /v1/channel-pointers/{type}/{ref}", mcpInput{"channel_type": "cli", "channel_ref": "fritz", "conversation": "01ABC"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("channel_type", "channel_ref", "conversation")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[2] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "conversation")
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
			"create": act("POST /v1/skills", mcpInput{"name": "deploy", "content": "# SKILL.md\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "content", "provenance")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildSkillCreate(v[0], v[1], v[2]), nil
			}),
			"set": act("PUT /v1/skills/{name}", mcpInput{"name": "deploy", "content": "# SKILL.md\n"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "content")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "content")
				}
				return request.BuildSkillPut(v[0], v[1]), nil
			}),
			"update": idPost("POST /v1/skills/{name}/update", "name", "deploy", request.BuildSkillUpdate),
			"delete": idDel("DELETE /v1/skills/{name}", "name", "deploy", request.BuildSkillDelete),
		},

		"secret": {
			"list": act("GET /v1/secrets", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSecretsListPath()), nil
			}),
			"set": act("PUT /v1/secrets/{name}", mcpInput{"name": "API_KEY", "value": "s3cr3t"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("name", "value")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				if v[1] == "" {
					return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "value")
				}
				return request.BuildSecretPut(v[0], v[1]), nil
			}),
			"delete": idDel("DELETE /v1/secrets/{name}", "name", "API_KEY", request.BuildSecretDelete),
		},

		"config": {
			"show": act("GET /v1/config", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildConfigGet(), nil }),
			"set": act("PATCH /v1/config", mcpInput{"key": "daemon.log_level", "value": "debug"}, func(in mcpInput) (request.HTTPRequest, error) {
				v, err := in.head("key", "value")
				if err != nil {
					return request.HTTPRequest{}, err
				}
				return request.BuildConfigSet(v[0], v[1]), nil
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
			"logs": act("GET /v1/system/logs", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemLogsPath()), nil
			}),
			"update": act("GET /v1/system/update", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildSystemUpdatePath()), nil
			}),
			"drain":   act("POST /v1/system/drain", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildSystemDrain(), nil }),
			"undrain": act("POST /v1/system/undrain", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildSystemUndrain(), nil }),
			"shutdown": act("POST /v1/system/shutdown", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return request.BuildServiceShutdown(), nil
			}),
			"cloud_status": act("GET /v1/cloud/status", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) {
				return mcpGet(request.BuildCloudStatusPath()), nil
			}),
			"cloud_link":   act("POST /v1/cloud/link", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildCloudLink(), nil }),
			"cloud_unlink": del("DELETE /v1/cloud/link", mcpInput{}, func(mcpInput) (request.HTTPRequest, error) { return request.BuildCloudUnlink(), nil }),
			"bus_poll": func() mcpAction {
				e := act("GET /v1/stream", mcpInput{}, func(in mcpInput) (request.HTTPRequest, error) {
					topics, err := in.str("topics")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					cursor, _, err := in.num("cursor")
					if err != nil {
						return request.HTTPRequest{}, err
					}
					return mcpGet(request.BuildStreamDumpPath(topics, strconv.Itoa(cursor))), nil
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

// jobPauseAction adapts the shared pause/unpause builder.
func jobPauseAction(pause bool) func(mcpInput) (request.HTTPRequest, error) {
	return func(in mcpInput) (request.HTTPRequest, error) {
		v, err := in.head("name", "workspace")
		if err != nil {
			return request.HTTPRequest{}, err
		}
		return request.BuildJobPause(v[0], v[1], pause), nil
	}
}

// fileListAdapter lists files for one owner: workspace, job, or skill.
func fileListAdapter(in mcpInput) (request.HTTPRequest, error) {
	v, err := in.strs("workspace", "name", "prefix", "cursor")
	if err != nil {
		return request.HTTPRequest{}, err
	}
	limit, _, err := in.num("limit")
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
		return mcpGet(request.BuildFileListPath(v[0], v[2], v[3], limit)), nil
	case "job":
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return mcpGet(request.BuildJobFileListPath(v[1], v[0], v[2], v[3], limit)), nil
	default:
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return mcpGet(request.BuildSkillFileListPath(v[1], v[2], v[3], limit)), nil
	}
}

// fileGetAdapter reads one file for one owner. stat returns JSON metadata only.
func fileGetAdapter(in mcpInput) (request.HTTPRequest, error) {
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

// filePutAdapter writes one text file for one owner with optional If-Match.
func filePutAdapter(in mcpInput) (request.HTTPRequest, error) {
	v, err := in.strs("workspace", "name", "path", "content", "content_type", "if_match")
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
	content := []byte(v[3])
	switch owner {
	case "workspace":
		if v[0] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "workspace")
		}
		return request.BuildFilePut(v[0], v[2], content, v[4], v[5]), nil
	case "job":
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return request.BuildJobFilePut(v[1], v[0], v[2], content, v[4], v[5]), nil
	default:
		if v[1] == "" {
			return request.HTTPRequest{}, fmt.Errorf("input field %q is required", "name")
		}
		return request.BuildSkillFilePut(v[1], v[2], content, v[4], v[5]), nil
	}
}

// fileDeleteAdapter removes one file for one owner with optional If-Match.
func fileDeleteAdapter(in mcpInput) (request.HTTPRequest, error) {
	v, err := in.strs("workspace", "name", "path", "if_match")
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
		return request.BuildFileDelete(v[0], v[2], v[3]), nil
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
