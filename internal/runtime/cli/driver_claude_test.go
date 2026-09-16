package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// replayFixture feeds one NDJSON fixture through the protocol parser.
func replayFixture(t *testing.T, path string, proto wireAdapter) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		out = append(out, proto.Parse(line)...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return out
}

func countType(evs []Event, typ EventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestClaudeFixtureBasic(t *testing.T) {
	t.Parallel()
	proto := newClaudeProtocol()
	evs := replayFixture(t, "testdata/claude/basic.ndjson", proto)
	if got := countType(evs, EventInit); got != 1 {
		t.Errorf("init events: %d", got)
	}
	if got := countType(evs, EventAssistant); got != 2 {
		t.Errorf("assistant events: %d (%+v)", got, evs)
	}
	if got := countType(evs, EventTool); got != 2 {
		t.Errorf("tool events: %d", got)
	}
	var result *Event
	for i := range evs {
		if evs[i].Type == EventResult {
			result = &evs[i]
		}
	}
	if result == nil || result.Result == nil {
		t.Fatalf("no result: %+v", evs)
	}
	if result.Result.Subtype != "success" || result.Result.IsError {
		t.Fatalf("result: %+v", result.Result)
	}
	if result.Result.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("session id: %q", result.Result.SessionID)
	}
	if result.Result.Usage.Output != 25 || result.Result.Usage.Input != 100 {
		t.Fatalf("usage: %+v", result.Result.Usage)
	}
	if result.Result.Usage.CostUSD != 0.0123 {
		t.Fatalf("cost: %v", result.Result.Usage.CostUSD)
	}
}

// TestClaudePermissionRoundtrip pins the control_request/control_response
// shapes (E.1): allow echoes updatedInput, deny carries a message.
func TestClaudePermissionRoundtrip(t *testing.T) {
	t.Parallel()
	proto := newClaudeProtocol()
	evs := replayFixture(t, "testdata/claude/permission.ndjson", proto)
	var perm *Event
	for i := range evs {
		if evs[i].Type == EventPermission {
			perm = &evs[i]
		}
	}
	if perm == nil || perm.Permission == nil {
		t.Fatalf("no permission event: %+v", evs)
	}
	if perm.Permission.ID != "req_9_abc12345" || perm.Permission.Tool != "Bash" {
		t.Fatalf("permission: %+v", perm.Permission)
	}

	frame, err := proto.EncodeDecision(perm.Permission.ID, Decision{Allow: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "control_response" {
		t.Fatalf("type: %v", m["type"])
	}
	resp, _ := m["response"].(map[string]any)
	if resp["subtype"] != "success" || resp["request_id"] != "req_9_abc12345" {
		t.Fatalf("response envelope: %+v", resp)
	}
	inner, _ := resp["response"].(map[string]any)
	if inner["behavior"] != "allow" {
		t.Fatalf("behavior: %+v", inner)
	}
	updated, _ := inner["updatedInput"].(map[string]any)
	if updated["command"] != "echo side-effect > /tmp/trebi-perm" {
		t.Fatalf("updatedInput must echo the original input: %+v", inner)
	}

	deny, err := proto.EncodeDecision(perm.Permission.ID, Decision{Allow: false, Message: "no"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(deny, &m); err != nil {
		t.Fatal(err)
	}
	resp, _ = m["response"].(map[string]any)
	inner, _ = resp["response"].(map[string]any)
	if inner["behavior"] != "deny" || inner["message"] != "no" {
		t.Fatalf("deny: %+v", inner)
	}
}

// TestClaudeErrorTaxonomy pins the classification fixtures (L.1).
func TestClaudeErrorTaxonomy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fixture string
		want    FailureClass
		end     string
	}{
		{"testdata/claude/error_limit.ndjson", FailLimit, "limit"},
		{"testdata/claude/error_auth.ndjson", FailAuth, "auth"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			proto := newClaudeProtocol()
			evs := replayFixture(t, tc.fixture, proto)
			var result *Result
			for i := range evs {
				if evs[i].Type == EventResult && evs[i].Result != nil {
					result = evs[i].Result
				}
			}
			if result == nil || !result.IsError {
				t.Fatalf("expected error result: %+v", evs)
			}
			if result.EndReason != string(tc.want) {
				t.Fatalf("class: got %q want %q (%s)", result.EndReason, tc.want, result.Text)
			}
			got := Classification{Class: FailureClass(result.EndReason)}
			if got.EndReason() != tc.end {
				t.Fatalf("end reason: got %q want %q", got.EndReason(), tc.end)
			}
		})
	}
}

func TestClassifyErrorFamilies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want FailureClass
	}{
		{"401 Unauthorized", FailAuth},
		{"OAuth token has been revoked", FailAuth},
		{"You have hit your weekly limit", FailLimit},
		{"429 too many requests", FailLimit},
		{"out of credits", FailLimit},
		{"403 entitlement required", FailEntitlement},
		{"overloaded_error", FailOverloaded},
		{"something strange", FailUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyError(tc.text, 0); got.Class != tc.want {
			t.Errorf("ClassifyError(%q) = %s want %s", tc.text, got.Class, tc.want)
		}
	}
}

// TestClaudeAskUserQuestionControlFrame pins G.4's explicit check: an
// AskUserQuestion request surfaces as a can_use_tool frame the approval inbox
// can answer.
func TestClaudeAskUserQuestionControlFrame(t *testing.T) {
	t.Parallel()
	proto := newClaudeProtocol()
	evs := proto.Parse([]byte(`{"type":"control_request","request_id":"req_q1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Pick one","options":[{"label":"A"},{"label":"B"}]}]}}}`))
	var perm *Event
	for i := range evs {
		if evs[i].Type == EventPermission {
			perm = &evs[i]
		}
	}
	if perm == nil || perm.Permission == nil {
		t.Fatalf("no permission event: %+v", evs)
	}
	if perm.Permission.ID != "req_q1" || perm.Permission.Tool != "AskUserQuestion" {
		t.Fatalf("permission: %+v", perm.Permission)
	}
	if !strings.Contains(perm.Permission.Input, "Pick one") {
		t.Fatalf("input not carried: %q", perm.Permission.Input)
	}
}

