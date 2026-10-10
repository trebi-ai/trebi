package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func harnessCommand() *cli.Command {
	return &cli.Command{
		Name:  "harness",
		Usage: "harness tools",
		Commands: append([]*cli.Command{
			{
				Name:  "list",
				Usage: "list harnesses",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runHarnessList,
			},
			{
				Name:      "enable",
				Usage:     "turn on a harness",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "mode", Usage: "shared|separate"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					body := map[string]any{"enabled": true}
					if m := c.String("mode"); m != "" {
						body["auth_mode"] = m
					}
					return runHarnessPatch(c, body)
				},
			},
			{
				Name:      "disable",
				Usage:     "turn off a harness",
				ArgsUsage: "<name>",
				Action: func(_ context.Context, c *cli.Command) error {
					return runHarnessPatch(c, map[string]any{"enabled": false})
				},
			},
			{
				Name:      "set",
				Usage:     "set the default model or effort of a harness (\"\" clears)",
				ArgsUsage: "<name>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "model", Usage: "default model"},
					&cli.StringFlag{Name: "effort", Usage: "default effort level"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					body := map[string]any{}
					for _, k := range []string{"model", "effort"} {
						if c.IsSet(k) {
							body[k] = c.String(k)
						}
					}
					if len(body) == 0 {
						return fmt.Errorf("set --model or --effort")
					}
					return runHarnessPatch(c, body)
				},
			},
			{
				Name:      "providers",
				Usage:     "list the OpenCode model providers",
				ArgsUsage: "[name]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runHarnessProviders,
			},
			harnessFilesCommand(),
			{
				Name:  "usage",
				Usage: "show the plan limits of each harness account",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "refresh", Usage: "read the Codex limits now"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
				},
				Action: runHarnessUsage,
			},
			{
				Name:      "ensure",
				Usage:     "ensure harness home + lifecycle install",
				ArgsUsage: "<name>",
				Action: func(_ context.Context, c *cli.Command) error {
					name := c.Args().Get(0)
					if name == "" {
						return fmt.Errorf("name required")
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var out map[string]any
					if err := client.DoHTTP(request.BuildHarnessEnsure(name), &out); err != nil {
						return err
					}
					fmt.Fprintf(outWriter(c), "ensured %s home=%v\n", name, out["home"])
					return nil
				},
			},
			{
				Name:  "auth",
				Usage: "probe and manage harness authentication",
				Commands: []*cli.Command{
					{
						Name:      "status",
						Usage:     "auth probe (ok|missing|expired|unknown)",
						ArgsUsage: "[name]",
						Flags: []cli.Flag{
							&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
							&cli.BoolFlag{Name: "live", Usage: "run the probe now"},
						},
						Action: runHarnessAuthStatus,
					},
					{
						Name:      "login",
						Usage:     "start vendor login (remote device/paste or --token-stdin)",
						ArgsUsage: "<name>",
						Flags: []cli.Flag{
							&cli.BoolFlag{Name: "token-stdin", Usage: "read API key from stdin"},
							&cli.BoolFlag{Name: "attach", Usage: "attach the login TUI over WebSocket"},
							&cli.StringFlag{Name: "mode", Usage: "shared|separate (PATCH first)"},
							&cli.StringFlag{Name: "provider", Usage: "OpenCode provider id (see: trebi harness providers opencode)"},
						},
						Action: runHarnessAuthLogin,
					},
					{
						Name:      "logout",
						Usage:     "clear credentials (mode-aware)",
						ArgsUsage: "<name>",
						Flags: []cli.Flag{
							&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "confirm shared logout"},
							&cli.StringFlag{Name: "mode", Usage: "shared|separate"},
						},
						Action: runHarnessAuthLogout,
					},
					{
						Name:      "cancel",
						Usage:     "cancel a pending login session",
						ArgsUsage: "<name>",
						Action: func(_ context.Context, c *cli.Command) error {
							client, err := clientFor(c)
							if err != nil {
								return err
							}
							return client.DoHTTP(request.BuildHarnessLoginCancel(c.Args().Get(0)), nil)
						},
					},
					{
						Name:      "mode",
						Usage:     "set auth mode shared|separate",
						ArgsUsage: "<name> <shared|separate>",
						Action:    runHarnessAuthMode,
					},
				},
			},
		}, harnessVersionCommands()...),
	}
}

