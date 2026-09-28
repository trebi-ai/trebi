package connectors_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
)

func TestCapabilitiesMCP(t *testing.T) {
	ctx := context.Background()
	_, _, svc := openStack(t)

	server := mcp.NewServer(&mcp.Implementation{Name: "demo-mcp", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: "Use echo."})
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo the text."}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
	})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(srv.Close)

	e := inline("demo", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{
			CLI: &manifest.CLIAction{Commands: []string{"sh", "no-such-demo-cli"}},
			MCP: &manifest.MCPAction{URL: srv.URL},
		}
	})
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	c, err := svc.Capabilities(ctx, "demo", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Actions == nil || len(c.Actions.Commands) != 2 || c.Actions.Commands[0].Path == "" || c.Actions.Commands[1].Path != "" {
		t.Fatalf("commands = %+v", c.Actions)
	}
	in := c.Actions.MCP
	if in == nil || in.Error != "" || in.Server.Name != "demo-mcp" || in.Instructions != "Use echo." || len(in.Tools) != 1 || in.Tools[0].Name != "echo" {
		t.Fatalf("mcp = %+v", in)
	}

	// The second call comes from the cache: a closed server still answers.
	srv.Close()
	if c, err = svc.Capabilities(ctx, "demo", "", ""); err != nil || len(c.Actions.MCP.Tools) != 1 {
		t.Fatalf("cached = %+v %v", c.Actions.MCP, err)
	}
	if _, err := svc.Capabilities(ctx, "demo", "", "other:x"); err == nil {
		t.Fatal("an instance of another connector must fail")
	}
}

func TestCapabilitiesMCPDown(t *testing.T) {
	_, _, svc := openStack(t)
	ctx := context.Background()
	e := inline("down", func(m *manifest.Manifest) {
		m.Actions = &manifest.Actions{MCP: &manifest.MCPAction{URL: "http://127.0.0.1:1/mcp"}}
	})
	if err := svc.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	c, err := svc.Capabilities(ctx, "down", "", "")
	if err != nil || c.Actions.MCP.Error == "" {
		t.Fatalf("down = %+v %v", c.Actions, err)
	}
}
