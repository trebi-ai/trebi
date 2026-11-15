package runs

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/trebi/internal/agentcli"
	"github.com/trebi-ai/trebi/internal/store"
)

// ChapterKind is the kind of one chapter: a group of tool steps, or one run fact.
type ChapterKind string

const (
	ChapterWork      ChapterKind = "work"
	ChapterMilestone ChapterKind = "milestone"
)

// ChapterKinds lists the closed set of chapter kinds.
var ChapterKinds = []ChapterKind{ChapterWork, ChapterMilestone} //nolint:gochecknoglobals // closed enum, never mutated

// MilestoneEvent is the fact that a milestone chapter records.
type MilestoneEvent string

const (
	MilestoneStarted   MilestoneEvent = "started"
	MilestoneResumed   MilestoneEvent = "resumed"
	MilestoneApproval  MilestoneEvent = "approval"
	MilestoneRequest   MilestoneEvent = "request"
	MilestoneSentToYou MilestoneEvent = "sent_to_you"
	MilestoneMailSent  MilestoneEvent = "mail_sent"
	MilestoneEnded     MilestoneEvent = "ended"
)

// MilestoneEvents lists the closed set of milestone events.
var MilestoneEvents = []MilestoneEvent{ //nolint:gochecknoglobals // closed enum, never mutated
	MilestoneStarted, MilestoneResumed, MilestoneApproval, MilestoneRequest,
	MilestoneSentToYou, MilestoneMailSent, MilestoneEnded,
}

// How a work chapter closes (Chapter.Break) and where its title comes from.
const (
	BreakMessage   = "message"
	BreakSteps     = "steps"
	BreakGap       = "gap"
	BreakMilestone = "milestone"
	BreakEnd       = "end"

	TitleFromMessage = "message"
	TitleFromSteps   = "steps"
)

// Notice kinds: an inbox item of the run, or an outbox row of the run that
// no inbox item owns.
const (
	NoticeInbox = "inbox"
	NoticeMail  = "mail"
)

const (
	chapterMaxSteps = 8
	chapterGap      = 60 * time.Second
	titleMaxRunes   = 160
	noteMaxRunes    = 240
	// approvalJoinSlop is how far apart a waiting event and its session
	// segment or approval row may start.
	approvalJoinSlop = 2 * time.Second
	// requestToolSlop is how long before the waiting event the request tool
	// may have ended and still count as the request tool.
	requestToolSlop = 30 * time.Second
	// shortChapterSteps is the most steps of a chapter that takes its step
	// labels as its title.
	shortChapterSteps = 2
)

// Chapters is the story of one run: tool steps grouped under the words of
// the agent, with the run facts between them.
type Chapters struct {
	RunID      string    `json:"run_id"`
	Live       bool      `json:"live"`
	WorkingMS  int64     `json:"working_ms"`
	WaitingMS  int64     `json:"waiting_ms"`
	IdleMS     int64     `json:"idle_ms"`
	StepCount  int       `json:"step_count"`
	EventCount int       `json:"event_count"`
	Chapters   []Chapter `json:"chapters"`
}

// Chapter is a flat union on Kind. The work fields and the milestone fields
// do not overlap. A milestone has no title: the client makes the words.
type Chapter struct {
	Kind       ChapterKind `json:"kind"`
	At         time.Time   `json:"at"`
	End        *time.Time  `json:"end,omitempty"`
	DurationMS *int64      `json:"duration_ms,omitempty"`

	// Work chapter.
	Title     string `json:"title,omitempty"`
	TitleFrom string `json:"title_from,omitempty"`
	Note      string `json:"note,omitempty"`
	Steps     []Step `json:"steps,omitempty"`
	StepCount int    `json:"step_count,omitempty"`
	Open      bool   `json:"open,omitempty"`
	Break     string `json:"break,omitempty"`

	// Milestone chapter.
	Event      MilestoneEvent   `json:"event,omitempty"`
	Reason     string           `json:"reason,omitempty"`
	Status     string           `json:"status,omitempty"`
	Question   string           `json:"question,omitempty"`
	Detail     string           `json:"detail,omitempty"`
	ApprovalID string           `json:"approval_id,omitempty"`
	RequestID  string           `json:"request_id,omitempty"`
	Decision   string           `json:"decision,omitempty"`
	Unanswered bool             `json:"unanswered,omitempty"`
	WaitedMS   *int64           `json:"waited_ms,omitempty"`
	LimitMS    *int64           `json:"limit_ms,omitempty"`
	Turn       int              `json:"turn,omitempty"`
	InboxID    string           `json:"inbox_id,omitempty"`
	Deliveries []NoticeDelivery `json:"deliveries,omitempty"`
}

