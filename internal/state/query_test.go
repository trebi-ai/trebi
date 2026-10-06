package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestQueryFilters(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	o := owner()
	seedQuery(t, s, o)
	cases := []struct {
		name string
		q    Query
		want string
	}{
		{"all by key", Query{}, "doi:1,doi:2,topic:alz,topic:none"},
		{"keys", Query{Keys: []string{"doi:2", "zz"}}, "doi:2"},
		{"prefix", Query{Prefix: "topic:"}, "topic:alz,topic:none"},
		{"fts diacritics", Query{Q: "cafe"}, "doi:1"},
		{"fts key", Query{Q: "alz"}, "topic:alz"},
		{"fts syntax chars", Query{Q: "bone-density"}, "doi:2"},
		{"fts prefix", Query{Q: "alzh*"}, "topic:alz"},
		{"since", Query{Since: "2026-09-15"}, "doi:1,topic:alz,topic:none"},
		{"until relative", Query{Until: "4d"}, "doi:1,doi:2"},
		{"where", Query{Where: "v.score >= 2"}, "doi:1,topic:alz"},
		{"where key", Query{Where: "k.startsWith('doi') && at < '2026-09-15'"}, "doi:2"},
		{"sort at desc", Query{Sort: "-at"}, "topic:none,topic:alz,doi:1,doi:2"},
		{"sort value desc", Query{Sort: "-v.score"}, "topic:alz,doi:1,doi:2,topic:none"},
		{"sort value asc", Query{Sort: "v.score"}, "doi:2,doi:1,topic:alz,topic:none"},
		{"limit", Query{Limit: 2}, "doi:1,doi:2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.q.Collection = "items"
			res, err := s.Query(context.Background(), o, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := keysOf(res); got != tc.want {
				t.Fatalf("keys = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestQueryShape(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	o := owner()
	seedQuery(t, s, o)
	ctx := context.Background()
	res, err := s.Query(ctx, o, Query{Collection: "items", Keys: []string{"topic:alz"}})
	if err != nil {
		t.Fatal(err)
	}
	row := res.Rows[0]
	if row["at"] != "2026-10-05T00:00:00Z" || row["title"] != "Alzheimer drug" {
		t.Fatalf("row = %v", row)
	}
	if n := len([]rune(row["long"].(string))); n != cutRunes+1 {
		t.Fatalf("long field has %d runes", n)
	}
	res, err = s.Query(ctx, o, Query{Collection: "items", Keys: []string{"topic:alz"}, Fields: []string{"key", "title", "v.score", "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	row = res.Rows[0]
	if len(row) != 4 || row["title"] != "Alzheimer drug" || row["v.score"] != float64(5) || row["missing"] != nil {
		t.Fatalf("fields row = %v", row)
	}
}

func TestQueryPages(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	o := owner()
	seedQuery(t, s, o)
	ctx := context.Background()
	for _, q := range []Query{{Limit: 3}, {Limit: 3, Where: "true"}} {
		q.Collection = "items"
		first, err := s.Query(ctx, o, q)
		if err != nil || first.Next == "" || len(first.Rows) != 3 {
			t.Fatalf("first page = %+v, %v", first, err)
		}
		q.Cursor = first.Next
		second, err := s.Query(ctx, o, q)
		if err != nil || second.Next != "" || keysOf(second) != "topic:none" {
			t.Fatalf("second page = %+v, %v", second, err)
		}
	}
	if _, err := s.Query(ctx, o, Query{Collection: "items", Cursor: "junk!"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestQueryErrors(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	o := owner()
	seedQuery(t, s, o)
	for _, q := range []Query{
		{Where: "v.score >="},
		{Where: "nope.x > 1"},
		{Sort: "size"},
		{Since: "soon"},
		{Since: "last-run"},
		{By: "when", Since: "7d"},
	} {
		q.Collection = "items"
		if _, err := s.Query(context.Background(), o, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("query %+v: err = %v, want ErrInvalid", q, err)
		}
	}
}

func TestQueryWhereTimeout(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{}, nil)
	o := owner()
	seedQuery(t, s, o)
	start := time.Now()
	_, err := s.Query(context.Background(), o, Query{Collection: "items", Where: "(function(){ while (true) {} })()"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestQueryScanCap(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, Limits{ScanRows: 5}, nil)
	o := owner()
	var rows []Row
	for i := range 10 {
		rows = append(rows, Row{Key: fmt.Sprintf("k%02d", i), Value: raw(fmt.Sprintf(`{"n":%d}`, i))})
	}
	mustPut(t, s, o, "c", "", rows...)
	res, err := s.Query(context.Background(), o, Query{Collection: "c", Where: "v.n % 2 == 0"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Scanned != 5 || keysOf(res) != "k00,k02,k04" {
		t.Fatalf("res = %+v", res)
	}
	res, err = s.Query(context.Background(), o, Query{Collection: "c"})
	if err != nil || res.Truncated || len(res.Rows) != 10 {
		t.Fatalf("plain query = %+v, %v", res, err)
	}
}

func TestQueryLastRun(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 6, 12, 30, 0, 0, time.UTC)
	s, _, tc := newTestStore(t, Limits{}, fakeClock{ref: RunRef{ID: "r0", Start: start}, ok: true})
	o := owner()
	mustPut(t, s, o, "c", "", Row{Key: "before", Value: raw(`{}`)})
	tc.advance(time.Hour)
	mustPut(t, s, o, "c", "", Row{Key: "after", Value: raw(`{}`)})
	res, err := s.Query(context.Background(), o, Query{Collection: "c", Since: "last-run", By: "created"})
	if err != nil || keysOf(res) != "after" {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func keysOf(res QueryResult) string {
	var ks []string
	for _, r := range res.Rows {
		ks = append(ks, fmt.Sprint(r["key"]))
	}
	return strings.Join(ks, ",")
}

func seedQuery(t *testing.T, s *Store, o Owner) {
	t.Helper()
	mustPut(t, s, o, "items", "",
		Row{Key: "doi:1", At: "2026-10-01", Value: raw(`{"title":"Café crème study","score":3,"topic":"hair"}`)},
		Row{Key: "doi:2", At: "2026-09-01", Value: raw(`{"title":"Bone density","score":1,"topic":"bone"}`)},
		Row{Key: "topic:alz", At: "2026-10-05", Value: raw(`{"title":"Alzheimer drug","score":5,"topic":"alz","long":"` + strings.Repeat("x", 400) + `"}`)},
		Row{Key: "topic:none", Value: raw(`{"title":"No score"}`)})
}
