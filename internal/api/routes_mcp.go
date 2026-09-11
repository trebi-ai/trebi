package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/tools"
)

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
		Instructions: tools.Instructions,
	})
	for _, def := range s.toolsReg.Tools() {
		srv.AddTool(mcpToolDef(def), s.mcpToolHandler(def.Name))
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

// mcpToolDef builds one tool: an exact action enum plus one loose input
// object, so the schema stays tiny and the enum stays in sync with the
// catalog.
func mcpToolDef(def tools.ToolDef) *mcp.Tool {
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
func (s *Server) mcpToolHandler(tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Action string      `json:"action"`
			Input  tools.Input `json:"input"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return mcpErrorResult("validation", "decode arguments: "+err.Error()), nil
			}
		}
		res := s.toolsReg.Call(ctx, tool, args.Action, args.Input)
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

// Dispatch serves one built /v1 request through the audited, confirm-gated,
// unauthenticated mux. Auth is skipped: every caller is responsible for
// having established that the caller is the daemon owner. MCP inherits that
// from authMiddleware on /mcp; the native loop is the daemon itself.
// Native passes actor=assistant and the run ID via tools.WithActor so
// auditMiddleware can attribute the call.
func (s *Server) Dispatch(ctx context.Context, built request.HTTPRequest) (int, []byte, error) {
	if s.apiWrapped == nil {
		return 0, nil, fmt.Errorf("dispatch: handler not mounted")
	}
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
	if a, ok := tools.ActorFrom(ctx); ok {
		req.Header.Set(tools.HeaderActor, a.Name)
		req.Header.Set(tools.HeaderRunID, a.RunID)
	}
	rec := &mcpRecorder{header: http.Header{}}
	s.apiWrapped.ServeHTTP(rec, req)
	code := rec.code
	if code == 0 {
		code = http.StatusOK
	}
	return code, rec.body.Bytes(), nil
}

var _ tools.Dispatcher = (*Server)(nil)

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
