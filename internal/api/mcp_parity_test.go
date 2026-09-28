package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/tools"
)

func TestMCPCatalogCoversRouteUniverse(t *testing.T) {
	t.Parallel()
	universe := mcpRouteUniverse(t)
	covered := map[string]bool{}
	for tool, actions := range tools.NewCatalog() {
		for name, entry := range actions {
			for _, route := range entry.Routes {
				if !universe[route] {
					t.Errorf("%s.%s declares route %q, which no route file registers", tool, name, route)
				}
				covered[route] = true
			}
		}
	}
	for route := range universe {
		if covered[route] {
			continue
		}
		if _, excluded := mcpExcluded[route]; !excluded {
			t.Errorf("route %q has no MCP action and no exclusion entry", route)
		}
	}
	for route, reason := range mcpExcluded {
		if covered[route] {
			t.Errorf("excluded route %q (%s) is also covered by an action", route, reason)
		}
		if !universe[route] {
			t.Errorf("excluded route %q (%s) is not a registered pattern", route, reason)
		}
	}
}

func TestMCPCatalogSamplesHitDeclaredRoutes(t *testing.T) {
	t.Parallel()
	mux := mcpTestMux()
	for tool, actions := range tools.NewCatalog() {
		for name, entry := range actions {
			samples := append([]tools.Input{entry.Sample}, entry.Samples...)
			hit := map[string]bool{}
			for i, sample := range samples {
				built, err := entry.Build(sample)
				if err != nil {
					t.Errorf("%s.%s sample %d: build: %v", tool, name, i, err)
					continue
				}
				req, err := http.NewRequest(built.Method, "http://parity.internal"+built.Path, nil)
				if err != nil {
					t.Fatalf("%s.%s sample %d: request: %v", tool, name, i, err)
				}
				_, pattern := mux.Handler(req)
				if !slices.Contains(entry.Routes, pattern) {
					t.Errorf("%s.%s sample %d hit %q; want one of %v", tool, name, i, pattern, entry.Routes)
					continue
				}
				hit[pattern] = true
			}
			for _, route := range entry.Routes {
				if !hit[route] {
					t.Errorf("%s.%s declares route %q but no sample hits it", tool, name, route)
				}
			}
		}
	}
}

func TestMCPDocsCoverEveryCatalogAction(t *testing.T) {
	t.Parallel()
	for tool, actions := range tools.NewCatalog() {
		doc, err := tools.ReadDoc(tool)
		if err != nil {
			t.Errorf("tool %q: read mcpdocs/%s.md: %v", tool, tool, err)
			continue
		}
		text := string(doc)
		for name := range actions {
			if !strings.Contains(text, "### "+name+"\n") {
				t.Errorf("tool %q: mcpdocs/%s.md does not document action %q", tool, tool, name)
			}
		}
	}
}

// A destructive action must document its confirm gate, or a model calls it,
// gets `confirm_required`, and has no field name to retry with.
func TestMCPDocsDocumentConfirmOnGatedActions(t *testing.T) {
	t.Parallel()
	gate := newConfirmGate()
	for tool, actions := range tools.NewCatalog() {
		doc, err := tools.ReadDoc(tool)
		if err != nil {
			t.Errorf("tool %q: read docs: %v", tool, err)
			continue
		}
		for name, entry := range actions {
			gated := false
			for _, route := range entry.Routes {
				if _, ok := gate.routes[route]; ok {
					gated = true
				}
			}
			if !gated {
				continue
			}
			section := tools.DocSection(string(doc), name)
			if section == "" {
				t.Errorf("%s.%s: no docs section", tool, name)
				continue
			}
			if !strings.Contains(section, "`confirm`") {
				t.Errorf("%s.%s hits a confirm-gated route but its docs do not name the confirm field", tool, name)
			}
			if _, ok := entry.Sample["confirm"]; !ok {
				t.Errorf("%s.%s sample omits confirm; the parity sample would be refused", tool, name)
			}
		}
	}
}

