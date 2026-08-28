package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/urfave/cli/v2"
)

var errBusLimit = errors.New("bus frame limit reached")

func outWriter(c *cli.Context) io.Writer {
	if c != nil && c.App != nil && c.App.Writer != nil {
		return c.App.Writer
	}
	return os.Stdout
}

func readFileOrStdin(path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(b), nil
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
		Subcommands: []*cli.Command{
			{
				Name:      "fix",
				Usage:     "apply the automatic fix for one finding",
				ArgsUsage: "<id>",
				Action:    runDoctorFixOne,
			},
		},
	}
}

func runDoctor(c *cli.Context) error {
	client, err := clientFor(c)
	if err != nil {
		return renderDoctorOffline(c, err)
	}
	var body map[string]any
	if err := client.GetJSON("/v1/system/doctor", &body); err != nil {
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

func renderDoctorOffline(c *cli.Context, clientErr error) error {
	w := outWriter(c)
	db := "present"
	if _, err := os.Stat(home.DBPath()); err != nil {
		db = "missing"
	}
	if c.Bool("json") {
		enc := json.NewEncoder(w)
		if err := enc.Encode(map[string]any{
			"home":  home.Base(),
			"db":    db,
			"error": "daemon unavailable",
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
	_ = clientErr
	return nil
}

func renderDoctor(w io.Writer, body map[string]any) error {
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
	for k, v := range body {
		if k == "findings" {
			continue
		}
		tbl.AppendRow(table.Row{k, fmt.Sprint(v)})
	}
	renderTable(tbl)
	return nil
}

func applyDoctorFixes(c *cli.Context, client *APIClient, body map[string]any) error {
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

func runDoctorFixOne(c *cli.Context) error {
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
	if err := client.DoHTTP(BuildDoctorFix(id), &out); err != nil {
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

func busCommand() *cli.Command {
	return &cli.Command{
		Name:  "bus",
		Usage: "tail the event bus (live stream; not trigger events, not a complete audit trail)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "topics", Usage: "exact topic or family prefix, comma-separated, OR-ed (server-side; e.g. run or run.failed,alert)"},
			&cli.Int64Flag{Name: "cursor", Usage: "bus cursor (event_log.seq); resume from a known point (server-side)"},
			&cli.BoolFlag{Name: "replay", Usage: "include retained history (default is live frames only)"},
			&cli.StringFlag{Name: "since", Usage: "RFC3339 lower bound on ts (client-side, inclusive)"},
			&cli.StringFlag{Name: "entity", Usage: "match entity (run, job, alert, …) (client-side)"},
			&cli.StringFlag{Name: "id", Usage: "match entity id (client-side)"},
			&cli.StringFlag{Name: "action", Usage: "match action (created/updated/deleted) (client-side)"},
			&cli.IntFlag{Name: "limit", Usage: "exit after n rendered frames (client-side)"},
			&cli.BoolFlag{Name: "json", Usage: "JSONL (one raw frame per line)"},
		},
		Action: runBus,
	}
}

func runBus(c *cli.Context) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := runBusWith(ctx, c)
	if err == nil || errors.Is(err, errBusLimit) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func runBusWith(ctx context.Context, c *cli.Context) error {
	topics, err := normalizeBusTopics(c.String("topics"))
	if err != nil {
		return err
	}
	filt, err := busFilterFrom(c)
	if err != nil {
		return err
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	cursorSet := c.IsSet("cursor")
	cursor := ""
	if cursorSet {
		cursor = fmt.Sprintf("%d", c.Int64("cursor"))
	}
	tail := &busTail{
		w:     outWriter(c),
		jsonl: c.Bool("json"),
		limit: c.Int("limit"),
		filt:  filt,
	}
	streamErr := client.streamEvents(ctx, BuildStreamPath(topics, cursor), !c.Bool("replay") && !cursorSet, tail.onFrame)
	return tail.finish(streamErr)
}

type busTail struct {
	w       io.Writer
	jsonl   bool
	limit   int
	filt    busFilter
	lastSeq int64
	printed int
}

func (t *busTail) onFrame(ev store.StreamEvent) error {
	if ev.Topic == store.TopicSystemReset {
		return t.noticeReset(ev.Seq)
	}
	if !t.filt.match(ev) {
		return nil
	}
	if ev.Seq > t.lastSeq {
		t.lastSeq = ev.Seq
	}
	if err := renderBusFrame(t.w, ev, t.jsonl); err != nil {
		return err
	}
	t.printed++
	if t.limit > 0 && t.printed >= t.limit {
		return errBusLimit
	}
	return nil
}

func (t *busTail) noticeReset(seq int64) error {
	if t.jsonl {
		b, _ := json.Marshal(map[string]any{"notice": "system.reset", "seq": seq})
		fmt.Fprintf(t.w, "%s\n", b)
		return nil
	}
	fmt.Fprintf(t.w, "bus gap: retained window starts at seq %d (system.reset); stream is not contiguous\n", seq)
	return nil
}

func (t *busTail) finish(streamErr error) error {
	if streamErr == nil || errors.Is(streamErr, errBusLimit) || errors.Is(streamErr, context.Canceled) {
		return streamErr
	}
	if t.lastSeq > 0 {
		return fmt.Errorf("bus connection dropped after seq %d; resume with --cursor %d: %w", t.lastSeq, t.lastSeq, streamErr)
	}
	return fmt.Errorf("bus connection dropped: %w", streamErr)
}

type busFilter struct {
	entity, id, action string
	since              time.Time
}

func busFilterFrom(c *cli.Context) (busFilter, error) {
	f := busFilter{
		entity: c.String("entity"),
		id:     c.String("id"),
		action: c.String("action"),
	}
	if s := c.String("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return f, fmt.Errorf("--since must be RFC3339: %w", err)
		}
		f.since = t
	}
	return f, nil
}

func (f busFilter) match(ev store.StreamEvent) bool {
	if f.entity != "" && ev.Entity != f.entity {
		return false
	}
	if f.id != "" && ev.ID != f.id {
		return false
	}
	if f.action != "" && ev.Action != f.action {
		return false
	}
	if !f.since.IsZero() && ev.TS.Before(f.since) {
		return false
	}
	return true
}

func renderBusFrame(w io.Writer, ev store.StreamEvent, jsonl bool) error {
	if jsonl {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	}
	ts := "-"
	if !ev.TS.IsZero() {
		ts = ev.TS.UTC().Format(time.RFC3339)
	}
	ent := dash(ev.Entity)
	id := dash(ev.ID)
	action := dash(ev.Action)
	_, err := fmt.Fprintf(w, "%d %s %s %s/%s %s rev=%d\n", ev.Seq, ts, ev.Topic, ent, id, action, ev.Revision)
	return err
}

func normalizeBusTopics(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var parts []string
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			return "", fmt.Errorf("empty topic term in %q", raw)
		}
		if err := validateTopicTerm(t); err != nil {
			return "", err
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, ","), nil
}

func validateTopicTerm(term string) error {
	if topicKnown(term) {
		return nil
	}
	if near := nearestTopic(term); near != "" {
		return fmt.Errorf("unknown topic %q (did you mean %q?)", term, near)
	}
	return fmt.Errorf("unknown topic %q", term)
}

func topicKnown(term string) bool {
	if store.IsRegistered(term) {
		return true
	}
	prefix := term + "."
	for _, e := range store.AllEvents() {
		if strings.HasPrefix(e.Name, prefix) {
			return true
		}
	}
	return false
}

func nearestTopic(term string) string {
	if fam, rest, ok := strings.Cut(term, "."); ok {
		singular := strings.TrimSuffix(fam, "s")
		cand := singular + "." + rest
		if store.IsRegistered(cand) {
			return cand
		}
		if topicKnown(singular) && singular != fam {
			return singular
		}
	}
	if singular := strings.TrimSuffix(term, "s"); singular != term && topicKnown(singular) {
		return singular
	}
	return ""
}
