package runs

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/agentcli"
)

var updateChapters = flag.Bool("update", false, "rewrite the chapter golden files")

func TestChapterTitle(t *testing.T) {
	long := strings.Repeat("word ", 60)
	tests := []struct{ name, in, want string }{
		{"first sentence", "I found the file. Then I read it.", "I found the file."},
		{"no cut before lower case", "Version 1.2 is out. Next.", "Version 1.2 is out."},
		{"markdown", "## **Done** with [the docs](http://x.y). Next.", "Done with the docs."},
		{"list item", "- First item\n- Second item", "First item"},
		{"code fence", "```\ncode\n```\nAfter the code.", "code"},
		{"empty", " \n ", ""},
		{"capped", long, strings.TrimSpace(long[:160]) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chapterTitle(tc.in); got != tc.want {
				t.Fatalf("chapterTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestChapterNote(t *testing.T) {
	got := chapterNote("# Title\n\nOne.\n\n- Two")
	if got != "Title One. Two" {
		t.Fatalf("note = %q", got)
	}
}

func TestMergeSteps(t *testing.T) {
	steps := []Step{
		{Label: "Read a.go", Count: 1, DurationMS: 1},
		{Label: "Read a.go", Count: 1, DurationMS: 2},
		{Label: agentcli.WebSearchLabel, Count: 1},
		{Label: "Ran a command", Count: 1},
		{Label: agentcli.WebSearchLabel, Count: 1},
		{Label: "", Count: 1},
		{Label: "", Count: 1},
	}
	got := mergeSteps(steps)
	if len(got) != 5 {
		t.Fatalf("merged = %+v", got)
	}
	if got[0].Count != 2 || got[0].DurationMS != 3 {
		t.Fatalf("same label did not merge: %+v", got[0])
	}
	if got[1].Count != 2 {
		t.Fatalf("web searches did not merge: %+v", got[1])
	}
}

func TestBuildChaptersGroups(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	in := chapterFixture(base)
	in.Turns = []agentcli.Turn{
		step(at(1), "Read", "Read a.go", 100),
		step(at(3), "Read", "Read b.go", 100),
		step(at(5), "Bash", "Ran a command", 100),
		{Role: "assistant", At: at(6), Text: "The code is clear. Now I edit it."},
		step(at(7), "Edit", "Edited a.go", 100),
		{Role: "assistant", At: at(8), Text: "Edited the file.\n\nIt compiles now."},
		// gap of more than 60 s closes the chapter
		step(at(100), "Read", "Read c.go", 100),
		step(at(200), "Read", "Read d.go", 100),
	}
	got := BuildChapters(in)
	if got.StepCount != 6 {
		t.Fatalf("step_count = %d", got.StepCount)
	}
	var kinds []string
	for _, c := range got.Chapters {
		kinds = append(kinds, string(c.Kind)+":"+c.Break)
	}
	want := "milestone:|work:message|work:message|work:gap|work:end|milestone:"
	if strings.Join(kinds, "|") != want {
		t.Fatalf("chapters = %v", kinds)
	}
	first := got.Chapters[1]
	if first.TitleFrom != TitleFromMessage || first.Title != "The code is clear." || first.StepCount != 3 {
		t.Fatalf("first chapter: %+v", first)
	}
	short := got.Chapters[2]
	if short.TitleFrom != TitleFromSteps || short.Title != "Edited a.go" || short.Note != "Edited the file. It compiles now." {
		t.Fatalf("short chapter: %+v", short)
	}
}

func TestBuildChaptersBreakOnStepCount(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	in := chapterFixture(base)
	for i := range 10 {
		in.Turns = append(in.Turns, step(base.Add(time.Duration(i+1)*time.Second), "Read", "Read f"+string(rune('a'+i)), 10))
	}
	got := BuildChapters(in)
	var counts []int
	for _, c := range got.Chapters {
		if c.Kind == ChapterWork {
			counts = append(counts, c.StepCount)
		}
	}
	if len(counts) != 2 || counts[0] != 8 || counts[1] != 2 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestBuildChaptersQuietStep(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	in := chapterFixture(base)
	in.Turns = []agentcli.Turn{step(base.Add(time.Second), "ToolSearch", "", 5), step(base.Add(2*time.Second), "Read", "Read a.go", 5)}
	got := BuildChapters(in)
	ch := got.Chapters[1]
	if ch.StepCount != 2 || len(ch.Steps) != 2 || got.StepCount != 2 {
		t.Fatalf("chapter = %+v", ch)
	}
	if ch.Title != "Read a.go" {
		t.Fatalf("a quiet step must not be in the title: %q", ch.Title)
	}
}

func TestBuildChaptersLive(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	in := chapterFixture(base)
	in.Run.Status, in.Run.EndedAt = StatusRunning, nil
	in.Turns = []agentcli.Turn{step(base.Add(time.Second), "Read", "Read a.go", 5)}
	got := BuildChapters(in)
	last := got.Chapters[len(got.Chapters)-1]
	if !got.Live || !last.Open || last.End != nil {
		t.Fatalf("live = %v, last = %+v", got.Live, last)
	}
}

func TestBuildChaptersApproval(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	waitAt := base.Add(10 * time.Second)
	detail := json.RawMessage(`{"question":"Send mail","permission_id":"k1","tool":"Bash"}`)
	build := func(decidedAt time.Time, ended bool) Chapter {
		in := chapterFixture(base)
		ms := int64(120000)
		segEnd := waitAt.Add(2 * time.Minute)
		in.Timeline.Events = []TimelinePoint{{Kind: "waiting", At: waitAt, Detail: detail}}
		in.Timeline.Segments = []TimelineSegment{{Layer: "session", State: "waiting", Start: waitAt, End: &segEnd, DurationMS: &ms}}
		in.Turns = []agentcli.Turn{step(waitAt, "Bash", "Ran a command", 9000)}
		in.Approvals = []Approval{{ID: "a1", Status: "decided", Decision: "allow", Key: "k1", CreatedAt: waitAt.Format(time.RFC3339Nano), DecidedAt: decidedAt.Format(time.RFC3339Nano)}}
		if !ended {
			in.Run.Status, in.Run.EndedAt = StatusRunning, nil
		}
		got := BuildChapters(in)
		for _, c := range got.Chapters {
			if c.Event == MilestoneApproval {
				if got.StepCount != 0 {
					t.Fatalf("the request tool must not be a step of a chapter: %+v", got.Chapters)
				}
				return c
			}
		}
		t.Fatalf("no approval: %+v", got.Chapters)
		return Chapter{}
	}
	answered := build(waitAt.Add(time.Minute), true)
	if answered.Decision != "allow" || answered.Unanswered || answered.WaitedMS == nil || *answered.WaitedMS != 120000 || answered.ApprovalID != "a1" {
		t.Fatalf("answered: %+v", answered)
	}
	late := build(base.Add(time.Hour), true)
	if late.Decision != "" || !late.Unanswered {
		t.Fatalf("answer after the end: %+v", late)
	}
	live := build(waitAt.Add(time.Minute), false)
	if live.Decision != "allow" || live.Unanswered {
		t.Fatalf("live: %+v", live)
	}
}

func TestBuildChaptersMilestones(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	in := chapterFixture(base)
	in.Run.Status, in.Run.EndReason = StatusTimedOut, EndMaxDuration
	in.Run.Session.MaxDurationMS = 600000
	in.Timeline.Events = []TimelinePoint{
		{Kind: "turn", At: base.Add(time.Minute), Detail: json.RawMessage(`{"turn":2,"reason":"message"}`)},
		{Kind: EventRequest, At: base.Add(2 * time.Minute), Detail: json.RawMessage(`{"id":"q1","status":"open","title":"Pick one"}`)},
		{Kind: EventRequest, At: base.Add(3 * time.Minute), Detail: json.RawMessage(`{"id":"q1","status":"answered","title":"Pick one"}`)},
	}
	in.Notices = []Notice{{Kind: NoticeInbox, InboxID: "i1", At: base.Add(4 * time.Minute), Title: "Waiting"}, {Kind: NoticeMail, At: base.Add(5 * time.Minute), Title: "Report", Status: "sent"}}
	got := BuildChapters(in)
	var events []string
	for _, c := range got.Chapters {
		events = append(events, string(c.Event))
	}
	if strings.Join(events, ",") != "started,resumed,request,sent_to_you,mail_sent,ended" {
		t.Fatalf("events = %v", events)
	}
	if r := got.Chapters[2]; r.Status != "answered" || r.RequestID != "q1" {
		t.Fatalf("request: %+v", r)
	}
	if end := got.Chapters[5]; end.LimitMS == nil || *end.LimitMS != 600000 || end.Reason != string(EndMaxDuration) {
		t.Fatalf("ended: %+v", end)
	}
}

func TestBuildChaptersGolden(t *testing.T) {
	const input, golden = "testdata/chapters/claude-timed-out.json", "testdata/chapters/claude-timed-out.golden.json"
	raw, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	var in ChapterInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(BuildChapters(in), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateChapters {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("chapters differ from %s; run with -update and review the diff\n%s", golden, got)
	}
}

// chapterFixture builds a small run that ended at base+10m.
func chapterFixture(base time.Time) ChapterInput {
	end := base.Add(10 * time.Minute)
	return ChapterInput{
		Run:      &Run{ID: "r1", Status: StatusComplete, Reason: "schedule", StartedAt: base, EndedAt: &end},
		Timeline: &Timeline{RunID: "r1"},
		Now:      end,
	}
}

func step(at time.Time, name, label string, ms int64) agentcli.Turn {
	return agentcli.Turn{Role: "tools", At: at.Add(time.Duration(ms) * time.Millisecond), Tools: []agentcli.ToolCall{{Name: name, Label: label, DurationMS: ms}}}
}
