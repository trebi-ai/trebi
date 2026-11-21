package alerts

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
	"github.com/trebi-ai/trebi/internal/runs"
	"github.com/trebi-ai/trebi/internal/skills"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/waittest"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

func TestAgentKitFindings(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  skills: [missing-skill]\n  connectors: [missing-connector]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001','ws','research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
		t.Fatal(err)
	}
	findings := agentKitFindings(st, launchView(t, st))
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	if !codes["agent_skill_missing"] || !codes["kit_connector_missing"] {
		t.Fatalf("findings = %+v", findings)
	}
}

// TestAgentKitFindingsOneConn proves the walk holds at most one pool
// connection at a time. Eight walks at once used to hold all eight
// connections and wait for a ninth.
func TestAgentKitFindingsOneConn(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  skills: [missing-skill]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001','ws','research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
		t.Fatal(err)
	}
	view := launchView(t, st)
	st.DB().SetMaxOpenConns(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		agentKitFindings(st, view)
	}()
	select {
	case <-done:
	case <-time.After(waittest.D(10 * time.Second)):
		t.Fatal("agentKitFindings waits for a second pool connection")
	}
}

// TestSkillDoctorSkipsArchived proves an archived skill gives no finding: the
// kit may still name it, and it overwrites nothing.
func TestSkillDoctorSkipsArchived(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  skills: [old, dup]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001','ws','research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
		t.Fatal(err)
	}
	sk := skills.New(st).WithOwners(workspaces.New(st))
	ctx := context.Background()
	mk := func(ref skills.Ref) {
		t.Helper()
		if _, err := sk.Create(ctx, skills.CreateOpts{Ref: ref, SkillMD: "# s\n"}); err != nil {
			t.Fatal(err)
		}
	}
	mk(skills.GlobalRef("old"))
	mk(skills.GlobalRef("dup"))
	agentDup := skills.Ref{Scope: skills.ScopeAgent, Owner: "agt_0000000001", Name: "dup"}
	mk(agentDup)
	codes := func() map[string]bool {
		out := map[string]bool{}
		for _, f := range agentKitFindings(st, launchView(t, st)) {
			out[f.Code] = true
		}
		for _, f := range skillOverwriteFindings(ctx, sk) {
			out[f.Code] = true
		}
		return out
	}
	if !codes()["skill_overwrite"] {
		t.Fatal("setup: the agent skill should overwrite the global skill")
	}
	for _, ref := range []skills.Ref{skills.GlobalRef("old"), agentDup} {
		if _, err := sk.Archive(ctx, ref, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	got := codes()
	if got["agent_skill_missing"] || got["skill_overwrite"] {
		t.Fatalf("findings %v", got)
	}
}

func TestInsideAgentFolder(t *testing.T) {
	if !insideAgentFolder("research/notes.md", "research") ||
		!insideAgentFolder("research", "research") ||
		insideAgentFolder("research/../outside.txt", "research") {
		t.Fatal("agent folder containment mismatch")
	}
}

// TestConnectorKitFindings covers the F.5 doctor checks: an inherited name, a
// connector with no actions block, and a CLI command that is not on PATH.
func TestConnectorKitFindings(t *testing.T) {
	st := kitStore(t, []string{
		inlineYAML("actions-only", "actions:\n  cli:\n    commands: [trebi-definitely-missing-command]\n"),
		inlineYAML("events-only", "events:\n  command: \"true\"\n"),
	}, "actions-only, events-only, ghost")
	findings := agentKitFindings(st, launchView(t, st))
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	for _, want := range []string{"connector_command_missing", "kit_connector_no_actions", "kit_connector_missing"} {
		if !codes[want] {
			t.Fatalf("missing %s in %+v", want, findings)
		}
	}
}

// TestConnectorKitFindingsMCPOnly proves a kit that names an MCP-only
// connector does not panic and gives no command finding.
func TestConnectorKitFindingsMCPOnly(t *testing.T) {
	st := kitStore(t, []string{inlineYAML("gh", "actions:\n  mcp:\n    command: npx\n")}, "gh")
	for _, finding := range agentKitFindings(st, launchView(t, st)) {
		if finding.Code == "connector_command_missing" {
			t.Fatalf("unexpected finding %+v", finding)
		}
	}
}

// TestDoctorUsesLaunchView proves the doctor sees the connectors a launch
// uses (P1-24): an owned job gets the owner kit, a top-level connectors: on
// the agent adds to its kit, the built-in email name is no finding, and the
// support table is the launch one.
func TestDoctorUsesLaunchView(t *testing.T) {
	st := kitStore(t, []string{inlineYAML("gh", "actions:\n  mcp:\n    command: npx\n")}, "kit-only")
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	// The agent research (from kitStore) names kit-only in its kit. An owned
	// job with no list of its own inherits that kit.
	writeJob(t, st, dir, "research/daily", "job", "research", "---\n---\n\nRun.\n")
	// An agent with a top-level list: it adds to the kit list.
	writeJob(t, st, dir, "pi-agent", "agent", "", "---\nharness: pi\nconnectors: [gh]\nkit:\n  connectors: [kit-ghost, email]\n---\n\nRun.\n")
	byID := map[string]DoctorFinding{}
	for _, f := range agentKitFindings(st, launchView(t, st)) {
		byID[f.ID+"|"+f.Code] = f
	}
	if _, ok := byID["connector-ws-research/daily-kit-only|kit_connector_missing"]; !ok {
		t.Fatalf("owned job does not see the owner kit: %+v", byID)
	}
	if _, ok := byID["connector-ws-pi-agent-gh|kit_actions_unsupported"]; !ok {
		t.Fatalf("pi MCP action not reported: %+v", byID)
	}
	if _, ok := byID["connector-ws-pi-agent-kit-ghost|kit_connector_missing"]; !ok {
		t.Fatalf("kit list not merged with the top-level list: %+v", byID)
	}
	for k := range byID {
		if strings.Contains(k, "-email|") {
			t.Fatalf("built-in email reported: %s", k)
		}
	}
}

// TestWorkingDirMissingFinding: a working_dir that is not a folder gives one
// finding for a job and for an agent; a folder that exists gives none.
func TestWorkingDirMissingFinding(t *testing.T) {
	st := kitStore(t, nil, "")
	dir := t.TempDir()
	writeJob(t, st, dir, "acme", "agent", "", "---\nworking_dir: ./clients/gone\n---\n\nLead.\n")
	writeJob(t, st, dir, "acme/sync", "job", "acme", "---\nworking_dir: /no/such/folder\n---\n\nRun.\n")
	writeJob(t, st, dir, "ok", "job", "", "---\nworking_dir: "+dir+"\n---\n\nRun.\n")
	got := map[string]string{}
	for _, f := range agentKitFindings(st, launchView(t, st)) {
		if f.Code == "working_dir_missing" {
			got[f.ID] = f.Message
		}
	}
	want := map[string]string{
		"working-dir-ws-acme":      "agent acme has working_dir " + filepath.Join(dir, "agents", "acme", "clients", "gone") + ", which is not a folder",
		"working-dir-ws-acme/sync": "job acme/sync has working_dir /no/such/folder, which is not a folder",
	}
	if len(got) != len(want) {
		t.Fatalf("findings = %+v", got)
	}
	for id, msg := range want {
		if got[id] != msg {
			t.Fatalf("finding %s = %q; want %q", id, got[id], msg)
		}
	}
}

// TestChannelOperatorConflictFinding: two operators conflict only when
// their routes share a room of one channel connection. A reply job counts
// as its owner agent. Two reply jobs on one room give reply_room_conflict.
// A connection without a channel and a route that gets nothing give nothing.
func TestChannelOperatorConflictFinding(t *testing.T) {
	type onConn struct {
		conn string
		r    routing.Route
	}
	every := routing.GetsEveryMessage
	agent := func(name string, rooms ...string) []onConn {
		return []onConn{
			{"chat", routing.Route{Agent: name, Gets: every, Filter: job.TriggerYAML{Rooms: rooms}}},
			{"feed", routing.Route{Agent: name, Gets: every}},
		}
	}
	assistant := func(conn string, rooms ...string) onConn {
		return onConn{conn, routing.Route{Assistant: true, Gets: every, Filter: job.TriggerYAML{Rooms: rooms}}}
	}
	reply := func(name, room string) onConn {
		return onConn{"chat", routing.Route{Job: name, Gets: every, Filter: job.TriggerYAML{Rooms: []string{room}, Chat: job.ChatReply}}}
	}
	cases := []struct {
		name      string
		routes    []onConn
		jobs      []string // index names to create besides research
		conflict  []string // words the conflict message holds; nil = no finding
		replyRoom string   // the room of a reply_room_conflict; "" = none
	}{
		{name: "rooms apart", routes: append(agent("research", "a"), assistant("chat", "b"))},
		{name: "agent all, assistant one room", routes: append(agent("research"), assistant("chat", "b")),
			conflict: []string{"research", "assistant", " b;"}},
		{name: "both all rooms", routes: append(agent("research"), assistant("chat"), assistant("feed")),
			conflict: []string{"research", "assistant", "all chats"}},
		{name: "owner and its reply job", routes: append(agent("research"), reply("research/italy", "r")),
			jobs: []string{"research/italy"}},
		{name: "reply job and other agent", routes: append(agent("other"), reply("research/italy", "r")),
			jobs:     []string{"research/italy", "other"},
			conflict: []string{"research", "other", " r;"}},
		{name: "route that gets nothing", routes: []onConn{
			{"chat", routing.Route{Agent: "research", Gets: routing.GetsOff}}, assistant("chat"),
		}},
		{name: "two reply jobs one room", routes: []onConn{reply("research/italy", "r"), reply("research/spain", "r")},
			jobs:      []string{"research/italy", "research/spain"},
			replyRoom: "r"},
		{name: "two reply jobs two rooms", routes: []onConn{reply("research/italy", "r"), reply("research/spain", "s")},
			jobs: []string{"research/italy", "research/spain"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := kitStore(t, []string{
				inlineYAML("chat", "events:\n  protocol: trebi-connector/1\n  command: cat\n  types:\n    - type: message\nchannel:\n  features: [typing]\n"),
				inlineYAML("feed", "events:\n  command: cat\n"),
			}, "")
			var dir string
			if err := st.DB().QueryRow(`SELECT path FROM workspaces WHERE id='ws'`).Scan(&dir); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.jobs {
				kind, owner := "job", job.OwnerOf(name)
				if owner == "" {
					kind = "agent"
				}
				writeJob(t, st, dir, name, kind, owner, "---\n---\n\nRun.\n")
			}
			files := map[string]routing.File{}
			for _, oc := range tc.routes {
				f := files[oc.conn]
				f.Routes = append(f.Routes, oc.r)
				files[oc.conn] = f
			}
			for conn, f := range files {
				routingtest.Write(t, dir, conn, f)
			}
			var conflicts, replies []DoctorFinding
			for _, f := range agentKitFindings(st, nil) {
				switch f.Code {
				case "channel_operator_conflict":
					conflicts = append(conflicts, f)
				case "reply_room_conflict":
					replies = append(replies, f)
				}
			}
			if (tc.conflict == nil) != (len(conflicts) == 0) || len(conflicts) > 1 {
				t.Fatalf("conflict findings = %+v", conflicts)
			}
			for _, w := range tc.conflict {
				if conflicts[0].ID != "channel-operator-conflict-ws-chat" || !strings.Contains(conflicts[0].Message, w) {
					t.Fatalf("finding = %+v; want %q", conflicts[0], w)
				}
			}
			if (tc.replyRoom == "") != (len(replies) == 0) {
				t.Fatalf("reply findings = %+v", replies)
			}
			if tc.replyRoom != "" && (replies[0].ID != "reply-room-conflict-ws-chat-"+tc.replyRoom ||
				!strings.Contains(replies[0].Message, "research/italy replies")) {
				t.Fatalf("reply finding = %+v", replies[0])
			}
		})
	}
}

func TestRoomsOverlap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{nil, []string{"x"}, true},
		{[]string{"x"}, []string{"y"}, false},
		{[]string{"x", "y"}, []string{"y"}, true},
	} {
		if got := roomsOverlap(tc.a, tc.b); got != tc.want {
			t.Fatalf("roomsOverlap(%v, %v) = %v", tc.a, tc.b, got)
		}
	}
}