// mcpExcluded lists the routes no MCP action covers, with the reason. The
// dump mode of GET /v1/stream is covered by system.bus_poll; only its
// follow mode (a query parameter, not a pattern) stays out.
var mcpExcluded = map[string]string{
	// Session-only controls: a session rides one conversation; the chat
	// tools cover send and events (plan G.2).
	"POST /v1/assistant/native/test":                        "the Test connection button is a settings-page affordance; it writes nothing",
	"POST /v1/conversations/{id}/close":                     "the CLI chat and the drawer close their own session",
	"POST /v1/conversations/{id}/permissions/{pid}":         "permission answers go through the drawer or the chat prompt",
	"POST /v1/conversations/{id}/mode":                      "the mode picker lives in the drawer",
	"POST /v1/conversations/{id}/revert":                    "revert runs from the drawer diff card",
	"GET /v1/envsets":                                       "envset names are a UI form-option list; the agent payload carries the chosen names",
	"GET /v1/workspaces/{id}/agents/{name}/email":           "address reveal is a UI copy affordance; the agent payload carries the masked form",
	"GET /v1/workspaces/{id}/agents":                        "superseded by cross-workspace GET /v1/agents",
	"GET /v1/proposals":                                     "proposal review is a human UI/CLI workflow",
	"POST /v1/proposals/{id}/decide":                        "decisions are human-only and blocked from MCP",
	"GET /v1/runs/{id}/attach":                              "WebSocket terminal",
	"GET /v1/harnesses/{name}/login/attach":                 "WebSocket login TUI",
	"GET /v1/connectors/{name}/login/{flow_id}":             "SSE stream that stays open until the login ends; login and login_submit return the current step",
	"POST /v1/agent-hook/{event}":                           "internal harness hook, not a client operation",
	"POST /v1/skills/{name}/update":                         "alias of refresh (same handler)",
	"POST /v1/jobs/{name}/memory/migrate":                   "repair path; the daemon migrates legacy files at launch",
	"POST /v1/workspaces/{id}/agents/{name}/memory/migrate": "repair path; the daemon migrates legacy files at launch",
	"POST /v1/workspaces/{id}/assistant/memory/migrate":     "repair path; the daemon migrates legacy files at launch",
}

// mcpRouteUniverse extracts every registered /v1 pattern from the route
// sources. It is the MCP analogue of the CLI parity rule's route set.
func mcpRouteUniverse(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("routes_*.go")
	if err != nil {
		t.Fatal(err)
	}
	universe := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // test sources are not route registrations
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mcpRouteRe.FindAllStringSubmatch(string(b), -1) {
			universe[m[1]+" "+m[2]] = true
		}
	}
	if len(universe) == 0 {
		t.Fatal("route universe is empty: run from internal/api")
	}
	return universe
}

