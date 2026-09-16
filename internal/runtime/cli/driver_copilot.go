package cli

import "context"

// CopilotDriver speaks Agent Client Protocol over stdio (`copilot --acp`).
type CopilotDriver struct{}

// Name implements Driver.
func (CopilotDriver) Name() string { return "copilot" }

// Start implements Driver.
func (CopilotDriver) Start(ctx context.Context, req StartRequest) (Session, error) {
	return acpStart(ctx, req, acpCopilot)
}

// Resume implements Driver.
func (CopilotDriver) Resume(ctx context.Context, req StartRequest) (Session, error) {
	return acpStart(ctx, req, acpCopilot)
}