func runHarnessList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Harnesses []map[string]any `json:"harnesses"`
	}
	if err := client.GetJSON(request.BuildHarnessesListPath(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Name", "On", "Installed", "Mode", "Status", "Account", "Bin"})
	for _, h := range body.Harnesses {
		if truthy(h["hidden"]) && !truthy(h["enabled"]) {
			continue
		}
		on := "off"
		if truthy(h["enabled"]) {
			on = "on"
		}
		tbl.AppendRow(table.Row{
			mapStr(h, "name"),
			on,
			mapStr(h, "installed"),
			dash(mapStr(h, "auth_mode")),
			dash(mapStr(h, "auth_status")),
			dash(mapStr(h, "auth_account")),
			dash(mapStr(h, "bin_path")),
		})
	}
	renderTable(tbl)
	return nil
}

func runHarnessPatch(c *cli.Command, body map[string]any) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildHarnessPatch(name, body), &out); err != nil {
		return err
	}
	on := "off"
	if truthy(out["enabled"]) {
		on = "on"
	}
	fmt.Fprintf(outWriter(c), "%s %s mode=%v status=%v\n", name, on, out["auth_mode"], out["auth_status"])
	return nil
}

func runHarnessProviders(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		name = "opencode"
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := client.DoHTTP(request.BuildHarnessProviders(name), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"ID", "Provider", "Logged in", "Methods", "Key URL"})
	for _, p := range body.Providers {
		methods := []string{}
		if list, ok := p["methods"].([]any); ok {
			for _, m := range list {
				methods = append(methods, fmt.Sprint(m))
			}
		}
		tbl.AppendRow(table.Row{mapStr(p, "id"), mapStr(p, "label"), mapStr(p, "logged_in"), strings.Join(methods, ", "), dash(mapStr(p, "key_url"))})
	}
	renderTable(tbl)
	return nil
}

func runHarnessUsage(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	req := request.BuildHarnessUsage()
	if c.Bool("refresh") {
		req = request.BuildHarnessUsageRefresh()
	}
	var body struct {
		Harnesses []struct {
			Harness string `json:"harness"`
			Plan    string `json:"plan"`
			Windows []struct {
				Key         string  `json:"key"`
				Scope       string  `json:"scope"`
				UsedPercent float64 `json:"used_percent"`
				ResetsAt    string  `json:"resets_at"`
				ObservedAt  string  `json:"observed_at"`
			} `json:"windows"`
		} `json:"harnesses"`
		RefreshError string `json:"refresh_error,omitempty"`
	}
	if err := client.DoHTTP(req, &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	w := outWriter(c)
	if body.RefreshError != "" {
		fmt.Fprintf(w, "refresh failed: %s\n", body.RefreshError)
	}
	tbl := newTable(w, []string{"Harness", "Window", "Used", "Resets in", "As of"})
	for _, h := range body.Harnesses {
		for _, win := range h.Windows {
			name := win.Key
			if win.Scope != "" {
				name += " (" + win.Scope + ")"
			}
			resets := "-"
			if t, err := time.Parse(time.RFC3339, win.ResetsAt); err == nil {
				resets = time.Until(t).Round(time.Minute).String()
			}
			asOf := win.ObservedAt
			if t, err := time.Parse(time.RFC3339, win.ObservedAt); err == nil {
				asOf = t.Local().Format("Jan 2 15:04")
			}
			tbl.AppendRow(table.Row{h.Harness, name, fmt.Sprintf("%.0f%%", win.UsedPercent), resets, asOf})
		}
	}
	renderTable(tbl)
	return nil
}

func runHarnessAuthStatus(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	live := c.Bool("live")
	name := c.Args().Get(0)
	var rows []map[string]any
	if name != "" {
		var body map[string]any
		if err := client.DoHTTP(request.BuildHarnessAuthGet(name, live), &body); err != nil {
			return err
		}
		rows = []map[string]any{body}
	} else {
		var list struct {
			Harnesses []map[string]any `json:"harnesses"`
		}
		if err := client.GetJSON(request.BuildHarnessesListPath(), &list); err != nil {
			return err
		}
		if live {
			for _, h := range list.Harnesses {
				n := mapStr(h, "name", "Name")
				var body map[string]any
				if err := client.DoHTTP(request.BuildHarnessAuthGet(n, true), &body); err != nil {
					rows = append(rows, map[string]any{"name": n, "auth_status": "error", "detail": err.Error()})
					continue
				}
				rows = append(rows, body)
			}
		} else {
			rows = list.Harnesses
		}
	}
	if c.Bool("json") {
		return encodeJSON(c, rows)
	}
	w := outWriter(c)
	tbl := newTable(w, []string{"Tool", "Mode", "Status", "Expires", "Detail", "Hint"})
	bad := false
	for _, r := range rows {
		st := mapStr(r, "auth_status", "status")
		if st != "ok" && st != "" {
			bad = true
		}
		hint := mapStr(r, "hint")
		if st == "ok" {
			hint = ""
		}
		tbl.AppendRow(table.Row{
			mapStr(r, "name", "tool", "Name"),
			dash(mapStr(r, "auth_mode", "mode")),
			dash(st),
			dash(mapStr(r, "auth_expires_at", "expires_at")),
			dash(mapStr(r, "auth_detail", "detail")),
			dash(hint),
		})
	}
	renderTable(tbl)
	for _, r := range rows {
		st := mapStr(r, "auth_status", "status")
		if truthy(r["keychain_bound"]) && st != "ok" && st != "" {
			fmt.Fprintln(w, "Claude did not pick up your macOS login — run: trebi harness auth login claude, or provide an API key")
			break
		}
	}
	if name != "" && bad {
		return fmt.Errorf("auth not ok")
	}
	return nil
}

func runHarnessAuthMode(_ context.Context, c *cli.Command) error {
	name, mode := c.Args().Get(0), c.Args().Get(1)
	if name == "" || (mode != "shared" && mode != "separate") {
		return fmt.Errorf("usage: trebi harness auth mode <name> shared|separate")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(request.BuildHarnessPatch(name, map[string]any{"auth_mode": mode}), &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "%s mode=%v status=%v\n", name, out["auth_mode"], out["auth_status"])
	return nil
}

func runHarnessAuthLogout(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(request.BuildHarnessLogout(name, c.String("mode"), c.Bool("yes")), nil)
}

func runHarnessAuthLogin(_ context.Context, c *cli.Command) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	mode := c.String("mode")
	if c.Bool("token-stdin") {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading token: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return fmt.Errorf("empty token on stdin")
		}
		var out map[string]any
		if err := client.DoHTTP(request.BuildHarnessLoginToken(name, token, c.String("provider")), &out); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "%s status=%v\n", name, out["auth_status"])
		return nil
	}
	if c.Bool("attach") {
		if err := client.DoHTTP(request.BuildHarnessLoginStart(name, mode, c.String("provider")), nil); err != nil {
			return renderLocalOnly(c, err)
		}
		return attachLoginWS(c, client, name)
	}
	var sess map[string]any
	if err := client.DoHTTP(request.BuildHarnessLoginStart(name, mode, c.String("provider")), &sess); err != nil {
		return renderLocalOnly(c, err)
	}
	return driveLogin(c, client, name, sess)
}