// TestClaudeUnknownControlRequestIsAnswered is the protocol hang guard: an
// unimplemented control request gets an explicit error response so the CLI
// never waits unanswered (A.10).
func TestClaudeUnknownControlRequestIsAnswered(t *testing.T) {
	t.Parallel()
	proto := newClaudeProtocol()
	var sent bytes.Buffer
	proto.setWriter(&wireWriter{s: &wireSession{stdin: &sent}})
	evs := proto.Parse([]byte(`{"type":"control_request","request_id":"req_7","request":{"subtype":"set_permission_mode"}}`))
	if len(evs) != 0 {
		t.Fatalf("unexpected events: %+v", evs)
	}
	out := sent.String()
	if !strings.Contains(out, `"request_id":"req_7"`) || !strings.Contains(out, `"subtype":"error"`) {
		t.Fatalf("no explicit control response: %s", out)
	}
}

// TestClaudeVersionFloor pins the E.1 floor: probe and fail closed below it.
func TestClaudeVersionFloor(t *testing.T) {
	t.Parallel()
	if !semverAtLeast("2.1.267 (Claude Code)", claudeMinVersion) {
		t.Fatal("current version must pass the floor")
	}
	if semverAtLeast("1.9.3", claudeMinVersion) || semverAtLeast("", claudeMinVersion) {
		t.Fatal("old/unparsable version must fail the floor")
	}
	dir := t.TempDir()
	old := dir + "/claude"
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho '1.9.3 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claudeCheckVersion(context.Background(), old); err == nil {
		t.Fatal("below-floor binary must fail closed")
	}
	fresh := dir + "/claude-new"
	if err := os.WriteFile(fresh, []byte("#!/bin/sh\necho '2.4.0 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claudeCheckVersion(context.Background(), fresh); err != nil {
		t.Fatalf("current binary rejected: %v", err)
	}
}

// TestErrorTaxonomyPerHarness pins an error frame per wire to a class (L.1).
func TestErrorTaxonomyPerHarness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		evs  []Event
		want FailureClass
	}{
		{"claude rate limit", newClaudeProtocol().Parse([]byte(
			`{"type":"rate_limit_event","status":"rejected","message":"weekly usage limit reached"}`)), FailLimit},
		{"claude credit", newClaudeProtocol().Parse([]byte(
			`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["out of credits for this workspace"]}`)), FailLimit},
		{"codex limit", (&codexProtocol{}).Parse([]byte(
			`{"jsonrpc":"2.0","method":"error","params":{"message":"429 too many requests"}}`)), FailLimit},
		{"codex auth", (&codexProtocol{}).Parse([]byte(
			`{"jsonrpc":"2.0","method":"error","params":{"message":"401 unauthorized: token revoked"}}`)), FailAuth},
		{"opencode auth", (&openCodeSession{id: "ses_1", parts: map[string]int{}}).parse(map[string]any{
			"type": "session.error", "properties": map[string]any{
				"sessionID": "ses_1", "error": map[string]any{"data": map[string]any{"message": "not logged in"}},
			}}), FailAuth},
		{"opencode entitlement", (&openCodeSession{id: "ses_1", parts: map[string]int{}}).parse(map[string]any{
			"type": "session.error", "properties": map[string]any{
				"sessionID": "ses_1", "error": map[string]any{"data": map[string]any{"message": "403 entitlement required"}},
			}}), FailEntitlement},
		{"pi auth", (&piProtocol{waiters: map[string]chan map[string]any{}}).Parse([]byte(
			`{"type":"error","error":"invalid api key"}`)), FailAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			for _, ev := range tc.evs {
				if ev.Type == EventError {
					got = ev.EndReason
				}
				if ev.Type == EventResult && ev.Result != nil && ev.Result.IsError {
					got = ev.Result.EndReason
				}
			}
			if got == "" {
				got = string(ClassifyError(tc.name, 0).Class)
			}
			if got != string(tc.want) {
				t.Fatalf("class = %q want %q (%+v)", got, tc.want, tc.evs)
			}
		})
	}
	// ACP answers a prompt with a JSON-RPC error; the prompt id is the turn.
	proto := &acpProtocol{kind: acpCopilot, promptID: 3, waiters: map[int]chan map[string]any{}}
	evs := proto.Parse([]byte(`{"jsonrpc":"2.0","id":3,"error":{"code":429,"message":"usage limit exceeded"}}`))
	found := ""
	for _, ev := range evs {
		if ev.Type == EventError {
			found = ev.EndReason
		}
	}
	if found != string(FailLimit) {
		t.Fatalf("acp class = %q want %q (%+v)", found, FailLimit, evs)
	}
}

// TestClaudeArgvRendersEqualsForm pins A.9.
func TestClaudeArgvRendersEqualsForm(t *testing.T) {
	t.Parallel()
	args := equalsForm([]string{"--session-id", "abc", "--settings", "/tmp/s.json"}, "--session-id")
	if args[0] != "--session-id=abc" {
		t.Fatalf("equals form: %v", args)
	}
	if !hasFlag([]string{"--permission-mode=plan"}, "--permission-mode") {
		t.Fatal("hasFlag missed equals form")
	}
}
