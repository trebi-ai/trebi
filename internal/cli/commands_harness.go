package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/urfave/cli/v2"
)

func harnessCommand() *cli.Command {
	return &cli.Command{
		Name:  "harness",
		Usage: "harness tools",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list harnesses",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
					&cli.BoolFlag{Name: "check", Usage: "note missing homes (exit 1 if any absent)"},
				},
				Action: runHarnessList,
			},
			{
				Name:      "ensure",
				Usage:     "ensure harness home + lifecycle install",
				ArgsUsage: "<name>",
				Action: func(c *cli.Context) error {
					name := c.Args().Get(0)
					if name == "" {
						return fmt.Errorf("name required")
					}
					client, err := clientFor(c)
					if err != nil {
						return err
					}
					var out map[string]any
					if err := client.DoHTTP(BuildHarnessEnsure(name), &out); err != nil {
						return err
					}
					fmt.Fprintf(outWriter(c), "ensured %s home=%v\n", name, out["home"])
					return nil
				},
			},
			{
				Name:  "auth",
				Usage: "probe and manage harness authentication",
				Subcommands: []*cli.Command{
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
						Name:      "mode",
						Usage:     "set auth mode shared|separate",
						ArgsUsage: "<name> <shared|separate>",
						Action:    runHarnessAuthMode,
					},
				},
			},
		},
	}
}

func runHarnessList(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Harnesses []map[string]any `json:"harnesses"`
	}
	if err := client.GetJSON("/v1/harnesses", &body); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	w := outWriter(c)
	headers := []string{"Name", "Present", "Home", "Bin", "Mode", "Status"}
	if c.Bool("check") {
		headers = append(headers, "Note")
	}
	tbl := newTable(w, headers)
	bad := false
	for _, h := range body.Harnesses {
		present := mapStr(h, "present", "Present")
		if present == "" {
			present = "-"
		}
		row := table.Row{
			mapStr(h, "name", "Name"),
			colorStatus(w, present),
			dash(mapStr(h, "home", "Home")),
			dash(mapStr(h, "bin_path", "BinPath", "bin")),
			dash(mapStr(h, "auth_mode")),
			dash(mapStr(h, "auth_status")),
		}
		if c.Bool("check") {
			note := ""
			if present == "false" || present == "0" || present == "-" {
				note = "missing"
				bad = true
			}
			row = append(row, dash(note))
		}
		tbl.AppendRow(row)
	}
	renderTable(tbl)
	if c.Bool("check") && bad {
		return fmt.Errorf("one or more harnesses missing")
	}
	return nil
}

func runHarnessAuthStatus(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	live := c.Bool("live")
	name := c.Args().Get(0)
	var rows []map[string]any
	if name != "" {
		var body map[string]any
		if err := client.DoHTTP(BuildHarnessAuthGet(name, live), &body); err != nil {
			return err
		}
		rows = []map[string]any{body}
	} else {
		var list struct {
			Harnesses []map[string]any `json:"harnesses"`
		}
		if err := client.GetJSON("/v1/harnesses", &list); err != nil {
			return err
		}
		if live {
			for _, h := range list.Harnesses {
				n := mapStr(h, "name", "Name")
				var body map[string]any
				if err := client.DoHTTP(BuildHarnessAuthGet(n, true), &body); err != nil {
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
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
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
		if keychainBound(r) && st != "ok" && st != "" {
			fmt.Fprintln(w, "shared file bridge unavailable on macOS — run: trebi harness auth login claude, or provide an API key")
			break
		}
	}
	if name != "" && bad {
		return fmt.Errorf("auth not ok")
	}
	return nil
}

func keychainBound(r map[string]any) bool {
	switch v := r["keychain_bound"].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return false
}

func runHarnessAuthMode(c *cli.Context) error {
	name, mode := c.Args().Get(0), c.Args().Get(1)
	if name == "" || (mode != "shared" && mode != "separate") {
		return fmt.Errorf("usage: trebi harness auth mode <name> shared|separate")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := client.DoHTTP(BuildHarnessPatch(name, mode), &out); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "%s mode=%v status=%v\n", name, out["auth_mode"], out["auth_status"])
	return nil
}

func runHarnessAuthLogout(c *cli.Context) error {
	name := c.Args().Get(0)
	if name == "" {
		return fmt.Errorf("name required")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	return client.DoHTTP(BuildHarnessLogout(name, c.String("mode"), c.Bool("yes")), nil)
}

func runHarnessAuthLogin(c *cli.Context) error {
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
		if err := client.DoHTTP(BuildHarnessLoginToken(name, token), &out); err != nil {
			return err
		}
		fmt.Fprintf(outWriter(c), "%s status=%v\n", name, out["auth_status"])
		return nil
	}
	if c.Bool("attach") {
		if err := client.DoHTTP(BuildHarnessLoginStart(name, mode), nil); err != nil {
			return renderLocalOnly(c, err)
		}
		return attachLoginWS(c, client, name)
	}
	var sess map[string]any
	if err := client.DoHTTP(BuildHarnessLoginStart(name, mode), &sess); err != nil {
		return renderLocalOnly(c, err)
	}
	return driveLogin(c, client, name, sess)
}

func driveLogin(c *cli.Context, client *APIClient, name string, sess map[string]any) error {
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
			if err := client.DoHTTP(BuildHarnessLoginCode(name, strings.TrimSpace(sc.Text())), &sess); err != nil {
				return err
			}
			continue
		}
		time.Sleep(500 * time.Millisecond)
		if err := client.DoHTTP(BuildHarnessLoginGet(name), &sess); err != nil {
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

func renderLocalOnly(c *cli.Context, err error) error {
	msg := err.Error()
	if strings.Contains(msg, "precondition_failed") || strings.Contains(msg, "daemon machine") {
		fmt.Fprintf(outWriter(c), "log in on the daemon machine (this harness has no remote relay)\n")
	}
	return err
}

func attachLoginWS(c *cli.Context, client *APIClient, name string) error {
	conn, err := dialAttachPath(client, "/v1/harnesses/"+name+"/login/attach")
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
