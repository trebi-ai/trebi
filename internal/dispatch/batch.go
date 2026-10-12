package dispatch

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BatchMax is the event count of one batch row. The event that fills a row
// makes it due now; the next event opens a new row.
const BatchMax = 500

// Limits of the events block of a batch prompt.
const (
	batchLines   = 50
	batchTextCut = 300
)

const batchLanePrefix = "batch:"

// BatchLaneKey is the lane key of the batch rows of one event trigger. Two
// batch triggers of one job do not share a lane, and a batch never mixes
// with a conversation lane.
func BatchLaneKey(onIndex int) string { return batchLanePrefix + strconv.Itoa(onIndex) }

// IsBatchLane reports whether a lane key is a batch lane key.
func IsBatchLane(key string) bool { return strings.HasPrefix(key, batchLanePrefix) }

// BatchClock sets the tick times of batch windows: start + k × window, where
// start is the daemon start time.
type BatchClock struct{ start time.Time }

// NewBatchClock starts the ticks at start, cut to the second, because
// next_attempt_at has second precision.
func NewBatchClock(start time.Time) BatchClock {
	return BatchClock{start: start.UTC().Truncate(time.Second)}
}

// NextTick is the first tick after t. A t on a tick gives the next tick. A t
// before the start gives the start.
func (c BatchClock) NextTick(t time.Time, window time.Duration) time.Time {
	start := c.start
	if start.IsZero() {
		start = time.Unix(0, 0).UTC()
	}
	if window <= 0 || t.Before(start) {
		return start
	}
	return start.Add((t.Sub(start)/window + 1) * window)
}

// BatchEvent is one event of a batch row: an entry of events.json.
type BatchEvent struct {
	EventID    string            `json:"event_id"`
	Connector  string            `json:"connector,omitempty"`
	ReceivedAt string            `json:"received_at,omitempty"`
	Sender     string            `json:"sender,omitempty"`
	Room       string            `json:"room,omitempty"`
	RoomName   string            `json:"room_name,omitempty"`
	Thread     string            `json:"thread,omitempty"`
	Text       string            `json:"text,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	Vars       map[string]string `json:"vars,omitempty"`
	Raw        json.RawMessage   `json:"raw,omitempty"`
	AddedAt    time.Time         `json:"-"`
}

// setRaw keeps a JSON event body as is and wraps any other body as a string.
func (e *BatchEvent) setRaw(raw string) {
	switch {
	case raw == "":
	case json.Valid([]byte(raw)):
		e.Raw = json.RawMessage(raw)
	default:
		e.Raw, _ = json.Marshal(raw) // a string always marshals
	}
}

// line is the events block line of e: - <HH:MM> <sender> in <room>: <text>.
// An event with no text (not a channel message) shows its summary.
func (e BatchEvent) line() string {
	var b strings.Builder
	b.WriteString("-")
	if t, err := time.Parse(time.RFC3339Nano, e.ReceivedAt); err == nil {
		b.WriteString(" " + t.Local().Format("15:04"))
	}
	if e.Sender != "" {
		b.WriteString(" " + e.Sender)
	}
	if room := firstNonEmpty(e.RoomName, e.Room); room != "" {
		b.WriteString(" in " + room)
	}
	text := strings.Join(strings.Fields(firstNonEmpty(e.Text, e.Summary)), " ")
	if r := []rune(text); len(r) > batchTextCut {
		text = string(r[:batchTextCut]) + "…"
	}
	if e.Sender != "" || e.RoomName != "" || e.Room != "" {
		b.WriteString(":")
	}
	b.WriteString(" " + text)
	return b.String()
}

// Batch is the events of one batch run and the path of its events.json.
type Batch struct {
	Events []BatchEvent
	File   string
}

// Lines is the events block: one line per event, at most 50, then a count
// of the rest.
func (b Batch) Lines() string {
	lines := make([]string, 0, min(len(b.Events), batchLines)+1)
	for i, ev := range b.Events {
		if i == batchLines {
			lines = append(lines, fmt.Sprintf("… and %d more in %s", len(b.Events)-batchLines, b.File))
			break
		}
		lines = append(lines, ev.line())
	}
	return strings.Join(lines, "\n")
}

// Vars are the prompt vars of a batch run. connection is set when every
// event came from one connection.
func (b Batch) Vars() map[string]string {
	vars := map[string]string{
		"event_count": strconv.Itoa(len(b.Events)),
		"events":      b.Lines(),
		"events_file": b.File,
	}
	conn := ""
	for i, ev := range b.Events {
		if i > 0 && ev.Vars["connection"] != conn {
			conn = ""
			break
		}
		conn = ev.Vars["connection"]
	}
	if conn != "" {
		vars["connection"] = conn
	}
	return vars
}

// Block is the default events block that ends a prompt with no {{events}}.
func (b Batch) Block() string { return BatchBlock(b.Vars()) }

// BatchBlock is the default events block made from the vars of a batch run.
// A rerun makes it again from the stored vars.
func BatchBlock(vars map[string]string) string {
	return fmt.Sprintf("New events (%s):\n%s\nAll events: %s", vars["event_count"], vars["events"], vars["events_file"])
}

// JSON is the content of events.json.
func (b Batch) JSON() ([]byte, error) {
	events := b.Events
	if events == nil {
		events = []BatchEvent{}
	}
	return json.MarshalIndent(events, "", "  ")
}

// EventIDs lists the event ids in join order.
func (b Batch) EventIDs() []string {
	ids := make([]string, len(b.Events))
	for i, ev := range b.Events {
		ids[i] = ev.EventID
	}
	return ids
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