// inlineYAML is the connection.yaml of one inline connection whose slug and
// type are name; body is manifest YAML.
func inlineYAML(name, body string) string {
	m := "schema: trebi-connector/1\nname: " + name + "\ntitle: T\ndescription: D\nversion: 1.0.0\n" + body
	return "id: " + connectors.NewConnectionID() + "\ntype: " + name + "\nslug: " + name +
		"\nfrom: inline\nmanifest:\n  " + strings.ReplaceAll(strings.TrimRight(m, "\n"), "\n", "\n  ") + "\n"
}

func kitStore(t *testing.T, conns []string, kitConnectors string) *store.Store {
	t.Helper()
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	for _, c := range conns {
		id := strings.TrimPrefix(strings.SplitN(c, "\n", 2)[0], "id: ")
		if err := os.MkdirAll(home.ConnectionDir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home.ConnectionDir(id), "connection.yaml"), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agents", "research")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nkit:\n  connectors: [" + kitConnectors + "]\n---\n\nRun.\n"
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES ('agt_0000000001','ws','research',?,'agent',1,'claude','hash',datetime())`, filepath.Join(agentDir, "AGENT.md")); err != nil {
		t.Fatal(err)
	}
	return st
}

// launchView is the runs service the daemon wires into the doctor.
func launchView(t *testing.T, st *store.Store) ActionsChecker {
	t.Helper()
	r := runs.New(st, nil, nil)
	r.SetKitSource(workspaces.New(st))
	return r
}

// writeJob writes one job file and indexes it.
func writeJob(t *testing.T, st *store.Store, dir, name, kind, owner, md string) {
	t.Helper()
	file := "JOB.md"
	if kind == "agent" {
		file = "AGENT.md"
	}
	path := filepath.Join(dir, filepath.FromSlash(job.Dir(name)), file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	prefix := "jb_"
	if kind == "agent" {
		prefix = "agt_"
	}
	id := fmt.Sprintf("%s%010x", prefix, crc32.ChecksumIEEE([]byte(name)))
	if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at,owner_id)
		VALUES (?,'ws',?,?,?,1,'claude','hash',datetime(),COALESCE((SELECT o.id FROM job_index o WHERE o.name = ?),''))`, id, name, path, kind, owner); err != nil {
		t.Fatal(err)
	}
}

