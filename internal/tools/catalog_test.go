package tools

import (
	"encoding/json"
	"testing"
)

func TestCatalogSamplesBuild(t *testing.T) {
	t.Parallel()
	for tool, actions := range NewCatalog() {
		for name, entry := range actions {
			samples := append([]Input{entry.Sample}, entry.Samples...)
			for i, sample := range samples {
				if _, err := entry.Build(sample); err != nil {
					t.Errorf("%s.%s sample %d: build: %v", tool, name, i, err)
				}
			}
		}
	}
}

func TestBusPollTransform(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		wantEvents int
		wantCursor int64
	}{
		{"empty dump", "", 0, 0},
		{"keepalive only", ": ping\n\n", 0, 0},
		{"events in order", "data: {\"seq\":5,\"topic\":\"job.created\"}\n\ndata: {\"seq\":7,\"topic\":\"job.updated\"}\n\n", 2, 7},
		{"cursor takes the max seq", "data: {\"seq\":9}\n\ndata: {\"seq\":2}\n\n", 2, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := busPollTransform(nil, 200, []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			var res struct {
				Events     []map[string]any `json:"events"`
				NextCursor int64            `json:"next_cursor"`
			}
			if err := json.Unmarshal(got, &res); err != nil {
				t.Fatalf("decode %s: %v", got, err)
			}
			if len(res.Events) != c.wantEvents || res.NextCursor != c.wantCursor {
				t.Fatalf("got %d events, cursor %d; want %d events, cursor %d",
					len(res.Events), res.NextCursor, c.wantEvents, c.wantCursor)
			}
		})
	}
}

func TestConnectorSetPatch(t *testing.T) {
	t.Parallel()
	set := NewCatalog()["connector"]["set"]
	for _, c := range []struct {
		in   Input
		want string
	}{
		{Input{"name": "x", "version": ""}, `{"version":""}`},
		{Input{"name": "x", "patch": nil}, `{"patch":null}`},
		{Input{"name": "x", "manifest": map[string]any{"name": "x"}}, `{"manifest":{"name":"x"}}`},
		{Input{"name": "x", "inputs": map[string]any{"A": "1"}}, `{"inputs":{"A":"1"}}`},
		{Input{"name": "x"}, `{}`},
	} {
		req, err := set.Build(c.in)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(req.Body)
		if string(b) != c.want {
			t.Fatalf("%v: body %s, want %s", c.in, b, c.want)
		}
	}
}
