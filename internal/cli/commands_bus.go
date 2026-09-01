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

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/urfave/cli/v3"
)

var errBusLimit = errors.New("bus frame limit reached")

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
			&cli.IntFlag{Name: "limit", Usage: "max rendered frames, then exit (does not follow live)"},
			&cli.BoolFlag{Name: "json", Usage: "JSONL (one raw frame per line)"},
		},
		Action: runBus,
	}
}

func runBus(ctx context.Context, c *cli.Command) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := runBusWith(ctx, c)
	if err == nil || errors.Is(err, errBusLimit) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func runBusWith(ctx context.Context, c *cli.Command) error {
	topics, err := normalizeBusTopics(c.String("topics"))
	if err != nil {
		return err
	}
	filt, err := newBusFilter(c)
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
	path := request.BuildStreamPath(topics, cursor)
	if tail.limit > 0 {
		path = request.BuildStreamDumpPath(topics, cursor)
	}
	streamErr := client.streamEvents(ctx, path, !c.Bool("replay") && !cursorSet, tail.onFrame)
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
	// --limit is a max. A clean close after fewer frames than N is success.
	if t.limit > 0 && errors.Is(streamErr, io.EOF) {
		return nil
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

func newBusFilter(c *cli.Command) (busFilter, error) {
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
		if !topicKnown(t) {
			if near := nearestTopic(t); near != "" {
				return "", fmt.Errorf("unknown topic %q (did you mean %q?)", t, near)
			}
			return "", fmt.Errorf("unknown topic %q", t)
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, ","), nil
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
