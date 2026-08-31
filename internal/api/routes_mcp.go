package api

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/request"
)

//go:embed mcpdocs/*.md
var mcpDocsFS embed.FS

// mcpInstructions is the remote client's front door (initialize result).
const mcpInstructions = `Trebi is an LLM task runner (daemon + /v1 API). Call each tool as {"action": "<name>", "input": {…}}. Call docs on a tool before first use. Call system.guide with topic mcp-basics before your first workflow, and system.guides for the index. Always pass workspace explicitly on workspace-scoped calls.`

// routeMCP mounts the MCP endpoint at /mcp (no /v1 prefix). Handler() must
// store apiWrapped after this runs: tool dispatch re-enters the audit-wrapped
// mux, not the outer auth layer.
func (s *Server) routeMCP(mux *http.ServeMux) {
	mux.Handle("/mcp", s.mcpHTTP)
}

// newMCPHandler builds the MCP server (one tool per catalog key) wrapped in
// the stateless streamable HTTP handler.
func (s *Server) newMCPHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "trebi", Version: Version}, &mcp.ServerOptions{
		Instructions: mcpInstructions,
	})
	for tool, actions := range s.mcpCatalog {
		srv.AddTool(mcpToolDef(tool, actions), s.mcpToolHandler(tool))
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

// mcpToolDef builds one tool: an exact action enum plus one loose input
// object, so the schema stays tiny and the enum stays in sync with the
// catalog.
func mcpToolDef(tool string, actions map[string]mcpAction) *mcp.Tool {
	names := make([]string, 0, len(actions)+1)
	for name := range actions {
		names = append(names, name)
	}
	slices.Sort(names)
	names = append(names, "docs") // handled in dispatch, not the catalog
	destructive := false
	for _, e := range actions {
		destructive = destructive || e.destructive
	}
	hint := destructive
	closed := false
	return &mcp.Tool{
		Name:        tool,
		Description: fmt.Sprintf("Trebi %s operations. Valid actions: %s. Action %q returns the input field reference for every action.", tool, strings.Join(names, ", "), "docs"),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        names,
					"description": "Operation to run. 'docs' returns the input field reference.",
				},
				"input": map[string]any{
					"type":                 "object",
					"description":          "Action inputs: path params, query params, and body fields in one flat object.",
					"additionalProperties": true,
				},
			},
			"required": []string{"action"},
		},
		Annotations: &mcp.ToolAnnotations{
			Title:           "trebi " + tool,
			DestructiveHint: &hint,
			IdempotentHint:  false,
			OpenWorldHint:   &closed,
		},
	}
}

// mcpToolHandler adapts one catalog tool into an MCP ToolHandler.
func (s *Server) mcpToolHandler(tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Action string   `json:"action"`
			Input  mcpInput `json:"input"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return mcpErrorResult("validation", "decode arguments: "+err.Error()), nil
			}
		}
		if args.Action == "docs" {
			return s.mcpDocsResult(tool)
		}
		entry, ok := s.mcpCatalog[tool][args.Action]
		if args.Action == "" || !ok {
			return mcpErrorResult("validation", fmt.Sprintf("unknown action %q for tool %q; action %q lists the valid ones", args.Action, tool, "docs")), nil
		}
		built, err := entry.build(args.Input)
		if err != nil {
			return mcpErrorResult("validation", err.Error()), nil
		}
		status, body, err := s.serveInProcess(ctx, built)
		if err != nil {
			return mcpErrorResult("internal", "dispatch: "+err.Error()), nil
		}
		// A 3xx carries no body. Returning it as success would report an
		// empty result for a write that never happened.
		if status >= 300 && status < 400 {
			return mcpErrorResult("internal", fmt.Sprintf("unexpected redirect (%d) for %s %s", status, built.Method, built.Path)), nil
		}
		if status < 400 {
			if entry.transform != nil {
				body, err = entry.transform(status, body)
				if err != nil {
					return mcpErrorResult("internal", "transform: "+err.Error()), nil
				}
			}
			return mcpTextResult(body), nil
		}
		// Error envelopes pass through verbatim so the machine code survives.
		res := mcpTextResult(body)
		res.IsError = true
		return res, nil
	}
}

// mcpDocsResult returns the embedded field reference for one tool.
func (s *Server) mcpDocsResult(tool string) (*mcp.CallToolResult, error) {
	doc, err := mcpDocsFS.ReadFile("mcpdocs/" + tool + ".md")
	if err != nil {
		return mcpErrorResult("internal", "no docs for tool "+tool), nil
	}
	return mcpTextResult(doc), nil
}

// mcpTextResult wraps one body as the tool response content.
func mcpTextResult(body []byte) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcpContent{&mcp.TextContent{Text: string(body)}},
	}
}

// mcpContent aliases the SDK content interface for shorter result literals.
type mcpContent = mcp.Content

// mcpErrorResult builds an IsError result carrying a trebi-style envelope.
func mcpErrorResult(code, message string) *mcp.CallToolResult {
	body, _ := json.Marshal(ErrorBody{Error: APIError{Code: code, Message: message}})
	res := mcpTextResult(body)
	res.IsError = true
	return res
}

// serveInProcess runs one built /v1 request through the audit-wrapped API
// mux. Auth is skipped: the /mcp request already passed authMiddleware.
func (s *Server) serveInProcess(ctx context.Context, built request.HTTPRequest) (int, []byte, error) {
	var reader *bytes.Reader
	if len(built.Raw) > 0 {
		reader = bytes.NewReader(built.Raw)
	} else if built.Body != nil {
		b, err := json.Marshal(built.Body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, built.Method, "http://mcp.internal"+built.Path, reader)
	if err != nil {
		return 0, nil, err
	}
	switch {
	case built.ContentType != "":
		req.Header.Set("Content-Type", built.ContentType)
	case len(built.Raw) > 0:
		req.Header.Set("Content-Type", "application/octet-stream")
	case built.Body != nil:
		req.Header.Set("Content-Type", "application/json")
	}
	if built.IfMatch != "" {
		req.Header.Set("If-Match", built.IfMatch)
	}
	rec := &mcpRecorder{header: http.Header{}}
	s.apiWrapped.ServeHTTP(rec, req)
	code := rec.code
	if code == 0 {
		code = http.StatusOK
	}
	return code, rec.body.Bytes(), nil
}

// mcpRecorder captures one in-process response. Flush exists because the
// /v1/stream dump handler requires an http.Flusher.
type mcpRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *mcpRecorder) Header() http.Header { return r.header }

func (r *mcpRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }

func (r *mcpRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *mcpRecorder) Flush() {}

// busPollTransform parses one /v1/stream SSE dump into a JSON object:
// {"events": […], "next_cursor": N}. The model carries next_cursor back on
// the next poll.
func busPollTransform(status int, body []byte) ([]byte, error) {
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
