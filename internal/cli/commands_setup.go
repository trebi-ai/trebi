package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/trebi-ai/trebi/internal/onboarding"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func setupCommand() *cli.Command {
	jsonFlag := &cli.BoolFlag{Name: "json", Usage: "emit JSON"}
	return &cli.Command{
		Name:   "setup",
		Usage:  "show or change the first-run setup",
		Flags:  []cli.Flag{jsonFlag},
		Action: runSetupStatus,
		Commands: []*cli.Command{
			{Name: "status", Usage: "show the setup state", Flags: []cli.Flag{jsonFlag}, Action: runSetupStatus},
			{
				Name:  "update",
				Usage: "change the setup state",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "step", Usage: "you, ai, first_job, or done"},
					&cli.BoolFlag{Name: "completed", Usage: "end the setup (--completed=false clears it)"},
					&cli.StringSliceFlag{Name: "hide", Usage: "hide a Home card: first_run, set_up, or pro"},
					&cli.StringSliceFlag{Name: "unhide", Usage: "show a Home card again"},
					&cli.StringFlag{Name: "first-job-id", Usage: "the first job that the user made"},
					&cli.IntFlag{Name: "revision", Usage: "the revision that you read; a stale one fails"},
					jsonFlag,
				},
				Action: runSetupUpdate,
			},
			{
				Name:  "defaults",
				Usage: "make the Personal workspace and turn on the only AI app that has a login",
				Flags: []cli.Flag{jsonFlag},
				Action: func(_ context.Context, c *cli.Command) error {
					var d onboarding.Defaults
					return runSetupCall(c, request.BuildOnboardingDefaults(), &d, func() string {
						out := "workspace: " + d.Workspace.Name + " (" + d.Workspace.Path + ")"
						switch {
						case d.Harness != "":
							out += "\nAI app: " + d.Harness + " is on"
						case d.Choose:
							out += "\nAI app: choose one"
						}
						return out
					})
				},
			},
			{
				Name:  "restart",
				Usage: "go back to the first step; keep the hidden cards, the workspace, and the first job",
				Flags: []cli.Flag{jsonFlag},
				Action: func(_ context.Context, c *cli.Command) error {
					var st onboarding.State
					return runSetupCall(c, request.BuildOnboardingRestart(), &st, func() string { return stateLine(st) })
				},
			},
		},
	}
}

func runSetupStatus(_ context.Context, c *cli.Command) error {
	var st onboarding.State
	return runSetupCall(c, request.HTTPRequest{Method: "GET", Path: request.BuildOnboardingGetPath()}, &st, func() string { return stateLine(st) })
}

func runSetupUpdate(_ context.Context, c *cli.Command) error {
	fields := map[string]any{}
	for flag, key := range map[string]string{"step": "step", "first-job-id": "first_job_id"} {
		if c.IsSet(flag) {
			fields[key] = c.String(flag)
		}
	}
	if c.IsSet("completed") {
		fields["completed"] = c.Bool("completed")
	}
	if c.IsSet("revision") {
		fields["revision"] = c.Int("revision")
	}
	for _, key := range []string{"hide", "unhide"} {
		if c.IsSet(key) {
			fields[key] = c.StringSlice(key)
		}
	}
	if len(fields) == 0 {
		return fmt.Errorf("set --step, --completed, --hide, --unhide, or --first-job-id")
	}
	var st onboarding.State
	return runSetupCall(c, request.BuildOnboardingPatch(fields), &st, func() string { return stateLine(st) })
}

// runSetupCall sends r, decodes the reply into out, and prints it.
func runSetupCall(c *cli.Command, r request.HTTPRequest, out any, text func() string) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(r, out); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, out)
	}
	fmt.Fprintln(outWriter(c), text())
	return nil
}

func stateLine(st onboarding.State) string {
	hidden := make([]string, len(st.Hidden))
	for i, h := range st.Hidden {
		hidden[i] = string(h)
	}
	out := fmt.Sprintf("step: %s\nrevision: %d", st.Step, st.Revision)
	if st.CompletedAt != nil {
		out += "\ndone: " + st.CompletedAt.Local().Format("2006-01-02 15:04")
	}
	if len(hidden) > 0 {
		out += "\nhidden: " + strings.Join(hidden, ", ")
	}
	return out
}
