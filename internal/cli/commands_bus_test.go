package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/store"
)

func TestStreamEventsQueryPassthrough(t *testing.T) {
	var gotPath string
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(1, "run.failed", "run", "01", "updated"))
		flusher.Flush()
		<-r.Context().Done()
	})
	_, err := runCLI(t, streamClient(srv), "bus", "--topics", "run.failed,alert", "--cursor", "42", "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "topics=run.failed%2Calert") && !strings.Contains(gotPath, "topics=run.failed,alert") {
		t.Fatalf("topics not passed: %s", gotPath)
	}
	if !strings.Contains(gotPath, "cursor=42") {
		t.Fatalf("cursor not passed: %s", gotPath)
	}
}

func TestStreamEventsFamilyPrefixNotExpanded(t *testing.T) {
	var got string
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("topics")
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(1, "run.created", "run", "01", "created"))
		flusher.Flush()
		<-r.Context().Done()
	})
	if _, err := runCLI(t, streamClient(srv), "bus", "--topics", "run", "--limit", "1", "--replay"); err != nil {
		t.Fatal(err)
	}
	if got != "run" {
		t.Fatalf("topics expanded client-side: %q", got)
	}
}

func TestStreamEventsSSEFraming(t *testing.T) {
	t.Parallel()
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "\n")
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(3, "run.created", "run", "01", "created"))
		flusher.Flush()
		<-r.Context().Done()
	})
	var n int
	err := streamClient(srv).StreamEvents(context.Background(), "/v1/stream", func(ev store.StreamEvent) error {
		n++
		if ev.Seq != 3 {
			t.Fatalf("seq %d", ev.Seq)
		}
		return errBusLimit
	})
	if !errors.Is(err, errBusLimit) {
		t.Fatalf("%v", err)
	}
	if n != 1 {
		t.Fatalf("keepalive/blank counted: n=%d", n)
	}
}

func TestStreamEventsSplitFrame(t *testing.T) {
	t.Parallel()
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		fmt.Fprint(w, "data: {\"seq\":1")
		flusher.Flush()
		fmt.Fprint(w, ",\"topic\":\"run.created\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	var n int
	err := streamClient(srv).StreamEvents(context.Background(), "/v1/stream", func(ev store.StreamEvent) error {
		n++
		if ev.Seq != 1 || ev.Topic != "run.created" {
			t.Fatalf("%+v", ev)
		}
		return errBusLimit
	})
	if !errors.Is(err, errBusLimit) || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestStreamEventsOversizedFrame(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 70*1024)
	payload := fmt.Sprintf(`{"seq":1,"topic":"run.created","attrs":{"n":"%s"}}`, big)
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
		<-r.Context().Done()
	})
	var n int
	err := streamClient(srv).StreamEvents(context.Background(), "/v1/stream", func(ev store.StreamEvent) error {
		n++
		if ev.Seq != 1 {
			t.Fatalf("%+v", ev)
		}
		return errBusLimit
	})
	if !errors.Is(err, errBusLimit) || n != 1 {
		t.Fatalf("oversized frame truncated n=%d err=%v", n, err)
	}
}

func TestBusSystemResetNotCounted(t *testing.T) {
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"topic\":\"system.reset\",\"seq\":10}\n\n")
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(11, "run.created", "run", "01", "created"))
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(12, "run.updated", "run", "01", "updated"))
		flusher.Flush()
		<-r.Context().Done()
	})
	out, err := runCLI(t, streamClient(srv), "bus", "--replay", "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "system.reset") && !strings.Contains(out, "bus gap") {
		t.Fatalf("missing gap notice: %q", out)
	}
	if !strings.Contains(out, "run.created") {
		t.Fatalf("missing live frame: %q", out)
	}
	if strings.Contains(out, "run.updated") {
		t.Fatalf("reset counted toward limit: %q", out)
	}
}

func TestBusClientFilters(t *testing.T) {
	frames := []string{
		frameJSON(1, "run.created", "run", "01ABC", "created"),
		frameJSON(2, "job.updated", "job", "demo", "updated"),
		frameJSON(3, "run.updated", "run", "01ABC", "updated"),
		frameJSON(4, "run.failed", "run", "99ZZZ", "updated"),
	}
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		flusher.Flush()
		<-r.Context().Done()
	})
	out, err := runCLI(t, streamClient(srv), "bus", "--replay", "--entity", "run", "--id", "01ABC", "--action", "updated", "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "01ABC") || !strings.Contains(out, "run.updated") {
		t.Fatalf("%q", out)
	}
	if strings.Contains(out, "job.updated") || strings.Contains(out, "99ZZZ") || strings.Contains(out, "run.created") {
		t.Fatalf("filter leaked: %q", out)
	}
}

