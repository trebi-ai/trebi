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
)

func TestMCPCatalogCoversRouteUniverse(t *testing.T) {
	t.Parallel()
	universe := mcpRouteUniverse(t)
	covered := map[string]bool{}
	for tool, actions := range newMCPCatalog() {
		for name, entry := range actions {
			for _, route := range entry.routes {
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
	for tool, actions := range newMCPCatalog() {
		for name, entry := range actions {
			samples := append([]mcpInput{entry.sample}, entry.samples...)
			hit := map[string]bool{}
			for i, sample := range samples {
				built, err := entry.build(sample)
				if err != nil {
					t.Errorf("%s.%s sample %d: build: %v", tool, name, i, err)
					continue
				}
				req, err := http.NewRequest(built.Method, "http://parity.internal"+built.Path, nil)
				if err != nil {
					t.Fatalf("%s.%s sample %d: request: %v", tool, name, i, err)
				}
				_, pattern := mux.Handler(req)
				if !slices.Contains(entry.routes, pattern) {
					t.Errorf("%s.%s sample %d hit %q; want one of %v", tool, name, i, pattern, entry.routes)
					continue
				}
				hit[pattern] = true
			}
			for _, route := range entry.routes {
				if !hit[route] {
					t.Errorf("%s.%s declares route %q but no sample hits it", tool, name, route)
				}
			}
		}
	}
}

func TestMCPDocsCoverEveryCatalogAction(t *testing.T) {
	t.Parallel()
	for tool, actions := range newMCPCatalog() {
		doc, err := mcpDocsFS.ReadFile("mcpdocs/" + tool + ".md")
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
	for tool, actions := range newMCPCatalog() {
		doc, err := mcpDocsFS.ReadFile("mcpdocs/" + tool + ".md")
		if err != nil {
			t.Errorf("tool %q: read docs: %v", tool, err)
			continue
		}
		for name, entry := range actions {
			gated := false
			for _, route := range entry.routes {
				if _, ok := gate.routes[route]; ok {
					gated = true
				}
			}
			if !gated {
				continue
			}
			section := docSection(string(doc), name)
			if section == "" {
				t.Errorf("%s.%s: no docs section", tool, name)
				continue
			}
			if !strings.Contains(section, "`confirm`") {
				t.Errorf("%s.%s hits a confirm-gated route but its docs do not name the confirm field", tool, name)
			}
			if _, ok := entry.sample["confirm"]; !ok {
				t.Errorf("%s.%s sample omits confirm; the parity sample would be refused", tool, name)
			}
		}
	}
}

// docSection returns the body of one "### <action>" section.

func TestBusPollTransform(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		wantEvents int
		wantCursor int64
	}{
		{"empty dump", "", 0, 0},
		{"keepalive only", ": ping\n\n", 0, 0},
		{"events in order", "data: {\"seq\":5,\"topic\":\"job.created\"}\n\ndata: {\"seq\":7,\"topic\":\"job.updated\"}\n\n", 2, 7},
		{"cursor takes the max seq", "data: {\"seq\":9}\n\ndata: {\"seq\":2}\n\n", 2, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := busPollTransform(nil, 200, []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			var res struct {
				Events     []map[string]any `json:"events"`
				NextCursor int64            `json:"next_cursor"`
			}
			if err := json.Unmarshal(got, &res); err != nil {
				t.Fatalf("decode %s: %v", got, err)
			}
			if len(res.Events) != c.wantEvents || res.NextCursor != c.wantCursor {
				t.Fatalf("got %d events, cursor %d; want %d events, cursor %d",
					len(res.Events), res.NextCursor, c.wantEvents, c.wantCursor)
			}
		})
	}
}

// mcpExcluded lists the routes no MCP action covers, with the reason. The
// dump mode of GET /v1/stream is covered by system.bus_poll; only its
// follow mode (a query parameter, not a pattern) stays out.
var mcpExcluded = map[string]string{
	"GET /v1/runs/{id}/attach":              "WebSocket terminal",
	"GET /v1/harnesses/{name}/login/attach": "WebSocket login TUI",
	"POST /v1/agent-hook/{event}":           "internal harness hook, not a client operation",
	"POST /v1/integrations/{name}/stop":     "alias of disconnect (same handler)",
	"POST /v1/skills/{name}/update":         "alias of refresh (same handler)",
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
	s.routeConversations(mux)
	s.routeEvents(mux)
	s.routeFiles(mux)
	s.routeHarness(mux)
	s.routeInbox(mux)
	s.routeIntegrations(mux)
	s.routeJobs(mux)
	s.routeQueue(mux)
	s.routeRuns(mux)
	s.routeSettings(mux)
	s.routeSystem(mux)
	s.routeWorkspaces(mux)
	return mux
}

// knownFields is every input key the catalog may read. The docs check probes
// each one against each action: a key that changes the request must be listed.
var knownFields = []docField{ //nolint:gochecknoglobals // static probe list, never mutated
	{"if_match", "string", false}, {"limit", "number", false}, {"cursor", "string", false},
	{"max_bytes", "number", false}, {"lines", "number", false}, {"encoding", "string", false},
	{"harnesses", "object", false}, {"facets", "array", false}, {"inputs", "object", false},
	{"harness", "string", false}, {"kind", "string", false}, {"reason", "string", false},
	{"conversation_id", "string", false}, {"idempotency_key", "string", false},
	{"extra_prompt", "string", false}, {"prompt", "string", false}, {"dry_run", "boolean", false},
	{"workspace", "string", false}, {"purge", "boolean", false}, {"values", "object", false},
	{"summary", "string", false}, {"revision", "number", false}, {"stat", "boolean", false},
	{"raw", "boolean", false}, {"prefix", "string", false}, {"env", "object", false},
	{"cwd", "string", false}, {"format", "string", false}, {"date", "string", false},
	{"wait_ms", "number", false}, {"state", "string", false}, {"approval_id", "string", false},
	{"channel_type", "string", false}, {"channel_ref", "string", false},
}

// probeValue returns a value that differs from what the sample carries, so a
// builder that reads the field must produce a different request.

// TestMCPDocsFieldParity is the field-level parity gate: every documented
// field must change the built request, every documented required field must be
// required, and no action may read a field the docs do not list.
func TestMCPDocsFieldParity(t *testing.T) {
	t.Parallel()
	for tool, actions := range newMCPCatalog() {
		doc, err := mcpDocsFS.ReadFile("mcpdocs/" + tool + ".md")
		if err != nil {
			t.Fatalf("read docs for %s: %v", tool, err)
		}
		for name, entry := range actions {
			section := docSection(string(doc), name)
			if section == "" {
				t.Errorf("%s.%s has no docs section", tool, name)
				continue
			}
			if _, err := entry.build(entry.sample); err != nil {
				t.Errorf("%s.%s: build sample: %v", tool, name, err)
				continue
			}
			fields := docFields(section)
			documented := map[string]bool{}
			for _, f := range fields {
				documented[f.name] = true
				assertFieldRead(t, tool, name, f, entry)
			}
			assertNoUndocumentedRead(t, tool, name, entry, documented)
			assertKnownFieldsDocumented(t, tool, name, entry, documented)
		}
	}
}

func TestDocsSignatureBlock(t *testing.T) {
	t.Parallel()
	for tool, actions := range newMCPCatalog() {
		doc, err := mcpDocsFS.ReadFile("mcpdocs/" + tool + ".md")
		if err != nil {
			t.Fatalf("read docs for %s: %v", tool, err)
		}
		block := docSignatures(string(doc))
		if !strings.Contains(block, "docs()") {
			t.Errorf("%s: block must list docs()", tool)
		}
		for action := range actions {
			line := sigLine(block, action)
			if line == "" {
				t.Errorf("%s: block has no signature for %q", tool, action)
				continue
			}
			for _, f := range docFields(docSection(string(doc), action)) {
				want := f.name + "?"
				if f.required {
					want = f.name
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
func assertFieldRead(t *testing.T, tool, action string, f docField, entry mcpAction) {
	t.Helper()
	read := false
	for _, sample := range append([]mcpInput{entry.sample}, entry.samples...) {
		base, err := entry.build(sample)
		if err != nil {
			continue // this sample does not apply to this field
		}
		probe := mcpInput{}
		for k, v := range sample {
			probe[k] = v
		}
		probe[f.name] = probeValue(f, sample)
		if f.name == "encoding" {
			probe["content"] = "aGk=" // base64 must decode; the sample text does not
		}
		got, err := entry.build(probe)
		if err != nil {
			continue
		}
		if !sameRequest(base, got) {
			read = true
			break
		}
	}
	if !read && !entry.bounded && !entry.readsResponseField(f.name) && !clientSideField(tool, action, f.name) {
		t.Errorf("%s.%s documents field %q, but no sample shows the builder reading it", tool, action, f.name)
	}
	// The confirm gate lives in the API, so a builder must not enforce it.
	if !f.required || f.name == "confirm" {
		return
	}
	without := mcpInput{}
	for k, v := range entry.sample {
		if k != f.name {
			without[k] = v
		}
	}
	if _, err := entry.build(without); err == nil {
		t.Errorf("%s.%s documents %q as required, but the builder accepts it missing", tool, action, f.name)
	}
}

// assertNoUndocumentedRead checks that the primary sample names no field the
// docs leave out. The extra samples are route probes and may carry a union of
// fields across the tool, so they are not checked here.
func assertNoUndocumentedRead(t *testing.T, tool, action string, entry mcpAction, documented map[string]bool) {
	t.Helper()
	for k := range entry.sample {
		if !documented[k] {
			t.Errorf("%s.%s reads field %q, which its docs do not list", tool, action, k)
		}
	}
}

// assertKnownFieldsDocumented probes every catalog input key against one
// action: a key that changes the built request must appear in the docs.
func assertKnownFieldsDocumented(t *testing.T, tool, action string, entry mcpAction, documented map[string]bool) {
	t.Helper()
	base, err := entry.build(entry.sample)
	if err != nil {
		return
	}
	for _, f := range knownFields {
		if documented[f.name] {
			continue
		}
		probe := mcpInput{}
		for k, v := range entry.sample {
			probe[k] = v
		}
		probe[f.name] = probeValue(f, entry.sample)
		got, err := entry.build(probe)
		if err != nil || sameRequest(base, got) {
			continue
		}
		t.Errorf("%s.%s reads field %q, which its docs do not list", tool, action, f.name)
	}
}

// clientSideField reports a field the local `trebi mcp serve` bridge consumes
// before the call reaches the daemon. The builder never sees it, so the read
// probe cannot find it here.
func clientSideField(tool, action, name string) bool {
	return tool == "secret" && action == "set" && name == "value_file"
}

// readsResponseField reports whether one field steers the response transform
// instead of the request, so the built request is rightly unchanged.
func (e mcpAction) readsResponseField(name string) bool {
	return e.transform != nil && (name == "lines" || name == "max_bytes")
}

// sameRequest compares two built requests by every field a change can touch.
func sameRequest(a, b request.HTTPRequest) bool {
	ab, _ := json.Marshal(a.Body)
	bb, _ := json.Marshal(b.Body)
	return a.Method == b.Method && a.Path == b.Path && a.IfMatch == b.IfMatch &&
		a.ContentType == b.ContentType && string(a.Raw) == string(b.Raw) && string(ab) == string(bb)
}

func probeValue(f docField, sample mcpInput) any {
	switch f.typ {
	case "boolean":
		return sample[f.name] != true
	case "number":
		if n, ok := sample[f.name].(float64); ok {
			return n + 1
		}
		return float64(7)
	case "object":
		return map[string]any{"probe": "1"}
	case "array":
		return []any{"probe"}
	}
	if f.name == "encoding" {
		return "base64"
	}
	if f.name == "owner" {
		if sample[f.name] == "job" {
			return "skill"
		}
		return "job"
	}
	return "probe"
}
