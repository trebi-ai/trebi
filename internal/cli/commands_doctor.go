package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/guides"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/urfave/cli/v3"
)

func guideCommand() *cli.Command {
	return &cli.Command{
		Name:      "guide",
		Usage:     "print a workflow guide (works with the daemon down)",
		ArgsUsage: "[topic]",
		Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
		Action:    runGuide,
	}
}

func runGuide(_ context.Context, c *cli.Command) error {
	topic := strings.TrimSpace(c.Args().Get(0))
	if topic == "" {
		return runGuideList(c)
	}
	g, err := guides.Get(topic)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]string{
			"topic": g.Topic, "title": g.Title, "description": g.Description, "body": g.Body,
		})
	}
	_, err = fmt.Fprint(outWriter(c), g.Body)
	return err
}

func runGuideList(c *cli.Command) error {
	list := guides.List()
	if c.Bool("json") {
		items := make([]map[string]string, 0, len(list))
		for _, g := range list {
			items = append(items, map[string]string{
				"topic": g.Topic, "title": g.Title, "description": g.Description,
			})
		}
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"items": items})
	}
	tbl := newTable(outWriter(c), []string{"Topic", "Title", "Description"})
	for _, g := range list {
		tbl.AppendRow(table.Row{g.Topic, g.Title, g.Description})
	}
	renderTable(tbl)
	return nil
}

func doctorCommand() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "system doctor findings",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "emit JSON"},
			&cli.BoolFlag{Name: "fix", Usage: "apply automatic fixes for every finding"},
		},
		Action: runDoctor,
		Commands: []*cli.Command{
			{
				Name:      "fix",
				Usage:     "apply the automatic fix for one finding",
				ArgsUsage: "<id>",
				Action:    runDoctorFixOne,
			},
		},
	}
}

func runDoctor(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return renderDoctorOffline(c, err)
	}
	var body map[string]any
	if err := client.GetJSON(request.BuildSystemDoctorPath(), &body); err != nil {
		return err
	}
	if c.Bool("fix") {
		return applyDoctorFixes(c, client, body)
	}
	if c.Bool("json") {
		enc := json.NewEncoder(outWriter(c))
		enc.SetIndent("", "  ")
		return enc.Encode(body)
	}
	return renderDoctor(outWriter(c), body)
}

func renderDoctorOffline(c *cli.Command, clientErr error) error {
	w := outWriter(c)
	db := "present"
	if _, err := os.Stat(home.DBPath()); err != nil {
		db = "missing"
	}
	info := home.DoctorDaemon()
	if c.Bool("json") {
		enc := json.NewEncoder(w)
		if err := enc.Encode(map[string]any{
			"home":               home.Base(),
			"db":                 db,
			"error":              "daemon unavailable",
			"daemon_lock_held":   info.LockHeld,
			"daemon_lock_pid":    info.LockPID,
			"api_json_url":       info.APIJSONURL,
			"api_json_reachable": info.APIJSONReachable,
		}); err != nil {
			return err
		}
		return fmt.Errorf("daemon unavailable")
	}
	fmt.Fprintf(w, "home=%s\n", home.Base())
	if db == "missing" {
		fmt.Fprintln(w, "db: missing (run trebi service migrate-db)")
	} else {
		fmt.Fprintln(w, "db: present")
	}
	printDoctorDaemon(w, info.LockHeld, info.LockPID, info.APIJSONURL, info.APIJSONReachable)
	_ = clientErr
	return nil
}

func doctorPID(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func printDoctorDaemon(w io.Writer, held bool, pid int, apiURL string, reachable bool) {
	switch {
	case held && pid > 0:
		fmt.Fprintf(w, "daemon.lock: pid %d\n", pid)
	case held:
		fmt.Fprintln(w, "daemon.lock: held")
	default:
		fmt.Fprintln(w, "daemon.lock: not held")
	}
	if apiURL == "" {
		fmt.Fprintln(w, "api.json: missing")
		return
	}
	if reachable {
		fmt.Fprintf(w, "api.json: %s\n", apiURL)
		return
	}
	fmt.Fprintf(w, "api.json: stale (%s does not answer /v1/system/health)\n", apiURL)
}

func renderDoctor(w io.Writer, body map[string]any) error {
	printDoctorDaemon(w,
		truthy(body["daemon_lock_held"]),
		doctorPID(body["daemon_lock_pid"]),
		mapStr(body, "api_json_url"),
		truthy(body["api_json_reachable"]),
	)
	if findings, ok := body["findings"].([]any); ok && len(findings) > 0 {
		tbl := newTable(w, []string{"ID", "Severity", "Code", "Message"})
		for _, raw := range findings {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			tbl.AppendRow(table.Row{
				dash(mapStr(m, "id")),
				mapStr(m, "severity", "level", "status"),
				mapStr(m, "code", "name"),
				mapStr(m, "message", "detail", "msg"),
			})
		}
		renderTable(tbl)
		return nil
	}
	tbl := newTable(w, []string{"Key", "Value"})
	skip := map[string]bool{
		"findings": true, "daemon_lock_held": true, "daemon_lock_pid": true,
		"api_json_url": true, "api_json_reachable": true,
	}
	for k, v := range body {
		if skip[k] {
			continue
		}
		tbl.AppendRow(table.Row{k, fmt.Sprint(v)})
	}
	renderTable(tbl)
	return nil
}

func applyDoctorFixes(c *cli.Command, client *APIClient, body map[string]any) error {
	findings, _ := body["findings"].([]any)
	w := outWriter(c)
	var results []map[string]any
	for _, raw := range findings {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := mapStr(m, "id")
		if id == "" {
			continue
		}
		line, result := doctorFixOne(client, id)
		results = append(results, result)
		if !c.Bool("json") {
			fmt.Fprintln(w, line)
		}
	}
	if c.Bool("json") {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}
	return nil
}

func runDoctorFixOne(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi doctor fix <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	line, _ := doctorFixOne(client, id)
	fmt.Fprintln(outWriter(c), line)
	return nil
}

func doctorFixOne(client *APIClient, id string) (string, map[string]any) {
	var out map[string]any
	if err := client.DoHTTP(request.BuildDoctorFix(id), &out); err != nil {
		return fmt.Sprintf("%s: error: %v", id, err), map[string]any{"id": id, "error": err.Error()}
	}
	if fixed, ok := out["fixed"]; ok {
		switch v := fixed.(type) {
		case bool:
			if !v {
				note := mapStr(out, "note")
				return fmt.Sprintf("%s: skip (%s)", id, dash(note)), out
			}
		case string:
			if v == "false" {
				return fmt.Sprintf("%s: skip (%s)", id, dash(mapStr(out, "note"))), out
			}
		}
	}
	action := mapStr(out, "action")
	if action == "" {
		action = "fixed"
	}
	return fmt.Sprintf("%s: %s", id, action), out
}
