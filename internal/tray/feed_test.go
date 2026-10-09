package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

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
			name:   "read withdraws a posted notice",
			events: []busEvent{ev("inbox.created", "i1", "unread"), ev("inbox.updated", "i1", "unread"), ev("inbox.updated", "i1", "resolved")},
			posted: []Notice{{ID: "i1", Title: "Run needs input", Body: "Answer it", Route: "/run/r1?tab=session&request=q1"}},
			pulled: []string{"i1"},
		},
		{
			name:   "update of an item not posted does nothing",
			events: []busEvent{ev("inbox.updated", "i9", "read")},
		},
		{
			name:   "read all withdraws every notice",
			events: []busEvent{ev("inbox.created", "i2", "unread"), ev("inbox.updated", "", "read")},
			posted: []Notice{{ID: "i2", Title: "Job failed", Body: "exit 1", Route: "/inbox/i2"}},
			pulled: []string{"i2"},
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
			f := NewFeed(srv.URL, func(n Notice) { posted = append(posted, n) }, func(id string) { pulled = append(pulled, id) })
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
