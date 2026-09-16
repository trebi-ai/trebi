package cli

import "context"

// CursorDriver speaks Agent Client Protocol (`cursor-agent acp`), with the
// print/stream-json fallback left to a later phase.
type CursorDriver struct{}

// Name implements Driver.
func (CursorDriver) Name() string { return "cursor" }

// Start implements Driver.
func (CursorDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return acpStart(ctx, req, acpCursor)
}

// Resume implements Driver.
func (CursorDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return acpStart(ctx, req, acpCursor)
}