// mcpRouteRe matches the route literals at the top of every routes_*.go file.
var mcpRouteRe = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE) (/v1/[^"]+)"`)

// mcpTestMux registers every route area on a fresh mux. Route registration
// never dereferences the server, so a zero Server is enough.
func mcpTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	s := &Server{}
	s.routeAgents(mux)
	s.routeAssistant(mux)
	s.routeConversations(mux)
	s.routeEvents(mux)
	s.routeFiles(mux)
	s.routeEmail(mux)
	s.routeHooks(mux)
	s.routeHarness(mux)
	s.routeInbox(mux)
	s.routeOutbox(mux)
	s.routeNotify(mux)
	s.routeConnectors(mux)
	s.routeJobs(mux)
	s.routeMemory(mux)
	s.routeQueue(mux)
	s.routeProposals(mux)
	s.routeRuns(mux)
	s.routeSettings(mux)
	s.routeSystem(mux)
	s.routeWorkspaces(mux)
	return mux
}

// knownFields is every input key the catalog may read. The docs check probes
// each one against each action: a key that changes the request must be listed.
var knownFields = []tools.DocField{ //nolint:gochecknoglobals // static probe list, never mutated
	{Name: "if_match", Type: "string"}, {Name: "limit", Type: "number"}, {Name: "cursor", Type: "string"},
	{Name: "max_bytes", Type: "number"}, {Name: "lines", Type: "number"}, {Name: "encoding", Type: "string"},
	{Name: "harnesses", Type: "object"}, {Name: "definition", Type: "object"}, {Name: "inputs", Type: "object"},
	{Name: "harness", Type: "string"}, {Name: "kind", Type: "string"}, {Name: "reason", Type: "string"},
	{Name: "conversation_id", Type: "string"}, {Name: "idempotency_key", Type: "string"},
	{Name: "extra_prompt", Type: "string"}, {Name: "prompt", Type: "string"}, {Name: "dry_run", Type: "boolean"},
	{Name: "workspace", Type: "string"}, {Name: "purge", Type: "boolean"}, {Name: "values", Type: "object"},
	{Name: "summary", Type: "string"}, {Name: "revision", Type: "number"}, {Name: "stat", Type: "boolean"},
	{Name: "raw", Type: "boolean"}, {Name: "prefix", Type: "string"}, {Name: "env", Type: "object"},
	{Name: "cwd", Type: "string"}, {Name: "format", Type: "string"}, {Name: "date", Type: "string"},
	{Name: "capability", Type: "string"}, {Name: "provenance", Type: "string"}, {Name: "kit", Type: "object"},
	{Name: "actions", Type: "object"}, {Name: "events", Type: "object"}, {Name: "channel", Type: "object"},
	{Name: "wait_ms", Type: "number"}, {Name: "state", Type: "string"}, {Name: "approval_id", Type: "string"},
	{Name: "channel_type", Type: "string"}, {Name: "channel_ref", Type: "string"},
	{Name: "content", Type: "string"}, {Name: "force", Type: "boolean"}, {Name: "at", Type: "string"},
	{Name: "text", Type: "string"}, {Name: "body", Type: "string"}, {Name: "entry_id", Type: "string"},
	{Name: "status", Type: "string"}, {Name: "note", Type: "string"},
	{Name: "reveal", Type: "boolean"}, {Name: "license", Type: "string"},
	{Name: "direction", Type: "string"}, {Name: "thread", Type: "string"},
	{Name: "message", Type: "string"}, {Name: "attachment", Type: "string"}, {Name: "out", Type: "string"},
}

// probeValue returns a value that differs from what the sample carries, so a
// builder that reads the field must produce a different request.

// TestMCPDocsFieldParity is the field-level parity gate: every documented
// field must change the built request, every documented required field must be
// required, and no action may read a field the docs do not list.
func TestMCPDocsFieldParity(t *testing.T) {
	t.Parallel()
	for tool, actions := range tools.NewCatalog() {
		doc, err := tools.ReadDoc(tool)
		if err != nil {
			t.Fatalf("read docs for %s: %v", tool, err)
		}
		for name, entry := range actions {
			section := tools.DocSection(string(doc), name)
			if section == "" {
				t.Errorf("%s.%s has no docs section", tool, name)
				continue
			}
			if _, err := entry.Build(entry.Sample); err != nil {
				t.Errorf("%s.%s: build sample: %v", tool, name, err)
				continue
			}
			fields := tools.DocFields(section)
			documented := map[string]bool{}
			for _, f := range fields {
				documented[f.Name] = true
				assertFieldRead(t, tool, name, f, entry)
			}
			assertNoUndocumentedRead(t, tool, name, entry, documented)
			assertKnownFieldsDocumented(t, tool, name, entry, documented)
		}
	}
}

func TestDocsSignatureBlock(t *testing.T) {
	t.Parallel()
	for tool, actions := range tools.NewCatalog() {
		doc, err := tools.ReadDoc(tool)
		if err != nil {
			t.Fatalf("read docs for %s: %v", tool, err)
		}
		block := tools.DocSignatures(string(doc))
		if !strings.Contains(block, "docs()") {
			t.Errorf("%s: block must list docs()", tool)
		}
		for action := range actions {
			line := sigLine(block, action)
			if line == "" {
				t.Errorf("%s: block has no signature for %q", tool, action)
				continue
			}
			for _, f := range tools.DocFields(tools.DocSection(string(doc), action)) {
				want := f.Name + "?"
				if f.Required {
					want = f.Name
				}
				if !strings.Contains(line, want) {
					t.Errorf("%s.%s: signature omits %q: %s", tool, action, want, line)
				}
			}
		}
	}
}

// sigLine returns the one signature line of an action.
func sigLine(block, action string) string {
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, action+"(") {
			return line
		}
	}
	return ""
}

// assertFieldRead checks that setting one documented field changes the request
// under at least one sample, and that a documented required field is enforced.
func assertFieldRead(t *testing.T, tool, action string, f tools.DocField, entry tools.Action) {
	t.Helper()
	read := false
	for _, sample := range append([]tools.Input{entry.Sample}, entry.Samples...) {
		base, err := entry.Build(sample)
		if err != nil {
			continue // this sample does not apply to this field
		}
		probe := tools.Input{}
		for k, v := range sample {
			probe[k] = v
		}
		probe[f.Name] = probeValue(f, sample)
		if f.Name == "encoding" {
			probe["content"] = "aGk=" // base64 must decode; the sample text does not
		}
		got, err := entry.Build(probe)
		if err != nil {
			continue
		}
		if !sameRequest(base, got) {
			read = true
			break
		}
	}
	if !read && !entry.Bounded && !readsResponseField(entry, f.Name) && !clientSideField(tool, action, f.Name) {
		t.Errorf("%s.%s documents field %q, but no sample shows the builder reading it", tool, action, f.Name)
	}
	// The confirm gate lives in the API, so a builder must not enforce it.
	if !f.Required || f.Name == "confirm" {
		return
	}
	without := tools.Input{}
	for k, v := range entry.Sample {
		if k != f.Name {
			without[k] = v
		}
	}
	if _, err := entry.Build(without); err == nil {
		t.Errorf("%s.%s documents %q as required, but the builder accepts it missing", tool, action, f.Name)
	}
}

// assertNoUndocumentedRead checks that the primary sample names no field the
// docs leave out. The extra samples are route probes and may carry a union of
// fields across the tool, so they are not checked here.
func assertNoUndocumentedRead(t *testing.T, tool, action string, entry tools.Action, documented map[string]bool) {
	t.Helper()
	for k := range entry.Sample {
		if !documented[k] {
			t.Errorf("%s.%s reads field %q, which its docs do not list", tool, action, k)
		}
	}
}

// assertKnownFieldsDocumented probes every catalog input key against one
// action: a key that changes the built request must appear in the docs.
func assertKnownFieldsDocumented(t *testing.T, tool, action string, entry tools.Action, documented map[string]bool) {
	t.Helper()
	base, err := entry.Build(entry.Sample)
	if err != nil {
		return
	}
	for _, f := range knownFields {
		if documented[f.Name] {
			continue
		}
		probe := tools.Input{}
		for k, v := range entry.Sample {
			probe[k] = v
		}
		probe[f.Name] = probeValue(f, entry.Sample)
		got, err := entry.Build(probe)
		if err != nil || sameRequest(base, got) {
			continue
		}
		t.Errorf("%s.%s reads field %q, which its docs do not list", tool, action, f.Name)
	}
}

// clientSideField reports a field the local `trebi mcp serve` bridge consumes
// before the call reaches the daemon. The builder never sees it, so the read
// probe cannot find it here.
func clientSideField(tool, action, name string) bool {
	return tool == "secret" && action == "set" && name == "value_file" ||
		tool == "email" && action == "attachment" && name == "out"
}

// readsResponseField reports whether one field steers the response transform
// instead of the request, so the built request is rightly unchanged.
func readsResponseField(e tools.Action, name string) bool {
	return e.Transform != nil && (name == "lines" || name == "max_bytes")
}

// sameRequest compares two built requests by every field a change can touch.
func sameRequest(a, b request.HTTPRequest) bool {
	ab, _ := json.Marshal(a.Body)
	bb, _ := json.Marshal(b.Body)
	return a.Method == b.Method && a.Path == b.Path && a.IfMatch == b.IfMatch &&
		a.ContentType == b.ContentType && string(a.Raw) == string(b.Raw) && string(ab) == string(bb)
}

func probeValue(f tools.DocField, sample tools.Input) any {
	switch f.Type {
	case "boolean":
		return sample[f.Name] != true
	case "number":
		if n, ok := sample[f.Name].(float64); ok {
			return n + 1
		}
		return float64(7)
	case "object":
		return map[string]any{"probe": "1"}
	case "array":
		return []any{"probe"}
	}
	if f.Name == "encoding" {
		return "base64"
	}
	if f.Name == "owner" {
		if sample[f.Name] == "job" {
			return "skill"
		}
		return "job"
	}
	return "probe"
}
