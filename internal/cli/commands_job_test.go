package cli

import (
	"sort"
	"testing"
)

func TestIndexSchedules(t *testing.T) {
	t.Parallel()
	rows := []map[string]any{
		{
			"workspace": "ws1", "job": "alpha", "spec": "0 9 * * *",
			"timezone": "UTC", "next_due_at": "2026-08-29T09:00:00Z", "last_fired_at": "2026-08-28T09:00:00Z",
		},
		{
			"workspace": "ws1", "job": "alpha", "spec": "@every 1h",
			"timezone": "UTC", "next_due_at": "2026-08-28T11:00:00Z", "last_fired_at": "2026-08-28T10:00:00Z",
		},
		{
			"workspace": "ws2", "job": "beta", "spec": "0 * * * *",
			"timezone": "America/Sao_Paulo", "next_due_at": "", "last_fired_at": "",
		},
	}
	got := indexSchedules(rows)
	alpha := got["ws1/alpha"]
	if alpha.Spec != "0 9 * * *, @every 1h" {
		t.Fatalf("spec=%q", alpha.Spec)
	}
	if alpha.Timezone != "UTC" {
		t.Fatalf("tz=%q", alpha.Timezone)
	}
	if alpha.Next != "2026-08-28T11:00:00Z" {
		t.Fatalf("next=%q (want earliest)", alpha.Next)
	}
	if alpha.Last != "2026-08-28T10:00:00Z" {
		t.Fatalf("last=%q (want latest)", alpha.Last)
	}
	beta := got["ws2/beta"]
	if beta.Spec != "0 * * * *" || beta.Timezone != "America/Sao_Paulo" {
		t.Fatalf("beta=%+v", beta)
	}
}

func TestJobScheduleKey(t *testing.T) {
	t.Parallel()
	got := jobScheduleKey(map[string]any{"workspace_id": "ws1", "name": "alpha"})
	if got != "ws1/alpha" {
		t.Fatalf("got %q", got)
	}
}

func TestCommandsAlphabetical(t *testing.T) {
	t.Parallel()
	app := NewApp(Options{})
	assertCommandsSorted(t, "trebi", app.Commands)
}

func assertCommandsSorted(t *testing.T, path string, cmds []*urfaveCommand) {
	t.Helper()
	var visible []string
	for _, c := range cmds {
		if c == nil || c.Hidden {
			continue
		}
		visible = append(visible, c.Name)
		assertCommandsSorted(t, path+" "+c.Name, c.Commands)
	}
	if !sort.StringsAreSorted(visible) {
		t.Fatalf("%s subcommands not alphabetical: %v", path, visible)
	}
}
