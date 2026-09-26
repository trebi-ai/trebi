package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPServerRoundTrip drives the registry's MCP server over one
// in-process transport pair: the exact path a native session takes. The
// tool list mirrors the catalog, and one call round trips the dispatcher
// envelope.
func TestMCPServerRoundTrip(t *testing.T) {
	t.Parallel()
	d := &fakeDisp{status: 200, body: []byte(`{"jobs":[]}`)}
	r := NewNativeRegistry(d)

	srv := r.MCPServer(&mcp.Implementation{Name: "trebi", Version: "test"})
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatal(err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "trebi-test", Version: "test"}, nil)
	cs, err := cli.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	want := map[string]bool{}
	for _, def := range r.Tools() {
		want[def.Name] = true
	}
	if len(want) == 0 {
		t.Fatal("the catalog is empty")
	}
	seen := map[string]bool{}
	for {
		page, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range page.Tools {
			seen[tl.Name] = true
		}
		if page.NextCursor == "" {
			break
		}
	}
	for name := range want {
		if !seen[name] {
			t.Fatalf("tool %s is missing from the MCP listing (%d seen)", name, len(seen))
		}
	}
	if seen["__bogus__"] {
		t.Fatal("the listing invented a tool")
	}

	// One call round trips: the arguments decode into action plus input,
	// and the dispatcher body comes back as text content. The SDK takes
	// the arguments as a decoded value, not as raw JSON.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "job",
		Arguments: map[string]any{"action": "list", "input": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("call failed: %+v", res.Content)
	}
	if d.n != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", d.n)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v", res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != `{"jobs":[]}` {
		t.Fatalf("content[0] = %#v", res.Content[0])
	}
}