func driveLogin(c *cli.Command, client *APIClient, name string, sess map[string]any) error {
	w := outWriter(c)
	printedURL := ""
	for {
		phase := mapStr(sess, "phase")
		url := mapStr(sess, "url")
		code := mapStr(sess, "user_code")
		if url != "" && url != printedURL {
			fmt.Fprintf(w, "Open: %s\n", url)
			printedURL = url
		}
		if code != "" {
			fmt.Fprintf(w, "Code: %s\n", code)
		}
		if phase == "" && !truthy(sess["active"]) {
			return fmt.Errorf("login session ended")
		}
		switch phase {
		case "done":
			fmt.Fprintf(w, "%s login complete\n", name)
			return nil
		case "failed":
			if hint := mapStr(sess, "hint"); hint != "" {
				fmt.Fprintf(w, "log in on the daemon: %s\n", hint)
			}
			if cmd := mapStr(sess, "local_cmd"); cmd != "" {
				fmt.Fprintf(w, "  run: %s\n", cmd)
			}
			return fmt.Errorf("login failed: %s", mapStr(sess, "error"))
		case "awaiting_code":
			fmt.Fprint(w, "paste code: ")
			sc := bufio.NewScanner(os.Stdin)
			if !sc.Scan() {
				return fmt.Errorf("no code")
			}
			if err := client.DoHTTP(request.BuildHarnessLoginCode(name, strings.TrimSpace(sc.Text())), &sess); err != nil {
				return err
			}
			continue
		}
		time.Sleep(500 * time.Millisecond)
		if err := client.DoHTTP(request.BuildHarnessLoginGet(name), &sess); err != nil {
			return err
		}
		if !truthy(sess["active"]) && phase != "done" && phase != "failed" && phase != "" {
			// session dropped
			if phase == "starting" || phase == "awaiting_browser" || phase == "verifying" {
				continue
			}
			return fmt.Errorf("login session ended")
		}
	}
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	}
	return false
}

func renderLocalOnly(c *cli.Command, err error) error {
	msg := err.Error()
	if strings.Contains(msg, "precondition_failed") || strings.Contains(msg, "daemon machine") {
		fmt.Fprintf(outWriter(c), "log in on the daemon machine (this harness has no remote relay)\n")
	}
	return err
}