// Step is one tool call of a work chapter, or several that merged.
type Step struct {
	Label      string    `json:"label"`
	Tool       string    `json:"tool"`
	Kind       string    `json:"kind,omitempty"`
	Count      int       `json:"count"`
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms"`
}

// Notice is something that Trebi sent to a person for the run.
type Notice struct {
	Kind       string           `json:"kind"`
	InboxID    string           `json:"inbox_id,omitempty"`
	At         time.Time        `json:"at"`
	Title      string           `json:"title,omitempty"`
	AlertKind  string           `json:"alert_kind,omitempty"`
	Status     string           `json:"status,omitempty"`
	Deliveries []NoticeDelivery `json:"deliveries,omitempty"`
}

// NoticeDelivery is one route step of a notice.
type NoticeDelivery struct {
	Via    string     `json:"via"`
	Status string     `json:"status"`
	SentAt *time.Time `json:"sent_at,omitempty"`
}

// ChapterInput is everything the builder reads. The builder does no I/O.
type ChapterInput struct {
	Run       *Run            `json:"run"`
	Turns     []agentcli.Turn `json:"turns"`
	Timeline  *Timeline       `json:"timeline"`
	Approvals []Approval      `json:"approvals"`
	Notices   []Notice        `json:"notices"`
	Now       time.Time       `json:"now"`
}

// chapterItem is a step or an assistant message, in run order.
type chapterItem struct {
	at       time.Time // start of a step, time of a message
	step     Step
	text     string
	isStep   bool
	consumed bool // the request tool of an approval: not a step
}

func (it chapterItem) end() time.Time {
	return it.at.Add(time.Duration(it.step.DurationMS) * time.Millisecond)
}

// workChapter is the chapter that is open while the builder walks the items.
type workChapter struct {
	steps   []Step
	lastEnd time.Time
}

type chapterBuilder struct {
	in    ChapterInput
	out   []Chapter
	cur   *workChapter
	total int
}

// BuildChapters turns the run data into chapters.
func BuildChapters(in ChapterInput) *Chapters {
	res := &Chapters{Chapters: []Chapter{}}
	if in.Run == nil {
		return res
	}
	if in.Timeline == nil {
		in.Timeline = &Timeline{}
	}
	b := &chapterBuilder{in: in}
	res.RunID = in.Run.ID
	res.Live = in.Run.Status == StatusRunning
	res.WorkingMS, res.WaitingMS, res.IdleMS = b.sessionTimes()
	res.EventCount = len(in.Timeline.Events)

	items := b.items()
	ms := b.milestones(items)
	b.walk(items, ms, res.Live)
	res.Chapters = append(res.Chapters, b.out...)
	res.StepCount = b.total
	if res.Chapters == nil {
		res.Chapters = []Chapter{}
	}
	return res
}

// sessionTimes sums the session segments by state. An open segment ends at
// the end of the run, or at Now.
func (b *chapterBuilder) sessionTimes() (working, waiting, idle int64) {
	limit := b.in.Now
	if e := b.in.Run.EndedAt; e != nil {
		limit = *e
	}
	for _, seg := range b.in.Timeline.Segments {
		if seg.Layer != "session" {
			continue
		}
		var ms int64
		switch {
		case seg.DurationMS != nil:
			ms = *seg.DurationMS
		case seg.End != nil:
			ms = seg.End.Sub(seg.Start).Milliseconds()
		default:
			ms = limit.Sub(seg.Start).Milliseconds()
		}
		ms = max(ms, 0)
		switch SessionState(seg.State) {
		case SessionRunning:
			working += ms
		case SessionWaiting:
			waiting += ms
		case SessionIdle:
			idle += ms
		}
	}
	return working, waiting, idle
}

// items lists the steps and the assistant messages in the order of the
// transcript. A tool result of a vendor file is not a step.
func (b *chapterBuilder) items() []chapterItem {
	turns := slices.Clone(b.in.Turns)
	if !slices.ContainsFunc(turns, func(t agentcli.Turn) bool { return t.At.IsZero() }) {
		slices.SortStableFunc(turns, func(a, c agentcli.Turn) int { return a.At.Compare(c.At) })
	}
	var items []chapterItem
	for _, t := range turns {
		if t.Role == "assistant" && !t.Synthetic && strings.TrimSpace(t.Text) != "" {
			items = append(items, chapterItem{at: t.At, text: t.Text})
		}
		for _, tc := range t.Tools {
			if tc.Name == "" || tc.Name == "tool_result" {
				continue
			}
			start := t.At
			if !tc.AtIsStart {
				start = t.At.Add(-time.Duration(tc.DurationMS) * time.Millisecond)
			}
			label := tc.Label
			if label == "" {
				label = agentcli.StepLabel(tc.Name, tc.Input)
			}
			items = append(items, chapterItem{at: start, isStep: true, step: Step{
				Label: label, Tool: tc.Name, Count: 1, At: start, DurationMS: tc.DurationMS,
				Kind: cmp.Or(tc.Kind, string(agentwire.ClassifyTool(tc.Name))),
			}})
		}
	}
	return items
}

