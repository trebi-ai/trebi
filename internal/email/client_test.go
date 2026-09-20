package email

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeWorker serves the contract endpoints well enough to drive the
// client. It records the last request per route for assertions.
type fakeWorker struct {
	mu     sync.Mutex
	token  string
	health Health

	lastPull  string // raw query
	lastAck   []byte
	lastSend  []byte
	pulled    []IngressMessage
	pullNext  int64
	sentID    string
	attach    []byte
	ackStatus int

	sendScript []int // statuses served in order before the success reply
}

func (f *fakeWorker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ingress", func(w http.ResponseWriter, r *http.Request) {
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.lastPull = r.URL.RawQuery
		msgs, next := f.pulled, f.pullNext
		f.mu.Unlock()
		writeJSON(w, map[string]any{"messages": msgs, "next": next})
	})
	mux.HandleFunc("POST /v1/ingress/ack", func(w http.ResponseWriter, r *http.Request) {
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.lastAck = readBody(r)
		status := f.ackStatus
		f.mu.Unlock()
		w.WriteHeader(status)
	})
	mux.HandleFunc("POST /v1/send", func(w http.ResponseWriter, r *http.Request) {
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.lastSend = readBody(r)
		sentID := f.sentID
		var status int
		if len(f.sendScript) > 0 {
			status = f.sendScript[0]
			f.sendScript = f.sendScript[1:]
		}
		f.mu.Unlock()
		switch status {
		case 0:
			writeJSON(w, map[string]any{"message_id": sentID})
		case http.StatusTooManyRequests:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"back off"},"retry_after_ms":1}`))
		default:
			w.WriteHeader(status)
		}
	})
	mux.HandleFunc("GET /v1/attachments/{message}/{attachment}", func(w http.ResponseWriter, r *http.Request) {
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, err := w.Write(f.attach)
		if err != nil {
			return
		}
	})
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		h := f.health
		f.mu.Unlock()
		writeJSON(w, h)
	})
	return mux
}

func (f *fakeWorker) authed(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+f.token
}

func TestClientPullAck(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000", pullNext: 9, ackStatus: http.StatusNoContent}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)
	fw.pulled = []IngressMessage{{V: 1, Seq: 8, ID: "m8", LocalPart: "ellis-x7k2qm4p"}}
	c := NewClient(srv.URL, fw.token)

	msgs, next, err := c.Pull(t.Context(), 7, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Seq != 8 || next != 9 {
		t.Fatalf("pull = %+v next=%d", msgs, next)
	}
	if want := "after=7&limit=50"; fw.lastPull != want {
		t.Errorf("pull query = %q, want %q", fw.lastPull, want)
	}

	if err := c.Ack(t.Context(), 8, []string{"ellis-x7k2qm4p"}); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Through int64    `json:"through"`
		Known   []string `json:"known"`
	}
	if err := json.Unmarshal(fw.lastAck, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Through != 8 || len(ack.Known) != 1 || ack.Known[0] != "ellis-x7k2qm4p" {
		t.Errorf("ack body = %s", fw.lastAck)
	}
}

func TestClientSendAttachmentHealth(t *testing.T) {
	t.Parallel()
	fw := &fakeWorker{token: "test-token-0000", sentID: "<out-1@x>", attach: []byte("file-bytes")}
	fw.health = Health{Version: "1.0.0", SpoolDepth: 2, KnownCount: 1}
	srv := httptest.NewServer(fw.handler())
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, fw.token)

	id, err := c.Send(t.Context(), SendRequest{V: 1, From: "trebi@x", To: []string{"ops@x"}, MessageID: "<out-1@x>"})
	if err != nil || id != "<out-1@x>" {
		t.Fatalf("send = %q,%v", id, err)
	}
	var sent SendRequest
	if err := json.Unmarshal(fw.lastSend, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MessageID != "<out-1@x>" || sent.From != "trebi@x" || len(sent.To) != 1 {
		t.Errorf("send body = %s", fw.lastSend)
	}

	b, err := c.Attachment(t.Context(), "<in-1@x>", "a1")
	if err != nil || string(b) != "file-bytes" {
		t.Fatalf("attachment = %q,%v", b, err)
	}
	h, err := c.Health(t.Context())
	if err != nil || h.Version != "1.0.0" || h.SpoolDepth != 2 {
		t.Fatalf("health = %+v,%v", h, err)
	}
}

func TestClientRetryAfter(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"back off"},"retry_after_ms":2500}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "test-token-0000")

	_, _, err := c.Pull(t.Context(), 0, 10)
	re, ok := err.(*RetryAfterError)
	if !ok {
		t.Fatalf("pull 429 error = %T(%v)", err, err)
	}
	if re.After != 2500*time.Millisecond {
		t.Errorf("retry after = %s, want 2.5s", re.After)
	}
}

func TestClientErrorEnvelope(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"bad_request","message":"nope"}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "test-token-0000")

	_, err := c.Send(t.Context(), SendRequest{})
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("send error = %T(%v)", err, err)
	}
	if ae.Status != http.StatusBadRequest || ae.Code != "bad_request" || ae.Message != "nope" {
		t.Errorf("api error = %+v", ae)
	}
}

// TestContractFixtures decodes the published fixture files into the wire
// types. A field rename or tag drift fails here first.
func TestContractFixtures(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "contract")

	raw, err := os.ReadFile(filepath.Join(dir, "ingress-message.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msg IngressMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("ingress fixture: %v", err)
	}
	if msg.V != 1 || msg.Seq != 7 || msg.LocalPart != "ellis-x7k2qm4p" || msg.Tag != "research" {
		t.Errorf("ingress fixture decoded = %+v", msg)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Name != "notes.txt" {
		t.Errorf("ingress attachments = %+v", msg.Attachments)
	}
	round, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(round, &b)
	if fmt.Sprint(a["headers"]) != fmt.Sprint(b["headers"]) {
		t.Error("headers must round-trip")
	}

	raw, err = os.ReadFile(filepath.Join(dir, "send-request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var req SendRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("send fixture: %v", err)
	}
	if req.V != 1 || req.From != "ellis-x7k2qm4p@mail.example.com" || req.InReplyTo == "" {
		t.Errorf("send fixture decoded = %+v", req)
	}
}

func readBody(r *http.Request) []byte {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil
	}
	return raw
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
