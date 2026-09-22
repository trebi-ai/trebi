package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/tools"
)

func TestDispatchActorHeaders(t *testing.T) {
	t.Parallel()
	s := &Server{}
	var got http.Header
	s.apiWrapped = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	ctx := tools.WithActor(context.Background(), tools.Actor{Name: "assistant", RunID: "01ABC"})
	_, _, err := s.Dispatch(ctx, request.HTTPRequest{Method: http.MethodGet, Path: "/v1/runs"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get(tools.HeaderActor) != "assistant" || got.Get(tools.HeaderRunID) != "01ABC" {
		t.Fatalf("headers %v", got)
	}
}

func TestAuditMiddlewareLogsActor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&buf, nil))}
	h := s.auditMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set(tools.HeaderActor, "assistant")
	req.Header.Set(tools.HeaderRunID, "01ABC")
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "actor=assistant") || !strings.Contains(out, "run_id=01ABC") {
		t.Fatalf("audit log: %s", out)
	}
}
