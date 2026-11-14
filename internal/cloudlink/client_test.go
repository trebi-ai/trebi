package cloudlink

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/email"
)

type callRecord struct {
	method string
	path   string
	body   string
	signed bool
}

// TestClientWirePaths pins the exact wire contract: every path and body
// shape from CONTRACT.md, Register unsigned, everything else signed with
// the four headers and no private key field anywhere.
func TestClientWirePaths(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		calls []callRecord
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, callRecord{
			method: r.Method,
			path:   r.URL.Path,
			body:   string(raw),
			signed: r.Header.Get(headerDevice) != "" && r.Header.Get(headerSignature) != "",
		})
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices":
			_, _ = w.Write([]byte(`{"device_id":"dev-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/addresses":
			var body map[string]string
			if err := json.Unmarshal(raw, &body); err != nil || body["handle"] != "acme" || body["workspace_id"] != "ws-1" {
				http.Error(w, "bad mint body", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"id":"addr-1","local_part":"acme-x7k2qm","address":"acme-x7k2qm@mail.example.com"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/addresses":
			_, _ = w.Write([]byte(`{"addresses":[{"id":"addr-1","local_part":"a-1","created_at":"2026-09-22T10:00:00Z"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/addresses/addr-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/hooks":
			_, _ = w.Write([]byte(`{"id":"hook-1","token":"tok","url":"https://in.example.com/hook/tok"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/hooks":
			_, _ = w.Write([]byte(`{"hooks":[{"id":"hook-1"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/hooks/hook-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/send":
			_, _ = w.Write([]byte(`{"message_id":"msg-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/attachments/msg-1/att-1":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello bytes"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			_, _ = w.Write([]byte(`{"limit":5,"machines":[{"device_id":"dev-2","name":"studio","online":false,"last_seen":null,"role":"owner","capabilities":null,"workspaces":[]}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/workspaces":
			var body WorkspaceIndex
			if err := json.Unmarshal(raw, &body); err != nil || len(body.Workspaces) != 1 || body.Workspaces[0].Handle != "acme" {
				http.Error(w, "bad index body", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/devices/me":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nope":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	id := Identity{deviceID: "dev-1", key: ed25519.NewKeyFromSeed(knownSeed)}
	c := NewClient(newEndpoints([]string{srv.URL}), id)
	ctx := context.Background()

	deviceID, err := c.Register(ctx, "ABCD-EFGH", "mac-mini")
	if err != nil || deviceID != "dev-1" {
		t.Fatalf("Register = (%q, %v)", deviceID, err)
	}
	addr, err := c.MintAddress(ctx, "ws-1", "acme")
	if err != nil || addr.ID != "addr-1" || addr.Address != "acme-x7k2qm@mail.example.com" {
		t.Fatalf("MintAddress = (%+v, %v)", addr, err)
	}
	addrs, err := c.ListAddresses(ctx)
	if err != nil || len(addrs) != 1 || addrs[0].LocalPart != "a-1" {
		t.Fatalf("ListAddresses = (%+v, %v)", addrs, err)
	}
	if err := c.DeleteAddress(ctx, "addr-1"); err != nil {
		t.Fatalf("DeleteAddress: %v", err)
	}
	hook, err := c.MintHook(ctx, MintHookRequest{Label: "deploy"})
	if err != nil || hook.ID != "hook-1" || hook.Token != "tok" || hook.URL == "" {
		t.Fatalf("MintHook = (%+v, %v)", hook, err)
	}
	hooks, err := c.ListHooks(ctx)
	if err != nil || len(hooks) != 1 || hooks[0].ID != "hook-1" {
		t.Fatalf("ListHooks = (%+v, %v)", hooks, err)
	}
	if err := c.DeleteHook(ctx, "hook-1"); err != nil {
		t.Fatalf("DeleteHook: %v", err)
	}
	res, err := c.Send(ctx, email.SendRequest{V: 1, From: "a-1@mail.example.com", To: []string{"you@example.com"}, Subject: "Hi", Text: "Run.", MessageID: "<m1@x>"})
	if err != nil || res.MessageID != "msg-1" {
		t.Fatalf("Send = (%+v, %v)", res, err)
	}
	data, contentType, err := c.Attachment(ctx, "msg-1/att-1")
	if err != nil || string(data) != "hello bytes" || contentType != "text/plain" {
		t.Fatalf("Attachment = (%q, %q, %v)", data, contentType, err)
	}
	roster, err := c.Roster(ctx)
	if err != nil || roster.Limit != 5 || len(roster.Machines) != 1 || roster.Machines[0].DeviceID != "dev-2" || roster.Machines[0].LastSeen != nil {
		t.Fatalf("Roster = (%+v, %v)", roster, err)
	}
	if err := c.PublishWorkspaces(ctx, []WorkspaceIndexItem{{ID: "ws_1", Name: "Acme", Handle: "acme"}}); err != nil {
		t.Fatalf("PublishWorkspaces: %v", err)
	}
	if err := c.Leave(ctx); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	want := []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/devices"},
		{http.MethodPost, "/v1/addresses"},
		{http.MethodGet, "/v1/addresses"},
		{http.MethodDelete, "/v1/addresses/addr-1"},
		{http.MethodPost, "/v1/hooks"},
		{http.MethodGet, "/v1/hooks"},
		{http.MethodDelete, "/v1/hooks/hook-1"},
		{http.MethodPost, "/v1/send"},
		{http.MethodGet, "/v1/attachments/msg-1/att-1"},
		{http.MethodGet, "/v1/machines"},
		{http.MethodPut, "/v1/workspaces"},
		{http.MethodDelete, "/v1/devices/me"},
	}
	if len(calls) != len(want) {
		t.Fatalf("recorded %d calls, want %d: %+v", len(calls), len(want), calls)
	}
	for i, w := range want {
		if calls[i].method != w.method || calls[i].path != w.path {
			t.Errorf("call %d = %s %s, want %s %s", i, calls[i].method, calls[i].path, w.method, w.path)
		}
	}
	// Register is the only unsigned call.
	if calls[0].signed {
		t.Error("Register must be unsigned")
	}
	for _, rec := range calls[1:] {
		if !rec.signed {
			t.Errorf("%s %s is unsigned", rec.method, rec.path)
		}
	}
	// The register body carries enrollment_token, name, public_key and
	// never a private key field (the worker refuses one).
	var register map[string]any
	if err := json.Unmarshal([]byte(calls[0].body), &register); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"enrollment_token", "name", "public_key"} {
		if _, ok := register[key]; !ok {
			t.Errorf("register body missing %q: %s", key, calls[0].body)
		}
	}
	for key := range register {
		if key == "private_key" || key == "privateKey" {
			t.Errorf("register body carries %q", key)
		}
	}
	// MintAddress carries the workspace_id body the worker ignores.
	if !json.Valid([]byte(calls[1].body)) || calls[1].body == "" {
		t.Errorf("MintAddress body = %q", calls[1].body)
	}
}

// TestClientErrorEnvelope proves the 429 and error-envelope mapping.
func TestClientErrorEnvelope(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/limited":
			w.Header().Set("Retry-After-Ms", "2500")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/v1/retry-body":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after_ms":150}`))
		case "/v1/forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)

	id := Identity{deviceID: "dev-1", key: ed25519.NewKeyFromSeed(knownSeed)}
	c := NewClient(newEndpoints([]string{srv.URL}), id)
	ctx := context.Background()

	err := c.call(ctx, http.MethodGet, "/v1/limited", nil, nil, true)
	var retry *RetryAfterError
	if !errors.As(err, &retry) || retry.After != 2500*time.Millisecond {
		t.Fatalf("Retry-After-Ms: %v", err)
	}
	err = c.call(ctx, http.MethodGet, "/v1/retry-body", nil, nil, true)
	retry = nil
	if !errors.As(err, &retry) || retry.After != 150*time.Millisecond {
		t.Fatalf("retry_after_ms body: %v", err)
	}
	err = c.call(ctx, http.MethodGet, "/v1/forbidden", nil, nil, true)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("apiError: %v", err)
	}
}

// TestClientLinkByEmail serves the contract fixtures and checks the wire:
// the three calls are unsigned, the poll carries the request secret, and
// the request body has the fixture keys.
func TestClientLinkByEmail(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "contract")
	fixture := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var created linkCreated
	mustDecodeStrict(t, dir, "link-request-created.v1.json", &created)
	var wantReq map[string]string
	mustDecode(t, dir, "link-request.v1.json", &wantReq)

	var (
		mu     sync.Mutex
		calls  []callRecord
		secret string
		state  = "link-status-pending.v1.json"
		resend = http.StatusNoContent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, callRecord{method: r.Method, path: r.URL.Path, body: string(raw), signed: r.Header.Get(headerSignature) != ""})
		secret = r.Header.Get("x-request-secret")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices/requests":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write(fixture("link-request-created.v1.json"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices/requests/"+created.RequestID:
			_, _ = w.Write(fixture(state))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices/requests/"+created.RequestID+"/resend":
			w.WriteHeader(resend)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	id := Identity{key: ed25519.NewKeyFromSeed(knownSeed)}
	c := NewClient(newEndpoints([]string{srv.URL}), id)
	ctx := context.Background()

	req, err := c.RequestLink(ctx, wantReq["email"], wantReq["name"])
	if err != nil {
		t.Fatal(err)
	}
	if req.ID != created.RequestID || req.Secret != created.PollSecret || req.PollAfter != 3*time.Second || req.ExpiresAt.IsZero() {
		t.Fatalf("RequestLink = %+v", req)
	}
	var sent map[string]string
	if err := json.Unmarshal([]byte(calls[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent) != len(wantReq) || sent["email"] != wantReq["email"] || sent["name"] != wantReq["name"] || sent["public_key"] != id.PublicKeyB64() {
		t.Fatalf("request body = %v", sent)
	}

	for name, want := range map[string]LinkState{
		"link-status-pending.v1.json":  {Status: LinkPending},
		"link-status-approved.v1.json": {Status: LinkApproved, DeviceID: "01j9a2b3c4d5e6f7g8h9j0k1m2"},
		"link-status-expired.v1.json":  {Status: LinkExpired},
		"link-status-limit.v1.json":    {Status: LinkMachineLimit, Message: "This plan allows 1 machine. Remove a machine in your Trebi account, or upgrade to Pro Cloud."},
	} {
		var fx LinkState
		mustDecodeStrict(t, dir, name, &fx)
		if fx != want {
			t.Errorf("%s decodes to %+v, want %+v", name, fx, want)
		}
		mu.Lock()
		state = name
		mu.Unlock()
		got, err := c.LinkStatus(ctx, req.ID, req.Secret)
		if err != nil || got != want {
			t.Errorf("LinkStatus(%s) = (%+v, %v), want %+v", name, got, err, want)
		}
		if secret != req.Secret {
			t.Errorf("poll sent secret %q, want %q", secret, req.Secret)
		}
	}
	if err := c.ResendLink(ctx, req.ID, req.Secret); err != nil || secret != req.Secret {
		t.Fatalf("ResendLink = %v (secret %q)", err, secret)
	}
	for _, rec := range calls {
		if rec.signed {
			t.Errorf("%s %s is signed; the link calls are unsigned", rec.method, rec.path)
		}
	}

	// A wrong id is a 404 APIError, and a 429 is a RetryAfterError.
	var apiErr *APIError
	if _, err := c.LinkStatus(ctx, "nope", req.Secret); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("unknown id: %v", err)
	}
	mu.Lock()
	resend = http.StatusTooManyRequests
	mu.Unlock()
	var retry *RetryAfterError
	if err := c.ResendLink(ctx, req.ID, req.Secret); !errors.As(err, &retry) {
		t.Errorf("429: %v", err)
	}
}

// TestClientRequestLinkErrors maps the 400 envelope and the 429 backoff.
func TestClientRequestLinkErrors(t *testing.T) {
	t.Parallel()
	code := http.StatusBadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After-Ms", "900000")
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"code":"validation","message":"email is not valid"}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(newEndpoints([]string{srv.URL}), Identity{key: ed25519.NewKeyFromSeed(knownSeed)})

	var apiErr *APIError
	if _, err := c.RequestLink(context.Background(), "x", "m"); !errors.As(err, &apiErr) || apiErr.Code != "validation" {
		t.Fatalf("400: %v", err)
	}
	code = http.StatusTooManyRequests
	var retry *RetryAfterError
	if _, err := c.RequestLink(context.Background(), "x", "m"); !errors.As(err, &retry) || retry.After != 15*time.Minute {
		t.Fatalf("429: %v", err)
	}
}
