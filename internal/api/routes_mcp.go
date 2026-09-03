package api

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trebi-ai/trebi/internal/request"
)

//go:embed mcpdocs/*.md
var mcpDocsFS embed.FS

// mcpInstructions is the remote client's front door (initialize result).
const mcpInstructions = `Trebi is an LLM task runner (daemon + /v1 API). Call each tool as {"action": "<name>", "input": {…}}.

Call docs on a tool before its first use: docs carries the field signature of every action and is authoritative.

Three rules:
1. Validate after you write a JOB.md (job.validate).
2. Dry-run before the first launch of a job you wrote or edited (job.run with dry_run).
3. Ask the user before a destructive action. Such an action answers "validation" with details.confirm_required and a message that names what it deletes; repeat the call with confirm: true once the user agrees.

Call system.guide with topic mcp-basics before your first workflow, and system.guides for the index. Always pass workspace explicitly on workspace-scoped calls.`

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
				body, err = entry.transform(args.Input, status, body)
				if err != nil {
					return mcpErrorResult("internal", "transform: "+err.Error()), nil
				}
			}
			if entry.bounded {
				if body, err = boundedBody(args.Input, body, entry.tail); err != nil {
					return mcpErrorResult("validation", err.Error()), nil
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

// mcpDocsResult returns the field reference for one tool: a generated
// signature block, then the embedded prose.
func (s *Server) mcpDocsResult(tool string) (*mcp.CallToolResult, error) {
	doc, err := mcpDocsFS.ReadFile("mcpdocs/" + tool + ".md")
	if err != nil {
		return mcpErrorResult("internal", "no docs for tool "+tool), nil
	}
	return mcpTextResult([]byte(docSignatures(string(doc)) + string(doc))), nil
}

// docSignatures renders one line per documented action, so a client reads the
// whole call shape of a tool at a glance. A ? marks an optional field.
func docSignatures(doc string) string {
	var b strings.Builder
	b.WriteString("## Signatures\n\n```\n")
	for _, action := range docActions(doc) {
		names := make([]string, 0, 8)
		for _, f := range docFields(docSection(doc, action)) {
			if f.required {
				names = append(names, f.name)
				continue
			}
			names = append(names, f.name+"?")
		}
		b.WriteString(action + "(" + strings.Join(names, ", ") + ")\n")
	}
	b.WriteString("docs()\n```\n\n")
	return b.String()
}

// docActions lists the action names of one tool document, in document order.
func docActions(doc string) []string {
	var out []string
	for _, line := range strings.Split(doc, "\n") {
		if name, ok := strings.CutPrefix(line, "### "); ok {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

// docField is one input field of one documented action.
type docField struct {
	name     string
	typ      string
	required bool
}

var docFieldRE = regexp.MustCompile(`^- ` + "`" + `([a-z_]+)` + "`" + ` \(([\w ]+?), (required|optional)([^)]*)\)`) //nolint:gochecknoglobals // compiled pattern

// docSection returns the body of one "### <action>" section.
func docSection(doc, action string) string {
	_, rest, ok := strings.Cut(doc, "### "+action+"\n")
	if !ok {
		return ""
	}
	if next := strings.Index(rest, "\n### "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// docFields parses the field list of one action section. A conditional
// "required for owner job" is not a bare requirement, so it reads as optional.
func docFields(section string) []docField {
	var out []docField
	for _, line := range strings.Split(section, "\n") {
		m := docFieldRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		out = append(out, docField{name: m[1], typ: m[2], required: m[3] == "required" && m[4] == ""})
	}
	return out
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
func busPollTransform(_ mcpInput, _ int, body []byte) ([]byte, error) {
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

// fileGetTransform wraps a non-text file body as base64 so a binary read is
// not mangled into replacement characters. Text passes through unchanged.
func fileGetTransform(_ mcpInput, _ int, body []byte) ([]byte, error) {
	if utf8.Valid(body) && !bytes.ContainsRune(body, 0) {
		return body, nil
	}
	return json.Marshal(map[string]any{
		"content":  base64.StdEncoding.EncodeToString(body),
		"encoding": "base64",
	})
}

// boundedBody caps one response at max_bytes. A cut body is no longer valid
// JSON, so it comes back wrapped and marked.
func boundedBody(in mcpInput, body []byte, tail bool) ([]byte, error) {
	cut, truncated, err := in.clampBody(body, tail)
	if err != nil {
		return nil, err
	}
	if !truncated {
		return body, nil
	}
	return json.Marshal(map[string]any{"content": string(cut), "truncated": true})
}

// sseLines pulls the `data:` payloads out of one SSE dump, newest last.
func sseLines(body []byte) []string {
	out := []string{}
	for _, line := range strings.Split(string(body), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue // keepalive comments and blank separators
		}
		if text := strings.TrimSpace(data); text != "" && text != "{}" {
			out = append(out, text)
		}
	}
	return out
}

// logLinesTransform rewrites one SSE log dump into {"lines": [...]}. Without
// it the model reads raw `data: {...}` text. The `lines` input bounds the
// tail; absent it keeps everything the handler sent.
func logLinesTransform(in mcpInput, _ int, body []byte) ([]byte, error) {
	lines := sseLines(body)
	n, ok, err := in.num("lines")
	if err != nil {
		return nil, err
	}
	res := map[string]any{"lines": lines}
	if ok && n > 0 && len(lines) > n {
		res["lines"] = lines[len(lines)-n:]
		res["truncated"] = true
	}
	return json.Marshal(res)
}
