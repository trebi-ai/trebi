package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/urfave/cli/v2"
)

func agentHookCommand() *cli.Command {
	return &cli.Command{
		Name:      "agent-hook",
		Usage:     "harness lifecycle hook (called by tools; prints {})",
		Hidden:    true,
		ArgsUsage: "<event>",
		Action: func(c *cli.Context) error {
			event := c.Args().Get(0)
			if event == "" {
				event = "running"
			}
			// Prefer HTTP to local daemon when available.
			if client, err := clientFor(c); err == nil {
				runID := os.Getenv("TREBI_RUN_ID")
				body, readErr := io.ReadAll(os.Stdin)
				if readErr != nil {
					body = nil // hook delivery remains best effort and stdout must stay valid JSON
				}
				path := "/v1/agent-hook/" + event
				if runID != "" {
					path += "?run_id=" + runID
				}
				reqBody := map[string]any{"payload": string(body)}
				_ = client.PostJSON(path, reqBody, nil) // hook is best effort; stdout must remain {}
			}
			// Claude requires JSON on stdout
			fmt.Print("{}")
			return nil
		},
	}
}
