package guides

import (
	"errors"
	"regexp"
	"testing"
)

var kebab = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestParseAll(t *testing.T) {
	t.Parallel()
	all, err := ParseAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Fatalf("got %d guides, want 10", len(all))
	}
	seen := map[string]bool{}
	for _, g := range all {
		if g.Topic == "" || g.Title == "" || g.Description == "" {
			t.Errorf("empty fields: %+v", g)
		}
		if !kebab.MatchString(g.Topic) {
			t.Errorf("topic %q is not kebab-case", g.Topic)
		}
		if seen[g.Topic] {
			t.Errorf("duplicate topic %q", g.Topic)
		}
		seen[g.Topic] = true
		if g.Body == "" || g.Body[len(g.Body)-1] != '\n' {
			t.Errorf("topic %q: empty body or missing trailing newline", g.Topic)
		}
	}
	if err := LoadError(); err != nil {
		t.Fatal(err)
	}
}

func TestListSortedNoBodies(t *testing.T) {
	t.Parallel()
	list := List()
	if len(list) != 10 {
		t.Fatalf("List len %d", len(list))
	}
	for i, g := range list {
		if g.Body != "" {
			t.Errorf("List[%d] %q has body", i, g.Topic)
		}
		if i > 0 && list[i-1].Topic >= g.Topic {
			t.Errorf("List not sorted: %q then %q", list[i-1].Topic, g.Topic)
		}
	}
}

func TestGetRoundTrip(t *testing.T) {
	t.Parallel()
	for _, item := range List() {
		g, err := Get(item.Topic)
		if err != nil {
			t.Fatalf("Get %q: %v", item.Topic, err)
		}
		if g.Topic != item.Topic || g.Title != item.Title || g.Description != item.Description {
			t.Errorf("Get %q meta mismatch", item.Topic)
		}
		if g.Body == "" {
			t.Errorf("Get %q empty body", item.Topic)
		}
	}
}

func TestGetUnknown(t *testing.T) {
	t.Parallel()
	_, err := Get("no-such-topic")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
}
