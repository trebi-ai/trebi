package daemon

import "context"

// TurnWatchTick runs one turn-watch pass, so a test needs no restart.
func (c *ControlPlane) TurnWatchTick(ctx context.Context) { c.turnWatchTick(ctx) }
