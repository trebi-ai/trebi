package cli

import (
	"context"
	"errors"
)

// ErrResumeUnsupported reports a driver that cannot resume a session (the
// vendor no longer knows it, or the protocol has no resume). The runs layer
// surfaces this to the caller instead of faking a TUI.
var ErrResumeUnsupported = errors.New("cli: session resume not supported")

// CodexDriver speaks the codex app-server JSON-RPC protocol.
type CodexDriver struct{}

// Name implements Driver.
func (CodexDriver) Name() string { return "codex" }

// Start implements Driver. The pre-assigned session id is trebi-minted, not
// a vendor thread id: a start must open a new thread and adopt the id the
// handshake reports.
func (CodexDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	req.SessionID = ""
	return codexStart(ctx, req)
}

// Resume implements Driver: the session id is the stored vendor thread id.
func (CodexDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return codexStart(ctx, req)
}

// OpenCodeDriver manages sessions on a daemon-owned `opencode serve`.
type OpenCodeDriver struct{}

// Name implements Driver.
func (OpenCodeDriver) Name() string { return "opencode" }

// Start implements Driver.
func (OpenCodeDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return openCodeStart(ctx, req)
}

// Resume implements Driver.
func (OpenCodeDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return openCodeStart(ctx, req)
}

// PiDriver speaks `pi --mode rpc`.
type PiDriver struct{}

// Name implements Driver.
func (PiDriver) Name() string { return "pi" }

// Start implements Driver.
func (PiDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return piStart(ctx, req)
}

// Resume implements Driver.
func (PiDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return piStart(ctx, req)
}