// walk joins the items and the milestones in time order.
func (b *chapterBuilder) walk(items []chapterItem, ms []Chapter, live bool) {
	next := 0
	flush := func(upTo time.Time, all bool) {
		for next < len(ms) && (all || !ms[next].At.After(upTo)) {
			brk := BreakMilestone
			if ms[next].Event == MilestoneEnded {
				brk = BreakEnd
			}
			b.closeWork(brk)
			b.out = append(b.out, ms[next])
			next++
		}
	}
	for _, it := range items {
		if it.consumed {
			continue
		}
		flush(it.at, false)
		if !it.isStep {
			b.message(it)
			continue
		}
		b.addStep(it)
	}
	flush(time.Time{}, true)
	switch {
	case b.cur == nil:
	case live:
		b.closeOpen()
	default:
		b.closeWork(BreakEnd)
	}
}

func (b *chapterBuilder) addStep(it chapterItem) {
	b.total++
	if b.cur != nil {
		switch {
		case len(b.cur.steps) >= chapterMaxSteps:
			b.closeWork(BreakSteps)
		case it.at.Sub(b.cur.lastEnd) >= chapterGap:
			b.closeWork(BreakGap)
		}
	}
	if b.cur == nil {
		b.cur = &workChapter{}
	}
	b.cur.steps = append(b.cur.steps, it.step)
	if e := it.end(); e.After(b.cur.lastEnd) {
		b.cur.lastEnd = e
	}
}

// message closes the open chapter. A message with no open chapter is a
// chapter of its own, with no steps.
func (b *chapterBuilder) message(it chapterItem) {
	if b.cur == nil {
		b.cur = &workChapter{lastEnd: it.at}
		b.closeWorkAt(BreakMessage, it.text, it.at, it.at)
		return
	}
	b.closeWorkAt(BreakMessage, it.text, it.at, b.cur.firstAt())
}

func (w *workChapter) firstAt() time.Time {
	if len(w.steps) == 0 {
		return time.Time{}
	}
	return w.steps[0].At
}

func (b *chapterBuilder) closeWork(brk string) {
	if b.cur == nil {
		return
	}
	b.closeWorkAt(brk, "", b.cur.lastEnd, b.cur.firstAt())
}

// closeOpen emits the open chapter of a live run.
func (b *chapterBuilder) closeOpen() {
	ch := b.build("", b.cur.firstAt())
	ch.Open = true
	b.out = append(b.out, ch)
	b.cur = nil
}

func (b *chapterBuilder) closeWorkAt(brk, text string, end, start time.Time) {
	ch := b.build(text, start)
	ch.Break = brk
	ch.End = &end
	d := max(end.Sub(start).Milliseconds(), 0)
	ch.DurationMS = &d
	b.out = append(b.out, ch)
	b.cur = nil
}

// build makes the chapter of the open steps and the closing message.
func (b *chapterBuilder) build(text string, start time.Time) Chapter {
	steps := mergeSteps(b.cur.steps)
	ch := Chapter{Kind: ChapterWork, At: start, Steps: steps, StepCount: len(b.cur.steps)}
	if len(b.cur.steps) == 0 {
		ch.Steps = nil
	}
	if len(b.cur.steps) <= shortChapterSteps {
		if title := shortTitle(steps); title != "" {
			ch.Title, ch.TitleFrom = title, TitleFromSteps
			ch.Note = chapterNote(text)
			return ch
		}
	}
	if title := chapterTitle(text); title != "" {
		ch.Title, ch.TitleFrom = title, TitleFromMessage
	}
	return ch
}

// shortTitle joins the distinct labels of the merged steps, with "×N" when a
// label repeats.
func shortTitle(steps []Step) string {
	var labels []string
	for _, s := range steps {
		if s.Label == "" {
			continue
		}
		label := s.Label
		if s.Count > 1 {
			label += " ×" + strconv.Itoa(s.Count)
		}
		if !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, " · ")
}