func TestStateRetiredFinding(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()
	if _, ok := stateRetiredFinding(t.Context(), db); ok {
		t.Fatal("finding on an empty store")
	}
	for _, q := range []string{
		`INSERT INTO job_index (id, workspace_id, name, path, updated_at, retired_at) VALUES
			('jb_live','ws','live','/x','2026-10-01T00:00:00Z',NULL),
			('jb_gone','ws','gone','/y','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z'),
			('jb_empty','ws','empty','/z','2026-10-01T00:00:00Z','2026-09-01T00:00:00Z')`,
		`INSERT INTO state_rows (owner_id, collection, key, value, created_at, updated_at) VALUES
			('jb_live','c','k','{}','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z'),
			('jb_gone','c','k','{}','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z'),
			('jb_gone','c','k2','{}','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f, ok := stateRetiredFinding(t.Context(), db)
	if !ok || f.Code != "state_retired" || f.Severity != "info" || !strings.Contains(f.Message, "1 deleted") || !strings.Contains(f.Message, "2026-11-01") {
		t.Fatalf("finding %+v", f)
	}
}

func TestDuplicateIDFinding(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, reason := range []string{"missing", "duplicate"} {
		if _, err := st.Append(t.Context(), store.BusEvent{
			Topic: "job.id_assigned", Entity: "job", EntityID: "jb_" + reason, Action: "updated",
			Attrs: map[string]string{"workspace": "ws", "name": "copy-" + reason, "id": "jb_" + reason, "reason": reason},
		}); err != nil {
			t.Fatal(err)
		}
	}
	f, ok := duplicateIDFinding(t.Context(), st)
	if !ok || f.Code != "id_duplicate" || !strings.Contains(f.Message, "copy-duplicate") || strings.Contains(f.Message, "copy-missing") {
		t.Fatalf("finding %+v", f)
	}
}

func TestAgentTeamUnknownFinding(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	dir := t.TempDir()
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id,name,path,git_enabled,created_at,updated_at) VALUES ('ws','ws',?,0,datetime(),datetime())`, dir); err != nil {
		t.Fatal(err)
	}
	for i, a := range []struct{ name, md string }{
		{"ceo", "---\ndescription: Runs the company.\nteam:\n  can_ask: [cfo, ghost]\n---\n\nLead.\n"},
		{"cfo", "---\ndescription: Money.\n---\n\nMoney.\n"},
	} {
		path := filepath.Join(dir, "agents", a.name, "AGENT.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(a.md), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO job_index (id,workspace_id,name,path,kind,active,harness,hash,updated_at) VALUES (?,'ws',?,?,'agent',1,'claude','hash',datetime())`, fmt.Sprintf("agt_000000000%d", i), a.name, path); err != nil {
			t.Fatal(err)
		}
	}
	var found []DoctorFinding
	for _, f := range agentKitFindings(st, nil) {
		if f.Code == "agent_team_unknown" {
			found = append(found, f)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0].Message, "ghost") {
		t.Fatalf("team findings = %+v", found)
	}
}

