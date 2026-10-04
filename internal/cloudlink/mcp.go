package cloudlink

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const (
	mcpBodyCap   = 4 << 20 // one MCP body, each direction
	mcpSlotCount = 8
	mcpTimeout   = 300 * time.Second
)

// JSON-RPC error codes. The cloud uses the same codes for the errors it builds.
const (
	mcpCodeOff     = -32002
	mcpCodeTimeout = -32003
	mcpCodeBusy    = -32004
)

// handleMcp serves one forwarded MCP request and sends the reply. It runs
// off the read loop, so a slow tool call does not stop heartbeats.
func (sk *socket) handleMcp(ctx context.Context, f McpRequest) {
	resp := sk.serveMcp(ctx, f)
	resp.V, resp.Type, resp.ID = ContractV, "mcp_response", f.ID
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	if err := sk.writeFrame(resp); err != nil {
		sk.svc.log.Warn("cloudlink.mcp", "id", f.ID, "err", err)
	}
}

func (sk *socket) serveMcp(ctx context.Context, f McpRequest) McpResponse {
	if !sk.svc.remoteMCP() {
		return mcpError(f, mcpCodeOff, "Remote access is turned off on this machine. Turn it on in Trebi settings.")
	}
	if len(f.Body) > mcpBodyCap {
		r := mcpError(f, mcpCodeBusy, "The request is too large.")
		r.Status = http.StatusRequestEntityTooLarge
		return r
	}
	select {
	case sk.mcpSlots <- struct{}{}:
		defer func() { <-sk.mcpSlots }()
	default:
		return mcpError(f, mcpCodeBusy, "Your Trebi machine is busy. Try again.")
	}
	ctx, cancel := context.WithTimeout(ctx, mcpTimeout)
	defer cancel()
	done := make(chan McpResponse, 1)
	// Stop condition: the sink returns; ctx ends only stops the wait.
	go func() { done <- sk.svc.mcp.ServeRemoteMCP(ctx, f) }()
	var resp McpResponse
	select {
	case resp = <-done:
	case <-ctx.Done():
		return mcpError(f, mcpCodeTimeout, "The tool call took too long.")
	}
	if len(resp.Body) > mcpBodyCap {
		r := mcpError(f, mcpCodeBusy, "The reply is too large.")
		r.Status = http.StatusBadGateway
		return r
	}
	return resp
}

// mcpError builds a JSON-RPC error reply for the request id. A
// notification has no id and gets 202 with no body.
func mcpError(f McpRequest, code int, msg string) McpResponse {
	var head struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal([]byte(f.Body), &head)
	if len(head.ID) == 0 || string(head.ID) == "null" {
		return McpResponse{Status: http.StatusAccepted, Headers: map[string]string{}}
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      head.ID,
		"error":   map[string]any{"code": code, "message": msg},
	})
	return McpResponse{
		Status:  http.StatusOK,
		Headers: map[string]string{"content-type": "application/json"},
		Body:    string(body),
	}
}
