package machines

import (
	"context"
	"reflect"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/job"
	"github.com/trebi-ai/trebi/internal/secrets"
)

func TestReadiness(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	if err := secrets.Set("openai_key", "x"); err != nil {
		t.Fatal(err)
	}
	conns := fakeConns{
		rows: []map[string]any{
			{"id": "con_slack", "slug": "slack", "provenance": "managed", "login": map[string]any{"state": "connected"}},
			{"id": "con_gh", "slug": "github", "provenance": "managed", "login": map[string]any{"state": "auth_required"}},
		},
		res: map[string]connectors.Resolved{
			"slack":  {ID: "con_slack", Inputs: map[string]string{"SLACK_TOKEN": "secret:slack_token"}},
			"github": {ID: "con_gh"},
		},
		errs: map[string]error{"linear": connectors.ErrNotInstalled},
	}
	ready := fakeAuth{"claude": authState(1), "codex": authState(0)}
	loc := fakeLocator{"claude": true, "codex": true, "opencode": true}

	for _, tc := range []struct {
		name  string
		needs Needs
		want  []Item
	}{
		{
			name:  "all ready",
			needs: Needs{Harness: "claude", Secrets: []string{"openai_key"}},
			want: []Item{
				{KindHarness, "claude", StateReady, ""},
				{KindSecret, "openai_key", StateReady, ""},
			},
		},
		{
			name:  "default harness",
			needs: Needs{},
			want:  []Item{{KindHarness, "claude", StateReady, ""}},
		},
		{
			name:  "missing login",
			needs: Needs{Harness: "codex"},
			want:  []Item{{KindHarness, "codex", StateLoggedOut, "Log in"}},
		},
		{
			name:  "harness off",
			needs: Needs{Harness: "opencode"},
			want:  []Item{{KindHarness, "opencode", StateOff, "Turn on"}},
		},
		{
			name:  "harness not installed",
			needs: Needs{Harness: "gemini"},
			want:  []Item{{KindHarness, "gemini", StateNotInstalled, "Install"}},
		},
		{
			name:  "missing connector",
			needs: Needs{Harness: "claude", Connectors: []string{"notion", "linear", "github"}},
			want: []Item{
				{KindHarness, "claude", StateReady, ""},
				{KindConnector, "notion", StateMissing, "Connect"},
				{KindConnector, "linear", StateNotInstalled, "Install"},
				{KindConnector, "github", StateLoggedOut, "Log in"},
			},
		},
		{
			name:  "missing secret",
			needs: Needs{Harness: "claude", Connectors: []string{"slack"}, Secrets: []string{"stripe_key", "openai_key"}},
			want: []Item{
				{KindHarness, "claude", StateReady, ""},
				{KindConnector, "slack", StateReady, ""},
				{KindSecret, "openai_key", StateReady, ""},
				{KindSecret, "slack_token", StateMissing, "Add secret"},
				{KindSecret, "stripe_key", StateMissing, "Add secret"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(st, ready, loc, conns, nil)
			got, err := svc.Readiness(ctx, "", tc.needs)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("readiness =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

func TestReadinessOtherMachine(t *testing.T) {
	testHome(t)
	ctx := context.Background()
	st := openStore(t)
	now := "2026-10-30T00:00:00Z"
	if _, err := st.DB().Exec(`INSERT INTO machines (id, name, is_self, created_at, updated_at) VALUES ('mch_other', 'Other', 0, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO workspaces (id, name, handle, path, machine_id, created_at, updated_at) VALUES ('ws_1', 'demo', 'demo', '/x', 'mch_other', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	svc := New(st, fakeAuth{}, fakeLocator{}, fakeConns{}, nil)
	got, err := svc.Readiness(ctx, "ws_1", Needs{Harness: "claude", Connectors: []string{"slack"}, Secrets: []string{"k"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{{KindHarness, "claude", StateUnknown, ""}, {KindConnector, "slack", StateUnknown, ""}, {KindSecret, "k", StateUnknown, ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readiness = %+v", got)
	}
}

func TestNeedsOf(t *testing.T) {
	t.Parallel()
	j := &job.Job{Harness: "codex", Connectors: []string{"github", job.EmailConnector, "slack"}, Env: map[string]string{"A": "secret:a_key", "B": "plain"}}
	kit := &job.KitYAML{Connectors: []string{"slack", "notion"}, Env: map[string]string{"C": "secret:c_key"}}
	got := NeedsOf(j, kit)
	want := Needs{Harness: "codex", Connectors: []string{"slack", "notion", "github"}, Secrets: []string{"a_key", "c_key"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("needs = %+v, want %+v", got, want)
	}
}
