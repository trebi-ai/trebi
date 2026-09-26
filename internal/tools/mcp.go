package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServer builds one MCP server over the registry: one tool per catalog
// key. The caller owns the transport, so the same surface serves /mcp over
// HTTP and the native harness over an in-process transport
// (plan 2026-09-26 I.7).
func (r *Registry) MCPServer(impl *mcp.Implementation) *mcp.Server {
	srv := mcp.NewServer(impl, &mcp.ServerOptions{Instructions: Instructions})
	for _, def := range r.Tools() {
		srv.AddTool(mcpToolDef(def), r.mcpToolHandler(def.Name))
	}
	return srv
}

// mcpToolDef builds one tool: an exact action enum plus one loose input
// object, so the schema stays tiny and the enum stays in sync with the
// catalog.
func mcpToolDef(def ToolDef) *mcp.Tool {
	hint := def.Destructive
	closed := false
	return &mcp.Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: def.Schema,
		Annotations: &mcp.ToolAnnotations{
			Title:           "trebi " + def.Name,
			DestructiveHint: &hint,
			IdempotentHint:  false,
			OpenWorldHint:   &closed,
		},
	}
}

// mcpToolHandler adapts one catalog tool into an MCP ToolHandler.
func (r *Registry) mcpToolHandler(tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Action string `json:"action"`
			Input  Input  `json:"input"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return mcpErrorResult("validation", "decode arguments: "+err.Error()), nil
			}
		}
		res := r.Call(ctx, tool, args.Action, args.Input)
		if res.Err != nil {
			return mcpErrorResult("internal", "dispatch: "+res.Err.Error()), nil
		}
		// A 3xx carries no body. Returning it as success would report an
		// empty result for a write that never happened.
		if res.Status >= 300 && res.Status < 400 {
			return mcpErrorResult("internal", fmt.Sprintf("unexpected redirect (%d)", res.Status)), nil
		}
		out := mcpTextResult(res.Body)
		if res.Status >= 400 {
			out.IsError = true
		}
		return out, nil
	}
}

func mcpTextResult(body []byte) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}
}

// mcpErrorResult builds an IsError result carrying a trebi-style envelope.
func mcpErrorResult(code, message string) *mcp.CallToolResult {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	res := mcpTextResult(body)
	res.IsError = true
	return res
}