// mergeSteps folds the web searches of a chapter into the first one, then
// folds the steps with the same label that follow each other.
func mergeSteps(steps []Step) []Step {
	var out []Step
	web := -1
	for _, s := range steps {
		if s.Label == agentcli.WebSearchLabel && web >= 0 {
			out[web].Count += s.Count
			out[web].DurationMS += s.DurationMS
			continue
		}
		if n := len(out); n > 0 && s.Label != "" && out[n-1].Label == s.Label {
			out[n-1].Count += s.Count
			out[n-1].DurationMS += s.DurationMS
			continue
		}
		if s.Label == agentcli.WebSearchLabel {
			web = len(out)
		}
		out = append(out, s)
	}
	return out
}

var (
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdPrefix = regexp.MustCompile(`^\s*(#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)`)
	mdMarks  = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "", "*", "") //nolint:gochecknoglobals // immutable replacer
	sentence = regexp.MustCompile(`[.!?]\s+\p{Lu}`)
)

// plainLines removes the Markdown of each line and drops the empty lines and
// the code fences.
func plainLines(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		line = mdLink.ReplaceAllString(line, "$1")
		line = mdPrefix.ReplaceAllString(line, "")
		line = strings.Join(strings.Fields(mdMarks.Replace(line)), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n])) + "…"
}

// chapterTitle is the first sentence of the text, without Markdown. The cut
// is at the first end mark that an upper-case letter follows, or at the end
// of the first line.
func chapterTitle(text string) string {
	lines := plainLines(text)
	if len(lines) == 0 {
		return ""
	}
	first := lines[0]
	if loc := sentence.FindStringIndex(first); loc != nil {
		first = first[:loc[0]+1]
	}
	return capRunes(first, titleMaxRunes)
}

// chapterNote is the whole message on one line, capped.
func chapterNote(text string) string {
	return capRunes(strings.Join(plainLines(text), " "), noteMaxRunes)
}

// waitingDetail is the detail of a waiting event.
type waitingDetail struct {
	Question     string `json:"question"`
	PermissionID string `json:"permission_id"`
	Tool         string `json:"tool"`
	Reason       string `json:"reason"`
}

