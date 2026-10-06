package state

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestBriefEmpty(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	got, err := s.Brief(context.Background(), owner())
	if err != nil || got != "" {
		t.Fatalf("brief = %q, %v", got, err)
	}
}

func TestBrief(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, fakeClock{ref: RunRef{ID: "r1"}, ok: true})
	ctx := context.Background()
	cols, err := ParseCollections(map[string]any{
		"reported": map[string]any{
			"description": "Items already sent.\nKey is a DOI.",
			"schema": map[string]any{"type": "object", "properties": map[string]any{
				"title": map[string]any{"type": "string"}, "topic": map[string]any{"type": "string"},
			}},
		},
		"empty": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	o := owner()
	o.Collections = cols
	o.RunID = "r1"
	mustPut(t, s, o, "reported", "", Row{Key: "a", At: "2026-09-22 10:00", Value: raw(`{"title":"x","topic":"y"}`)})
	o.RunID = "r2"
	mustPut(t, s, o, "sources", "",
		Row{Key: "s1", Value: raw(`{"url":"u","score":1}`)},
		Row{Key: "s2", Value: raw(`{"url":"u"}`)})
	got, err := s.Brief(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	want := "## State\n" +
		"This job keeps its records in trebi state. Query state before you decide. Do not guess from memory.\n\n" +
		"| collection | rows | newest at  | last run | fields       | description                       |\n" +
		"|------------|------|------------|----------|--------------|-----------------------------------|\n" +
		"| empty      | 0    | -          | -        |              |                                   |\n" +
		"| reported   | 1    | 2026-09-22 | +1       | title, topic | Items already sent. Key is a DOI. |\n" +
		"| sources    | 2    | -          | -        | url, score   |                                   |\n"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("brief:\n%s\nwant prefix:\n%s", got, want)
	}
	for _, line := range []string{
		"Commands (the job comes from this run):",
		"  trebi state has <collection> <key>...",
		`MCP: tool "state" with actions summary, query, get, has, put, delete, prune, drop. Owner "self".`,
	} {
		if !strings.Contains(got, line) {
			t.Errorf("brief has no line %q", line)
		}
	}
	stats, err := s.Collections(ctx, o)
	if err != nil || len(stats) != 3 || !stats[0].Declared || stats[2].Declared {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

func TestBriefAgentAndCap(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	o := Owner{ID: "agt_0000000001"}
	for i := range 23 {
		mustPut(t, s, o, fmt.Sprintf("c%02d", i), "", Row{Key: "k", Value: raw(`{}`)})
	}
	got, err := s.Brief(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "This agent keeps") || !strings.Contains(got, "(the agent comes from this run)") {
		t.Fatalf("brief does not name the agent:\n%s", got)
	}
	if !strings.Contains(got, "(+3 more: trebi state)") || strings.Contains(got, "| c20") {
		t.Fatalf("brief has no cap:\n%s", got)
	}
}