func attachLoginWS(c *cli.Command, client *APIClient, name string) error {
	conn, err := client.DialWS("/v1/harnesses/" + name + "/login/attach")
	if err != nil {
		return err
	}
	defer conn.Close()
	w := outWriter(c)
	fmt.Fprintf(w, "attached to %s login (WebSocket). Ctrl-C to detach.\n", name)
	a := &wsAttach{conn: conn, out: w}
	done := make(chan struct{})
	errCh := make(chan error, 1)
	go a.readMessages(errCh, done)
	select {
	case <-done:
		return nil
	case err := <-errCh:
		if attachEndOK(err) {
			return nil
		}
		return err
	}
}

func agentHookCommand() *cli.Command {
	return &cli.Command{
		Name:      "agent-hook",
		Usage:     "harness lifecycle hook (called by tools; prints {})",
		Hidden:    true,
		ArgsUsage: "<event>",
		Action: func(_ context.Context, c *cli.Command) error {
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

// harnessFilesCommand manages the vendor config files: yours or Trebi's own.
func harnessFilesCommand() *cli.Command {
	nameKey := func(c *cli.Command) (string, string, error) {
		name, key := c.Args().Get(0), c.Args().Get(1)
		if name == "" || key == "" {
			return "", "", fmt.Errorf("name and key required")
		}
		return name, key, nil
	}
	return &cli.Command{
		Name:  "files",
		Usage: "show and edit the vendor config files that runs use",
		Commands: []*cli.Command{
			{
				Name:      "list",
				Usage:     "list the config files of a harness",
				ArgsUsage: "<name>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: func(_ context.Context, c *cli.Command) error {
					name := c.Args().Get(0)
					if name == "" {
						return fmt.Errorf("name required")
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body struct {
						Files []map[string]any `json:"files"`
					}
					if err := client.DoHTTP(request.BuildHarnessFiles(name), &body); err != nil {
						return err
					}
					if c.Bool("json") {
						return encodeJSON(c, body)
					}
					tbl := newTable(outWriter(c), []string{"Key", "File", "Mode", "Changed", "Path"})
					for _, f := range body.Files {
						path := mapStr(f, "user_path")
						if mapStr(f, "mode") == "own" {
							path = mapStr(f, "trebi_path")
						}
						changed := ""
						if truthy(f["user_changed_since_own"]) {
							changed = "your file changed"
						}
						tbl.AppendRow(table.Row{mapStr(f, "key"), mapStr(f, "label"), mapStr(f, "mode"), dash(changed), path})
					}
					renderTable(tbl)
					return nil
				},
			},
			{
				Name:      "show",
				Usage:     "print one config file",
				ArgsUsage: "<name> <key>",
				Flags:     []cli.Flag{&cli.StringFlag{Name: "side", Usage: "user|trebi (default: the file that runs use)"}},
				Action: func(_ context.Context, c *cli.Command) error {
					name, key, err := nameKey(c)
					if err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var body struct {
						Content string `json:"content"`
					}
					if err := client.DoHTTP(request.BuildHarnessFileRead(name, key, c.String("side")), &body); err != nil {
						return err
					}
					_, err = fmt.Fprint(outWriter(c), body.Content)
					return err
				},
			},
			{
				Name:      "own",
				Usage:     "make Trebi's own copy, so that runs use it",
				ArgsUsage: "<name> <key>",
				Flags:     []cli.Flag{&cli.StringFlag{Name: "start", Usage: "user|empty|backup", Value: "user"}},
				Action: func(_ context.Context, c *cli.Command) error {
					name, key, err := nameKey(c)
					if err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildHarnessFileOwn(name, key, c.String("start")), nil)
				},
			},
			{
				Name:      "write",
				Usage:     "replace Trebi's copy from a file or stdin",
				ArgsUsage: "<name> <key>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Usage: "read the content from this file (default: stdin)"},
					&cli.StringFlag{Name: "etag", Usage: "refuse the write when the file changed"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					name, key, err := nameKey(c)
					if err != nil {
						return err
					}
					content, err := readFileOrStdin(c.String("file"))
					if err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildHarnessFileWrite(name, key, content, c.String("etag")), nil)
				},
			},
			{
				Name:      "reset",
				Usage:     "go back to your file; Trebi keeps its copy as a backup for 30 days",
				ArgsUsage: "<name> <key>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "confirm the reset"}},
				Action: func(_ context.Context, c *cli.Command) error {
					name, key, err := nameKey(c)
					if err != nil {
						return err
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					return client.DoHTTP(request.BuildHarnessFileReset(name, key, c.Bool("yes")), nil)
				},
			},
		},
	}
}
