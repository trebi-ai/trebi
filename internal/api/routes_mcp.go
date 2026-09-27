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

// newMCPHandler builds the stateless streamable HTTP handler over the
// registry's MCP server (shared with the native harness).
func (s *Server) newMCPHandler() http.Handler {
	srv := s.toolsReg.MCPServer(&mcp.Implementation{Name: "trebi", Version: Version})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

// Dispatch serves one built /v1 request through the audited, confirm-gated mux.
// The local API carries no auth, so there is nothing to skip: every caller is
// already the daemon owner, whether it arrives over /mcp, over /v1, or from the
// native tool loop inside the daemon.
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
		if a.Token != "" {
			req.Header.Set(tools.HeaderRunToken, a.Token)
		}
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