func TestBusSinceInclusive(t *testing.T) {
	bound := "2026-08-28T12:00:00Z"
	before, _ := json.Marshal(store.StreamEvent{
		Seq: 1, Topic: "run.created", TS: time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC),
	})
	on, _ := json.Marshal(store.StreamEvent{
		Seq: 2, Topic: "run.updated", TS: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	})
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", before)
		fmt.Fprintf(w, "data: %s\n\n", on)
		flusher.Flush()
		<-r.Context().Done()
	})
	out, err := runCLI(t, streamClient(srv), "bus", "--replay", "--since", bound, "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "run.updated") {
		t.Fatalf("bound frame dropped: %q", out)
	}
	if strings.Contains(out, "run.created") {
		t.Fatalf("earlier frame kept: %q", out)
	}
}

func TestBusJSONL(t *testing.T) {
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(1, "run.created", "run", "01", "created"))
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(2, "run.updated", "run", "01", "updated"))
		flusher.Flush()
		<-r.Context().Done()
	})
	out, err := runCLI(t, streamClient(srv), "bus", "--replay", "--json", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d: %q", len(lines), out)
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d: %v %q", i, err, line)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatal("JSONL must not be a wrapping array")
	}
}

func TestBusTopicValidationNoRequest(t *testing.T) {
	var called atomic.Bool
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(200)
	})
	_, err := runCLI(t, streamClient(srv), "bus", "--topics", "runs.failed")
	if err == nil || !strings.Contains(err.Error(), "run.failed") {
		t.Fatalf("want near-miss, got %v", err)
	}
	if called.Load() {
		t.Fatal("HTTP request reached handler")
	}
}

func TestBusDroppedConnectionNamesSeq(t *testing.T) {
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(7, "run.created", "run", "01", "created"))
		flusher.Flush()
	})
	_, err := runCLI(t, streamClient(srv), "bus", "--replay")
	if err == nil {
		t.Fatal("expected drop error")
	}
	if !strings.Contains(err.Error(), "seq 7") || !strings.Contains(err.Error(), "--cursor 7") {
		t.Fatalf("%v", err)
	}
}

func TestStreamEventsCancel(t *testing.T) {
	t.Parallel()
	srv := fakeStream(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- streamClient(srv).StreamEvents(ctx, "/v1/stream", func(store.StreamEvent) error { return nil })
	}()
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestStreamEventsTimeoutNotInherited(t *testing.T) {
	t.Parallel()
	srv := fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		time.Sleep(80 * time.Millisecond)
		fmt.Fprintf(w, "data: %s\n\n", frameJSON(1, "run.created", "run", "01", "created"))
		flusher.Flush()
		<-r.Context().Done()
	})
	client := &APIClient{Base: srv.URL, Token: "t", HTTP: &http.Client{Timeout: 30 * time.Millisecond}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got store.StreamEvent
	err := client.StreamEvents(ctx, "/v1/stream", func(ev store.StreamEvent) error {
		got = ev
		return errBusLimit
	})
	if !errors.Is(err, errBusLimit) {
		t.Fatalf("%v", err)
	}
	if got.Seq != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestNormalizeBusTopics(t *testing.T) {
	t.Parallel()
	got, err := normalizeBusTopics("run.failed, alert")
	if err != nil || got != "run.failed,alert" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := normalizeBusTopics("runs.failed"); err == nil {
		t.Fatal("expected typo error")
	}
	got, err = normalizeBusTopics("run")
	if err != nil || got != "run" {
		t.Fatalf("family %q %v", got, err)
	}
}

func TestBusFilterMatch(t *testing.T) {
	t.Parallel()
	ev := store.StreamEvent{Entity: "run", ID: "01", Action: "created", TS: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)}
	f := busFilter{entity: "run", id: "01", action: "created"}
	if !f.match(ev) {
		t.Fatal("should match")
	}
	f.id = "nope"
	if f.match(ev) {
		t.Fatal("id should drop")
	}
	f = busFilter{since: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)}
	if !f.match(ev) {
		t.Fatal("exact since should be kept")
	}
	f.since = time.Date(2026, 8, 28, 12, 0, 1, 0, time.UTC)
	if f.match(ev) {
		t.Fatal("before since should drop")
	}
}

func fakeStream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return fakeStreamHandler(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			flusher.Flush()
		}
		<-r.Context().Done()
	})
}

func fakeStreamHandler(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func streamClient(srv *httptest.Server) *APIClient {
	return &APIClient{Base: srv.URL, Token: "t", HTTP: srv.Client()}
}

func frameJSON(seq int64, topic, entity, id, action string) string {
	b, _ := json.Marshal(store.StreamEvent{
		Seq: seq, Topic: topic, Entity: entity, ID: id, Action: action, Revision: 1,
		TS: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	})
	return string(b)
}