// TestConnectorScopeFindings proves the doctor reports a kit connector and
// an event trigger outside the scope, and an event trigger on an unknown
// connector.
func TestConnectorScopeFindings(t *testing.T) {
	st := kitStore(t, []string{inlineYAML("chat", "actions:\n  cli:\n    commands: [sh]\n") + "scope: [ws_other]\n"}, "chat")
	dir := workspaceDir(t, st)
	writeJob(t, st, dir, "research/watch", "job", "research", "---\n---\n\nRun.\n")
	for conn, id := range map[string]string{"chat": "rt_00000000c1", "nosuch": "rt_00000000d2"} {
		routingtest.Write(t, dir, conn, routing.File{Routes: []routing.Route{{ID: id, Job: "research/watch", Gets: routing.GetsEveryMessage}}})
	}
	byID := map[string]DoctorFinding{}
	for _, f := range agentKitFindings(st, launchView(t, st)) {
		byID[f.ID+"|"+f.Code] = f
	}
	for _, want := range []string{
		"connector-ws-research-chat|connector_out_of_scope",
		"trigger-connector-ws-research/watch-chat-rt_00000000c1|connector_out_of_scope",
		"trigger-connector-ws-research/watch-nosuch-rt_00000000d2|trigger_connector_unknown",
		"route-connection-ws-nosuch|route_connection_unknown",
	} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("missing %s in %+v", want, byID)
		}
	}
}

