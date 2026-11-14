package tray

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestFeedNotificationsRequested(t *testing.T) {
	t.Parallel()
	asked := 0
	f := NewFeed("http://127.0.0.1:1", func(Notice) {}, func(string) {}, func() { asked++ })
	f.handle(context.Background(), busEvent{Topic: "inbox.updated"})
	if asked != 0 {
		t.Fatalf("asked %d times on an inbox frame", asked)
	}
	f.handle(context.Background(), busEvent{Topic: "system.notifications_requested"})
	if asked != 1 {
		t.Fatalf("asked %d times, want 1", asked)
	}
}

func TestReportNotifications(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Status string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = r.Method + " " + r.URL.Path + " " + body.Status
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	if err := ReportNotifications(context.Background(), srv.Client(), srv.URL, NotificationsDenied); err != nil {
		t.Fatal(err)
	}
	if want := "PUT /v1/system/notifications denied"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFeedHandle(t *testing.T) {
	t.Parallel()
	items := map[string]string{
		"i1": `{"id":"i1","title":"Run needs input","body":"","hint":"Answer it","actions":[{"route":"/v1/runs/r1/attach"},{"route":"/run/r1?tab=session&request=q1"}]}`,
		"i2": `{"id":"i2","title":"Job failed","body":"exit 1","actions":[{"route":"/v1/runs/r2/retry"}]}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/inbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, ok := items[r.PathValue("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ev := func(topic, id, status string) busEvent {
		e := busEvent{Topic: topic, ID: id}
		e.Attrs.Status = status
		return e
	}
	upd := func(op string, ids ...string) busEvent {
		e := busEvent{Topic: "inbox.updated"}
		if len(ids) == 1 {
			e.ID = ids[0]
		}
		e.Attrs.Op, e.Attrs.IDs = op, ids
		return e
	}
	tests := []struct {
		name   string
		events []busEvent
		posted []Notice
		pulled []string
	}{
		{
			name:   "created unread posts with the first UI route",
			events: []busEvent{ev("inbox.created", "i1", "unread")},
			posted: []Notice{{ID: "i1", Title: "Run needs input", Body: "Answer it", Route: "/run/r1?tab=session&request=q1"}},
		},
		{
			name:   "no UI route falls back to the item",
			events: []busEvent{ev("inbox.created", "i2", "unread")},
			posted: []Notice{{ID: "i2", Title: "Job failed", Body: "exit 1", Route: "/inbox/i2"}},
		},
		{
			name:   "created read posts nothing",
			events: []busEvent{ev("inbox.created", "i1", "read")},
		},
		{
			name:   "archive withdraws a posted notice",
			events: []busEvent{ev("inbox.created", "i1", "unread"), upd("update", "i1"), upd("unread", "i1"), upd("archive", "i1")},
			posted: []Notice{{ID: "i1", Title: "Run needs input", Body: "Answer it", Route: "/run/r1?tab=session&request=q1"}},
			pulled: []string{"i1"},
		},
		{
			name:   "update of an item not posted does nothing",
			events: []busEvent{upd("read", "i9")},
		},
		{
			name:   "bulk read withdraws every named notice",
			events: []busEvent{ev("inbox.created", "i2", "unread"), upd("read", "i2", "i7")},
			posted: []Notice{{ID: "i2", Title: "Job failed", Body: "exit 1", Route: "/inbox/i2"}},
			pulled: []string{"i2"},
		},
		{
			name:   "prune withdraws a posted notice",
			events: []busEvent{ev("inbox.created", "i1", "unread"), upd("prune", "i1", "i3")},
			posted: []Notice{{ID: "i1", Title: "Run needs input", Body: "Answer it", Route: "/run/r1?tab=session&request=q1"}},
			pulled: []string{"i1"},
		},
		{
			name:   "missing item posts nothing",
			events: []busEvent{ev("inbox.created", "gone", "unread")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var posted []Notice
			var pulled []string
			f := NewFeed(srv.URL, func(n Notice) { posted = append(posted, n) }, func(id string) { pulled = append(pulled, id) }, nil)
			for _, e := range tt.events {
				f.handle(context.Background(), e)
			}
			if !reflect.DeepEqual(posted, tt.posted) {
				t.Errorf("posted %+v, want %+v", posted, tt.posted)
			}
			if !reflect.DeepEqual(pulled, tt.pulled) {
				t.Errorf("pulled %v, want %v", pulled, tt.pulled)
			}
		})
	}
}
