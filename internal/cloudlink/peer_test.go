package cloudlink

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
)

// TestPeerCalls pins the calls of a machine client: a signed enroll, a
// Bearer leave, and the relay socket with base64 text messages.
func TestPeerCalls(t *testing.T) {
	t.Parallel()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/clients/peers":
			raw, _ := io.ReadAll(r.Body)
			var body PeerEnrollRequest
			if r.Header.Get(headerSignature) == "" || json.Unmarshal(raw, &body) != nil || body.Claim == "" || body.Name != "mbp" {
				http.Error(w, "bad enroll", http.StatusBadRequest)
				return
			}
			if body.Claim == "trcl_used" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"bad_claim","message":"used"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"client_id":"cl_1","token":"trc_1"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/clients/me":
			if r.Header.Get("Authorization") != "Bearer trc_1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/relay":
			if r.Header.Get("Authorization") != "Bearer trc_1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"bad_token","message":"no"}}`))
				return
			}
			conn, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("skip"))
			typ, msg, err := conn.ReadMessage()
			if err != nil || typ != websocket.TextMessage {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, msg) // echo
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(newEndpoints([]string{srv.URL}), Identity{deviceID: "dev-1", key: ed25519.NewKeyFromSeed(knownSeed)})
	ctx := context.Background()

	enr, err := c.EnrollPeer(ctx, "trcl_x", "mbp")
	if err != nil || enr.ClientID != "cl_1" || enr.Token != "trc_1" {
		t.Fatalf("EnrollPeer = (%+v, %v)", enr, err)
	}
	var apiErr *APIError
	if _, err := c.EnrollPeer(ctx, "trcl_used", "mbp"); !errors.As(err, &apiErr) || apiErr.Code != "bad_claim" {
		t.Fatalf("used claim: %v", err)
	}
	if err := c.LeaveClient(ctx, "trc_1"); err != nil {
		t.Fatalf("LeaveClient: %v", err)
	}
	if err := c.LeaveClient(ctx, "trc_bad"); err == nil {
		t.Fatal("LeaveClient with a bad token")
	}

	if _, err := dialRelay(ctx, srv.URL, "trc_bad"); !errors.As(err, &apiErr) || apiErr.Code != "bad_token" {
		t.Fatalf("relay with a bad token: %v", err)
	}
	s, err := dialRelay(ctx, srv.URL, "trc_1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send([]byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recv()
	if err != nil || base64.StdEncoding.EncodeToString(got) != "AAEC" {
		t.Fatalf("Recv = (%v, %v)", got, err)
	}
}
