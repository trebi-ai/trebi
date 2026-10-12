package artifacts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/tools"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestBlobs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := NewBlobs(dir, 10)
	sha1, n, err := b.Put(strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("put %v %d", err, n)
	}
	sha2, _, err := b.Put(strings.NewReader("hello"))
	if err != nil || sha2 != sha1 || blobCount(t, b) != 1 {
		t.Fatalf("dedupe %s %s %v", sha1, sha2, err)
	}
	if _, _, err := b.Put(strings.NewReader("hello world!")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
	b.rename = func(string, string) error { return errors.New("crash") }
	if _, _, err := b.Put(strings.NewReader("other")); err == nil {
		t.Fatal("a failed rename must fail")
	}
	b.rename = os.Rename
	if err := os.WriteFile(filepath.Join(dir, tempPrefix+"left"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if blobCount(t, b) != 1 {
		t.Fatal("walk must skip temp files")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) && e.Name() != tempPrefix+"left" {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
	if _, _, err := b.Open("../../etc/passwd"); !errors.Is(err, ErrBadSHA) {
		t.Fatalf("bad sha: %v", err)
	}
	rc, size, err := b.Open(sha1)
	if err != nil || size != 5 {
		t.Fatalf("open %v %d", err, size)
	}
	_ = rc.Close()
}

func TestDetectKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, head string
		want       Kind
	}{
		{"report.md", "# Hi", KindMarkdown},
		{"draft.md", "---\nkind: email\nto: a@b.c\n---\nhi", KindEmail},
		{"draft.md", "---\ntitle: x\n---\nhi", KindMarkdown},
		{"r.HTML", "<p>", KindHTML},
		{"d.csv", "a,b", KindTable},
		{"d.tsv", "a\tb", KindTable},
		{"d.json", "{}", KindJSON},
		{"q.sql", "select 1", KindCode},
		{"i.svg", "<svg>", KindSVG},
		{"i.png", "\x89PNG\r\n\x1a\n", KindImage},
		{"f.pdf", "%PDF", KindPDF},
		{"deck.pptx", "PK", KindFile},
		{"blob", "\x00\x01", KindFile},
	}
	for _, tc := range cases {
		if got, _ := DetectKind(tc.name, []byte(tc.head)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParseDraft(t *testing.T) {
	t.Parallel()
	ok := "---\nkind: email\nto: [Ann <ann@x.com>]\ncc: bob@x.com, cy@x.com\nsubject: Hello\nin_reply_to: <a1@x.com>\n---\nBody **here**\n"
	d, errs := ParseDraft([]byte(ok))
	if len(errs) != 0 || d.From != FromTrebi || len(d.Cc) != 2 || d.Body != "Body **here**\n" {
		t.Fatalf("draft %+v %v", d, errs)
	}
	bad := []struct{ header, field string }{
		{"kind: email\nsubject: x", "to"},
		{"kind: email\nto: not-an-address\nsubject: x", "to"},
		{"kind: email\nto: a@x.com", "subject"},
		{"kind: email\nto: a@x.com\nsubject: x\nfrom: Bad Name", "from"},
		{"kind: email\nto: a@x.com\nsubject: x\nin_reply_to: abc", "in_reply_to"},
		{"kind: email\nto: a@x.com\nsubject: x\ncc: nope", "cc"},
	}
	for _, tc := range bad {
		_, errs := ParseDraft([]byte("---\n" + tc.header + "\n---\nbody"))
		if len(errs) != 1 || errs[0].Field != tc.field {
			t.Errorf("%q: errors %v, want one on %s", tc.header, errs, tc.field)
		}
	}
	if errs := KindJSON.Validate([]byte("{")); len(errs) != 1 {
		t.Fatal("bad JSON must fail")
	}
	if errs := KindTable.Validate([]byte("a,\"b\n")); len(errs) != 0 {
		t.Fatalf("lazy quotes: %v", errs)
	}
}

func TestPutStale(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, nil)
	runA := Origin{Kind: OriginRun, RunID: "run-a", OwnerKind: OwnerJob, OwnerID: "job1"}
	user := Origin{Kind: OriginUser, OwnerKind: OwnerJob, OwnerID: "job1"}
	for i, want := range []int{1, 2} {
		v, err := put(t, s, runA, nil, "v"+string(rune('1'+i)))
		if err != nil || v.N != want {
			t.Fatalf("save %d: %v %+v", want, err, v)
		}
	}
	if _, err := put(t, s, user, nil, "user"); err == nil {
		t.Fatal("a user save with no base over a run version must be stale")
	}
	two := 2
	if v, err := put(t, s, user, &two, "user"); err != nil || v.N != 3 {
		t.Fatalf("user save: %v %+v", err, v)
	}
	var se *StaleError
	if _, err := put(t, s, runA, nil, "again"); !errors.As(err, &se) || se.Latest != 3 {
		t.Fatalf("want StaleError{3}, got %v", err)
	}
	three := 3
	if v, err := put(t, s, runA, &three, "again"); err != nil || v.N != 4 {
		t.Fatalf("base 3: %v %+v", err, v)
	}
	a, n, err := s.Resolve(context.Background(), "ws1", Origin{}, "weekly@2")
	if err != nil || n != 2 || a.LatestVersion != 4 || a.VersionCount != 4 || a.Latest == nil || a.Latest.N != 4 {
		t.Fatalf("resolve %+v %d %v", a, n, err)
	}
	var topics []string
	rows, _ := s.st.DB().Query(`SELECT topic FROM event_log ORDER BY seq`)
	for rows.Next() {
		var tp string
		_ = rows.Scan(&tp)
		topics = append(topics, tp)
	}
	_ = rows.Close()
	if !slices.Equal(topics, []string{"artifact.created", "artifact.updated", "artifact.updated", "artifact.updated"}) {
		t.Fatalf("topics %v", topics)
	}
}

func TestPutConcurrent(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, nil)
	o := Origin{Kind: OriginRun, RunID: "r", OwnerKind: OwnerAssistant}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := put(t, s, o, nil, strings.Repeat("x", i+1))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	vs, err := s.Versions(context.Background(), mustID(t, s))
	if err != nil || len(vs) != 8 || vs[0].N != 8 || vs[7].N != 1 {
		t.Fatalf("versions %d %v", len(vs), err)
	}
}

func TestPutFromFile(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.html"), []byte("<p>hi</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := PutOpts{Workspace: "ws1", Name: "report", Origin: Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}}
	v, err := s.PutFromFile(context.Background(), o, dir, filepath.Join(dir, "report.html"))
	if err != nil || v.FileName != "report.html" || v.MIME != "text/html; charset=utf-8" {
		t.Fatalf("put %+v %v", v, err)
	}
	for _, rel := range []string{"../x.html", "missing.html", "/etc/hosts"} {
		var ie *InvalidError
		if _, err := s.PutFromFile(context.Background(), o, dir, rel); !errors.As(err, &ie) || ie.Fields[0].Field != "from_path" {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	bad := o
	bad.Name = "Bad Name"
	if _, err := s.PutFromFile(context.Background(), bad, dir, "report.html"); err == nil {
		t.Fatal("a bad name must fail")
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, nil)
	o := Origin{Kind: OriginRun, RunID: "r", OwnerKind: OwnerAssistant}
	for i := 1; i <= 6; i++ {
		if _, err := put(t, s, o, nil, strings.Repeat("v", i)); err != nil {
			t.Fatal(err)
		}
	}
	id := mustID(t, s)
	ctx := context.Background()
	if err := s.Pin(ctx, id, 2, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.DB().Exec(`INSERT INTO email_messages (id, direction, disposition, created_at, artifact_id, artifact_version)
  VALUES ('m1','out','sent','2026-10-10T00:00:00Z',?,4)`, id); err != nil {
		t.Fatal(err)
	}
	orphanOld, _, _ := s.blobs.Put(strings.NewReader("orphan-old"))
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(s.blobs.path(orphanOld), old, old); err != nil {
		t.Fatal(err)
	}
	orphanNew, _, _ := s.blobs.Put(strings.NewReader("orphan-new"))
	stats, err := s.Prune(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	vs, _ := s.Versions(ctx, id)
	var ns []int
	for _, v := range vs {
		ns = append(ns, v.N)
	}
	if !slices.Equal(ns, []int{6, 5, 4, 2}) || stats.Versions != 2 {
		t.Fatalf("kept %v, stats %+v", ns, stats)
	}
	if s.blobs.Has(orphanOld) || !s.blobs.Has(orphanNew) {
		t.Fatal("orphan blobs: the old must go, the new must stay")
	}
	d, _ := s.Deliveries(ctx, id)
	if len(d) != 1 || d[0].Version != 4 {
		t.Fatalf("deliveries %+v", d)
	}
}

type fakeRuns struct{ run runs.Run }

func (f fakeRuns) Get(context.Context, string) (*runs.Run, error) { r := f.run; return &r, nil }

type fakeJobs struct{ row workspaces.JobRow }

func (f fakeJobs) JobByID(context.Context, string) (*workspaces.JobRow, error) {
	r := f.row
	return &r, nil
}

type fakeMark struct{ got []runs.MarkOpts }

func (f *fakeMark) Mark(_ context.Context, o runs.MarkOpts) (*runs.Run, error) {
	f.got = append(f.got, o)
	return &runs.Run{}, nil
}

type fakeAlerts struct {
	got      []alerts.RaiseOpts
	resolved []string
}

func (f *fakeAlerts) ResolveSubject(_ context.Context, subject, _ string) error {
	f.resolved = append(f.resolved, subject)
	return nil
}

func (f *fakeAlerts) Raise(_ context.Context, o alerts.RaiseOpts) (string, error) {
	f.got = append(f.got, o)
	return "a1", nil
}

func TestRunGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobPath := filepath.Join(dir, "JOB.md")
	md := "---\nharness: claude\nartifacts:\n  - path: report.html\n  - {path: data.csv, name: sales}\n---\nbody\n"
	if err := os.WriteFile(jobPath, []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.html"), []byte("<p>hi</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := runs.Run{ID: "run1", Workspace: "ws1", JobID: "job1", JobName: "weekly", Kind: "job", WorkingDir: dir}
	s := newTestService(t, fakeRuns{run}, fakeJobs{workspaces.JobRow{ID: "job1", Path: jobPath}})
	m, al := &fakeMark{}, &fakeAlerts{}
	g := NewRunGate(s, m, al)
	g.RunEnded(context.Background(), "run1", runs.StatusFailed)
	if refs, _ := s.ByRun(context.Background(), "run1"); len(refs) != 0 {
		t.Fatal("a failed run saves nothing")
	}
	g.RunEnded(context.Background(), "run1", runs.StatusComplete)
	refs, _ := s.ByRun(context.Background(), "run1")
	if len(refs) != 1 || refs[0].Name != "report" || refs[0].N != 1 {
		t.Fatalf("refs %+v", refs)
	}
	if len(m.got) != 1 || m.got[0].Status != runs.StatusComplete || m.got[0].Note != "Artifact data.csv was not written." {
		t.Fatalf("mark %+v", m.got)
	}
	if len(al.got) != 1 || al.got[0].Kind != alerts.KindArtifactMissing || al.got[0].RunID != "run1" {
		t.Fatalf("alerts %+v", al.got)
	}
	// The next run of the same job adds a version with no stale error.
	s.runs = fakeRuns{runs.Run{ID: "run2", Workspace: "ws1", JobID: "job1", Kind: "job", WorkingDir: dir}}
	g.RunEnded(context.Background(), "run2", runs.StatusComplete)
	if refs, _ := s.ByRun(context.Background(), "run2"); len(refs) != 1 || refs[0].N != 2 {
		t.Fatalf("second run %+v", refs)
	}
}

type mapRuns map[string]runs.Run

func (m mapRuns) Get(_ context.Context, id string) (*runs.Run, error) {
	r, ok := m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &r, nil
}

type fakeCont struct {
	live   map[string]bool
	made   []string // workspace|operator|title
	system []string // conv|text
	sent   []string // workspace|conv|text
}

func (f *fakeCont) ConversationExists(_ context.Context, id string) bool { return f.live[id] }

func (f *fakeCont) NewConversation(_ context.Context, ws, operator, title string) (string, error) {
	f.made = append(f.made, ws+"|"+operator+"|"+title)
	id := fmt.Sprintf("conv-new-%d", len(f.made))
	f.live[id] = true
	return id, nil
}

func (f *fakeCont) AppendSystem(_ context.Context, id, text string) error {
	f.system = append(f.system, id+"|"+text)
	return nil
}

func (f *fakeCont) SendToConversation(_ context.Context, ws, id, text string) error {
	f.sent = append(f.sent, ws+"|"+id+"|"+text)
	return nil
}

func TestRunOrigin(t *testing.T) {
	t.Parallel()
	rs := mapRuns{
		"ra": {ID: "ra", ConversationID: "c1"},
		"rb": {ID: "rb", ConversationID: "c2"},
		"rc": {ID: "rc"},
		"rd": {ID: "rd"},
	}
	cases := []struct {
		name                       string
		actor                      tools.Actor
		kind, conv, owner, ownerID string
	}{
		{"assistant in a conversation", tools.Actor{Kind: OwnerAssistant, RunID: "ra"}, OriginConversation, "c1", OwnerAssistant, ""},
		{"agent in a conversation", tools.Actor{Kind: OwnerAgent, RunID: "rb", JobID: "agt_e"}, OriginConversation, "c2", OwnerAgent, "agt_e"},
		{"agent with no conversation", tools.Actor{Kind: OwnerAgent, RunID: "rc", JobID: "agt_e"}, OriginRun, "", OwnerAgent, "agt_e"},
		{"plain job", tools.Actor{Kind: "job", RunID: "rd", JobID: "job1"}, OriginRun, "", OwnerJob, "job1"},
	}
	s := newTestService(t, rs, nil)
	for _, c := range cases {
		o, err := s.OriginFor(context.Background(), &c.actor, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if o.Kind != c.kind || o.RunID != c.actor.RunID || o.ConversationID != c.conv || o.OwnerKind != c.owner || o.OwnerID != c.ownerID {
			t.Errorf("%s: %+v", c.name, o)
		}
	}
}

func TestConversationRunsShareStale(t *testing.T) {
	t.Parallel()
	rs := mapRuns{
		"ra": {ID: "ra", ConversationID: "c1"},
		"rb": {ID: "rb", ConversationID: "c1"},
		"rx": {ID: "rx"},
	}
	s := newTestService(t, rs, nil)
	ctx := context.Background()
	origin := func(run string) Origin {
		o, err := s.OriginFor(ctx, &tools.Actor{Kind: OwnerAgent, RunID: run, JobID: "agt_e"}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	if _, err := put(t, s, origin("ra"), nil, "one"); err != nil {
		t.Fatal(err)
	}
	v, err := put(t, s, origin("rb"), nil, "two")
	if err != nil || v.N != 2 || v.RunID != "rb" || v.ConversationID != "c1" {
		t.Fatalf("same conversation %+v %v", v, err)
	}
	var se *StaleError
	if _, err := put(t, s, origin("rx"), nil, "three"); !errors.As(err, &se) {
		t.Fatalf("run outside the conversation: %v", err)
	}
}

func TestChat(t *testing.T) {
	t.Parallel()
	user := Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}
	cases := []struct {
		name     string
		live     []string
		runs     mapRuns
		stored   string // artifacts.conversation_id
		origins  []Origin
		want     string
		created  bool
		operator string
	}{
		{name: "stored chat", live: []string{"c1"}, stored: "c1",
			origins: []Origin{{Kind: OriginConversation, ConversationID: "c0", OwnerKind: OwnerAssistant}}, want: "c1"},
		{name: "conversation version under a user version", live: []string{"c2"},
			origins: []Origin{{Kind: OriginConversation, ConversationID: "c2", OwnerKind: OwnerAssistant}, user}, want: "c2"},
		{name: "run of a conversation", live: []string{"c3"}, runs: mapRuns{"r3": {ID: "r3", ConversationID: "c3"}},
			origins: []Origin{{Kind: OriginRun, RunID: "r3", OwnerKind: OwnerAgent, OwnerID: "agt_e"}}, want: "c3"},
		{name: "agent owner", runs: mapRuns{"r4": {ID: "r4"}},
			origins: []Origin{{Kind: OriginRun, RunID: "r4", OwnerKind: OwnerAgent, OwnerID: "agt_e"}},
			want:    "conv-new-1", created: true, operator: "agt_e"},
		{name: "plain job", origins: []Origin{{Kind: OriginRun, RunID: "r5", OwnerKind: OwnerJob, OwnerID: "job1"}},
			want: "conv-new-1", created: true},
		{name: "deleted chat", stored: "gone", origins: []Origin{user}, want: "conv-new-1", created: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newTestService(t, c.runs, nil)
			if c.runs == nil {
				s.runs = mapRuns{}
			}
			f := newFakeCont(c.live...)
			s.WithContinuer(f)
			ctx := context.Background()
			for i, o := range c.origins {
				var base *int
				if i > 0 {
					base = &i
				}
				if _, err := put(t, s, o, base, fmt.Sprint("v", i)); err != nil {
					t.Fatal(err)
				}
			}
			id := mustID(t, s)
			if c.stored != "" {
				if _, err := s.st.DB().Exec(`UPDATE artifacts SET conversation_id=? WHERE id=?`, c.stored, id); err != nil {
					t.Fatal(err)
				}
			}
			res, err := s.Chat(ctx, id, 0)
			if err != nil || res.ConversationID != c.want || res.Created != c.created {
				t.Fatalf("chat %+v %v", res, err)
			}
			if c.created && f.made[0] != "ws1|"+c.operator+"|About weekly" {
				t.Fatalf("made %v", f.made)
			}
			again, err := s.Chat(ctx, id, 0)
			if err != nil || again.ConversationID != c.want || again.Created || len(f.made) > 1 {
				t.Fatalf("second chat %+v %v %v", again, err, f.made)
			}
			if a, _ := s.Get(ctx, id); a.ConversationID != c.want {
				t.Fatalf("stored %q", a.ConversationID)
			}
			if len(f.system) != 1 || !strings.HasPrefix(f.system[0], c.want+"|This chat is about artifact weekly (weekly), version ") {
				t.Fatalf("system %v", f.system)
			}
		})
	}
}

func TestChatContextPerVersion(t *testing.T) {
	t.Parallel()
	s := newTestService(t, mapRuns{}, nil)
	f := newFakeCont()
	s.WithContinuer(f)
	ctx := context.Background()
	user := Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}
	if _, err := put(t, s, user, nil, "one"); err != nil {
		t.Fatal(err)
	}
	id := mustID(t, s)
	for range 2 {
		if _, err := s.Chat(ctx, id, 0); err != nil {
			t.Fatal(err)
		}
	}
	one := 1
	if _, err := put(t, s, user, &one, "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.system) != 1 {
		t.Fatalf("an older version adds no line: %v", f.system)
	}
	if _, err := s.Chat(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	if len(f.system) != 2 || !strings.Contains(f.system[1], "version 2. Read it with `trebi artifact get weekly@2`") {
		t.Fatalf("system %v", f.system)
	}
	if _, err := s.Chat(ctx, id, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
}

func TestAsk(t *testing.T) {
	t.Parallel()
	s := newTestService(t, mapRuns{}, nil)
	f := newFakeCont("c1")
	s.WithContinuer(f)
	ctx := context.Background()
	if _, err := put(t, s, Origin{Kind: OriginConversation, ConversationID: "c1", OwnerKind: OwnerAssistant}, nil, "mine"); err != nil {
		t.Fatal(err)
	}
	id := mustID(t, s)
	res, err := s.Ask(ctx, AskOpts{ID: id, Text: "Shorter", Selection: strings.Repeat("a", 3000)})
	if err != nil || res.Via != OriginConversation || res.ConversationID != "c1" || len(f.sent) != 1 {
		t.Fatalf("ask %+v %v", res, err)
	}
	if !strings.HasPrefix(f.sent[0], "ws1|c1|The user asks about artifact weekly, version 1.") || strings.Count(f.sent[0], "a") > 2100 {
		t.Fatalf("prompt %.200s", f.sent[0])
	}
	if _, err := s.Ask(ctx, AskOpts{ID: id}); err == nil {
		t.Fatal("empty text must fail")
	}
}

func TestListByConversation(t *testing.T) {
	t.Parallel()
	s := newTestService(t, mapRuns{}, nil)
	s.WithContinuer(newFakeCont())
	ctx := context.Background()
	if _, err := put(t, s, Origin{Kind: OriginConversation, ConversationID: "c1", OwnerKind: OwnerAssistant}, nil, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, PutOpts{Workspace: "ws1", Name: "other", FileName: "o.md", Content: strings.NewReader("x"),
		Origin: Origin{Kind: OriginUser, OwnerKind: OwnerAssistant}}); err != nil {
		t.Fatal(err)
	}
	list, _, err := s.List(ctx, ListFilter{Workspace: "ws1", Conversation: "c1"})
	if err != nil || len(list) != 1 || list[0].Name != "weekly" {
		t.Fatalf("by version %v %v", list, err)
	}
	other, _, _ := s.List(ctx, ListFilter{Workspace: "ws1", Query: "other"})
	res, err := s.Chat(ctx, other[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	list, _, err = s.List(ctx, ListFilter{Workspace: "ws1", Conversation: res.ConversationID})
	if err != nil || len(list) != 1 || list[0].Name != "other" {
		t.Fatalf("by chat %v %v", list, err)
	}
}

func newFakeCont(live ...string) *fakeCont {
	f := &fakeCont{live: map[string]bool{}}
	for _, id := range live {
		f.live[id] = true
	}
	return f
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestService(t *testing.T, rs Runs, jobs Jobs) *Service {
	t.Helper()
	return NewService(openStore(t), NewBlobs(t.TempDir(), 1<<20), rs, jobs, Config{MaxBytes: 1 << 20, KeepVersions: 3})
}

func blobCount(t *testing.T, b *Blobs) int {
	t.Helper()
	n := 0
	if err := b.Walk(func(string, int64, time.Time) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

func put(t *testing.T, s *Service, o Origin, base *int, body string) (Version, error) {
	t.Helper()
	return s.Put(context.Background(), PutOpts{
		Workspace: "ws1", Name: "weekly", FileName: "weekly.md", Content: strings.NewReader(body), Origin: o, Base: base,
	})
}

func mustID(t *testing.T, s *Service) string {
	t.Helper()
	list, _, err := s.List(context.Background(), ListFilter{Workspace: "ws1"})
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	return list[0].ID
}
