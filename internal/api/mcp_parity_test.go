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
			got, err := busPollTransform(200, []byte(c.body))
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