func TestAlertChannelFindings(t *testing.T) {
	st, err := store.OpenPath(filepath.Join(t.TempDir(), "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows := []struct{ id, kind, instance, status, msg, at string }{
		{"o1", "email", "", "sent", "", "2026-10-10T10:00:00Z"},
		{"o2", "email", "", "failed", "smtp 535", "2026-10-11T10:00:00Z"},
		{"o3", "channel", "slack-main", "failed", "token revoked", "2026-10-10T10:00:00Z"},
		{"o4", "channel", "slack-main", "sent", "", "2026-10-12T10:00:00Z"},
		{"o5", "channel", "discord", "canceled", "", "2026-10-12T10:00:00Z"},
	}
	for _, r := range rows {
		if _, err := st.DB().Exec(`INSERT INTO outbox (id,kind,purpose,instance,status,error_message,created_at)
			VALUES (?,?,'alert',?,?,?,?)`, r.id, r.kind, r.instance, r.status, store.NullStr(r.msg), r.at); err != nil {
			t.Fatal(err)
		}
	}
	got := alertChannelFindings(t.Context(), st.DB())
	if len(got) != 1 || got[0].ID != "alert-channel-email" || got[0].Code != "alert_channel_failed" ||
		!strings.Contains(got[0].Message, "Alerts by email do not send: smtp 535 (last try Oct 11)") {
		t.Fatalf("findings %+v", got)
	}
}

// TestTriggerAccessFindings proves the doctor reports an event trigger
// whose runs get no action of the trigger connection, and only that state.
func TestTriggerAccessFindings(t *testing.T) {
	ops := "actions:\n  operations:\n    - name: list\n      label: List\n      readonly: true\n      command: [sh]\n"
	events := "events:\n  command: cat\n"
	st := kitStore(t, []string{
		inlineYAML("reader", events+ops),
		inlineYAML("plain", events+"actions:\n  cli:\n    commands: [sh]\n"),
		inlineYAML("kitted", events+"actions:\n  cli:\n    commands: [sh]\n"),
		inlineYAML("listed", events+"actions:\n  cli:\n    commands: [sh]\n"),
		inlineYAML("chat", events+"channel:\n  features: [typing]\n"),
	}, "kitted")
	dir := workspaceDir(t, st)
	writeJob(t, st, dir, "research/watch", "job", "research", "---\nconnectors: [listed]\n---\n\nRun.\n")
	for _, conn := range []string{"reader", "plain", "kitted", "listed", "chat", "email"} {
		routes := []routing.Route{{ID: "rt_00000000a1", Job: "research/watch", Gets: routing.GetsEveryMessage}}
		if conn == "plain" {
			routes = append(routes, routing.Route{ID: "rt_00000000a2", Job: "research/watch", Gets: routing.GetsOff})
		}
		routingtest.Write(t, dir, conn, routing.File{Routes: routes})
	}
	var got []string
	for _, f := range agentKitFindings(st, nil) {
		if f.Code == "trigger_access_none" {
			got = append(got, f.ID)
			if !strings.Contains(f.Message, "runs that T starts get no actions of it") {
				t.Fatalf("message = %q", f.Message)
			}
		}
	}
	if want := []string{"trigger-access-ws-research/watch-plain-rt_00000000a1"}; !slices.Equal(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
}

// TestRouteFindings proves the doctor reports the problems of a routes
// file with the doctor severities, and the removed assistant.triggers key.
func TestRouteFindings(t *testing.T) {
	st := kitStore(t, []string{inlineYAML("chat", "events:\n  command: cat\n")}, "chat")
	dir := workspaceDir(t, st)
	writeJob(t, st, dir, "research/watch", "job", "research", "---\n---\n\nReply to {{text}}.\n")
	routingtest.WriteRaw(t, dir, "chat", "mode: on\nroutes:\n"+
		"  - id: rt_00000000e1\n    agent: gone\n    gets: every_message\n"+
		"  - id: rt_00000000e2\n    agent: research\n    job: research/watch\n"+
		"  - id: rt_00000000e3\n    job: research/watch\n    gets: every_message\n    batch: 15m\n")
	if err := os.WriteFile(filepath.Join(dir, "trebi.yaml"), []byte("assistant:\n  triggers:\n    - event: chat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sev := map[string]string{}
	for _, f := range agentKitFindings(st, launchView(t, st)) {
		sev[f.Code] = f.Severity
	}
	want := map[string]string{
		routing.CodeRouteTargetMissing: "error",
		routing.CodeInvalidRoute:       "error",
		routing.CodeNoChoices:          "warn",
		job.CodeMayBeEmpty:             "warn",
		"assistant_triggers_removed":   "error",
	}
	for code, s := range want {
		if sev[code] != s {
			t.Fatalf("%s severity = %q, want %q; all = %v", code, sev[code], s, sev)
		}
	}
	for code, s := range sev {
		if s != "error" && s != "warn" && s != "info" {
			t.Fatalf("%s has severity %q", code, s)
		}
	}
}

// kitStore writes one folder per connection and one agent whose kit names
// connectors.
// workspaceDir is the folder of workspace ws.
func workspaceDir(t *testing.T, st *store.Store) string {
	t.Helper()
	var dir string
	if err := st.DB().QueryRow(`SELECT path FROM workspaces WHERE id='ws'`).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	return dir
}