// milestones lists the run facts in time order. It marks the request tool of
// each approval as consumed.
func (b *chapterBuilder) milestones(items []chapterItem) []Chapter {
	run := b.in.Run
	ms := []Chapter{{Kind: ChapterMilestone, At: run.StartedAt, Event: MilestoneStarted, Reason: string(run.Reason)}}
	usedApprovals := map[string]bool{}
	requests := map[string]int{}
	for _, ev := range b.in.Timeline.Events {
		switch ev.Kind {
		case "turn":
			var d struct {
				Turn   int    `json:"turn"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(ev.Detail, &d) == nil && d.Turn > 1 {
				ms = append(ms, Chapter{Kind: ChapterMilestone, At: ev.At, Event: MilestoneResumed, Turn: d.Turn, Reason: d.Reason})
			}
		case "waiting":
			var d waitingDetail
			_ = json.Unmarshal(ev.Detail, &d) // an unreadable detail gives an approval with no question
			ms = append(ms, b.approval(ev.At, d, items, usedApprovals))
		case EventRequest:
			var tr agentcli.TurnRequest
			if json.Unmarshal(ev.Detail, &tr) != nil || tr.ID == "" {
				continue
			}
			if i, ok := requests[tr.ID]; ok {
				ms[i].Status, ms[i].Question = tr.Status, tr.Title
				continue
			}
			requests[tr.ID] = len(ms)
			ms = append(ms, Chapter{Kind: ChapterMilestone, At: ev.At, Event: MilestoneRequest,
				RequestID: tr.ID, Status: tr.Status, Question: tr.Title})
		}
	}
	ms = append(ms, b.noticeChapters()...)
	if run.EndedAt != nil {
		ms = append(ms, b.endChapter())
	}
	slices.SortStableFunc(ms, func(a, c Chapter) int {
		if r := a.At.Compare(c.At); r != 0 {
			return r
		}
		return endLast(a) - endLast(c)
	})
	return ms
}

// noticeChapters makes one milestone for each inbox item or mail of the run.
func (b *chapterBuilder) noticeChapters() []Chapter {
	var out []Chapter
	for _, n := range b.in.Notices {
		ch := Chapter{Kind: ChapterMilestone, At: n.At, Detail: n.Title, Reason: n.AlertKind,
			Status: n.Status, InboxID: n.InboxID, Deliveries: n.Deliveries}
		ch.Event = MilestoneMailSent
		if n.Kind == NoticeInbox {
			ch.Event = MilestoneSentToYou
		}
		out = append(out, ch)
	}
	return out
}

// endChapter makes the closing milestone of an ended run.
func (b *chapterBuilder) endChapter() Chapter {
	run := b.in.Run
	end := Chapter{Kind: ChapterMilestone, At: *run.EndedAt, Event: MilestoneEnded,
		Status: string(run.Status), Reason: string(run.EndReason)}
	if run.EndReason == EndMaxDuration && run.Session.MaxDurationMS > 0 {
		limit := run.Session.MaxDurationMS
		end.LimitMS = &limit
	}
	return end
}

func endLast(c Chapter) int {
	if c.Event == MilestoneEnded {
		return 1
	}
	return 0
}

// approval makes the milestone of one waiting event.
func (b *chapterBuilder) approval(at time.Time, d waitingDetail, items []chapterItem, used map[string]bool) Chapter {
	ch := Chapter{Kind: ChapterMilestone, At: at, Event: MilestoneApproval, Question: d.Question, Detail: d.Reason}
	if i := requestTool(items, d.Tool, at); i >= 0 {
		items[i].consumed = true
		ch.At = items[i].at
	}
	row := b.approvalRow(at, d.PermissionID, used)
	seg := b.waitingSegment(at)
	if seg != nil {
		ms := b.segmentMS(seg)
		ch.WaitedMS = &ms
	}
	if row != nil {
		ch.ApprovalID = row.ID
	}
	b.decide(&ch, row, seg)
	return ch
}

// waitingSegment finds the waiting segment that starts with the event.
func (b *chapterBuilder) waitingSegment(at time.Time) *TimelineSegment {
	for i := range b.in.Timeline.Segments {
		s := &b.in.Timeline.Segments[i]
		if s.Layer == "session" && SessionState(s.State) == SessionWaiting && absDur(s.Start.Sub(at)) <= approvalJoinSlop {
			return s
		}
	}
	return nil
}

// segmentMS is the length of a segment. An open segment ends at Now.
func (b *chapterBuilder) segmentMS(seg *TimelineSegment) int64 {
	ms := b.in.Now.Sub(seg.Start).Milliseconds()
	switch {
	case seg.DurationMS != nil:
		ms = *seg.DurationMS
	case seg.End != nil:
		ms = seg.End.Sub(seg.Start).Milliseconds()
	}
	return max(ms, 0)
}

// decide sets the answer of an approval. After the end of the run, only an
// answer that came before the end counts.
func (b *chapterBuilder) decide(ch *Chapter, row *Approval, seg *TimelineSegment) {
	run := b.in.Run
	if run.EndedAt == nil {
		if row != nil && row.Status == "decided" {
			ch.Decision = row.Decision
		}
		return
	}
	answered := row != nil && row.Status == "decided" && !store.ParseTS(row.DecidedAt).After(*run.EndedAt)
	switch {
	case answered:
		ch.Decision = row.Decision
	case row != nil:
		ch.Unanswered = true
	case seg != nil && seg.End != nil && seg.End.After(run.EndedAt.Add(-time.Second)):
		ch.Unanswered = true
	}
}

// requestTool finds the call that the permission is for: the latest step of
// that tool that started before the waiting event and has not ended long
// before it.
func requestTool(items []chapterItem, tool string, at time.Time) int {
	if tool == "" {
		return -1
	}
	found := -1
	for i, it := range items {
		if !it.isStep || it.consumed || it.step.Tool != tool || it.at.After(at) {
			continue
		}
		if at.Sub(it.end()) > requestToolSlop {
			continue
		}
		if found < 0 || it.at.After(items[found].at) {
			found = i
		}
	}
	return found
}

// approvalRow finds the row of a waiting event: by permission key, or, for
// a pane or hook approval with no key, by the nearest creation time.
func (b *chapterBuilder) approvalRow(at time.Time, key string, used map[string]bool) *Approval {
	var best *Approval
	var bestGap time.Duration
	for i := range b.in.Approvals {
		a := &b.in.Approvals[i]
		if used[a.ID] {
			continue
		}
		if key != "" {
			if a.Key == key {
				used[a.ID] = true
				return a
			}
			continue
		}
		if a.Key != "" {
			continue
		}
		if gap := absDur(store.ParseTS(a.CreatedAt).Sub(at)); gap <= 5*approvalJoinSlop && (best == nil || gap < bestGap) {
			best, bestGap = a, gap
		}
	}
	if best != nil {
		used[best.ID] = true
	}
	return best
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
