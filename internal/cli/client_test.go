package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusErrorText(t *testing.T) {
	t.Parallel()
	c, _ := statusServer(t)
	err := c.GetJSON("/missing", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("err %v", err)
	}
	if want := "GET /missing: 404 Not Found: {\"error\":\"no row\"}\n"; err.Error() != want {
		t.Fatalf("text %q, want %q", err.Error(), want)
	}
	if !isNotFound(err) {
		t.Fatal("404 is not found")
	}
	err = c.GetJSON("/boom", nil)
	if isNotFound(err) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a 500 with 404 in the body is not a 404: %v", err)
	}
	if isNotFound(errors.New("GET /x: 404")) {
		t.Fatal("a plain error is not a 404")
	}
	if err := c.PostJSON("/denied", nil, nil); err == nil || !strings.HasPrefix(err.Error(), "POST /denied: 403 Forbidden: ") {
		t.Fatalf("post err %v", err)
	}
}

func TestStreamLinesStatusError(t *testing.T) {
	t.Parallel()
	c, _ := statusServer(t)
	err := c.StreamLines(context.Background(), "/denied", func([]byte) error { return nil })
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("err %v", err)
	}
	var got []string
	err = c.StreamLines(context.Background(), "/lines", func(b []byte) error {
		got = append(got, string(b))
		return nil
	})
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("lines %v err %v", got, err)
	}
}

func TestFetchConnectorRowNotFound(t *testing.T) {
	t.Parallel()
	c, _ := statusServer(t)
	row, err := fetchConnectorRow(c, "missing")
	if err != nil || len(row) != 0 {
		t.Fatalf("unknown ref: %v %v", row, err)
	}
	if row, err = fetchConnectorRow(c, "known"); err != nil || row["id"] != "c1" {
		t.Fatalf("known ref: %v %v", row, err)
	}
	if _, err = fetchConnectorRow(c, "boom"); err == nil {
		t.Fatal("a 500 returns the error")
	}
}

func statusServer(t *testing.T) (*APIClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.Error(w, `{"error":"no row"}`, http.StatusNotFound)
		case "/boom":
			http.Error(w, "path /v1/404/x failed", http.StatusInternalServerError)
		case "/denied":
			http.Error(w, "denied", http.StatusForbidden)
		case "/lines":
			_, _ = w.Write([]byte("a\n\nb\n"))
		case "/v1/connectors/missing":
			http.Error(w, "no row", http.StatusNotFound)
		case "/v1/connectors/boom":
			http.Error(w, "store failed", http.StatusInternalServerError)
		case "/v1/connectors/known":
			_, _ = w.Write([]byte(`{"id":"c1"}`))
		default:
			http.Error(w, "unknown", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return &APIClient{base: srv.URL, httpClient: srv.Client()}, srv
}
