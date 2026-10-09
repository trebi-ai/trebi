package cloudlink

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContractFixtures decodes the published fixture files into the wire
// types. A field rename or tag drift fails here first.
func TestContractFixtures(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "contract")

	var hello Hello
	mustDecode(t, dir, "hello.v1.json", &hello)
	if hello.V != ContractV || hello.Type != "hello" || len(hello.Caps) != 2 || hello.Caps[0] != CapMCP || hello.Caps[1] != CapMobile {
		t.Errorf("hello = %+v", hello)
	}

	var beat Heartbeat
	mustDecode(t, dir, "heartbeat.v1.json", &beat)
	if beat.V != ContractV || beat.Type != "heartbeat" {
		t.Errorf("heartbeat = %+v", beat)
	}

	var alert Alert
	mustDecode(t, dir, "alert.v1.json", &alert)
	if alert.V != ContractV || alert.Cursor != "7" || alert.Kind != "approval" || alert.MessageID == "" || alert.Summary == "" {
		t.Errorf("alert = %+v", alert)
	}

	var ack Ack
	mustDecode(t, dir, "ack.v1.json", &ack)
	if ack.V != ContractV || ack.Type != "ack" || ack.ID == "" {
		t.Errorf("ack = %+v", ack)
	}

	var relay RelayFrame
	mustDecode(t, dir, "relay-frame.v1.json", &relay)
	if relay.V != ContractV || relay.Type != "relay_frame" || relay.SID == "" || relay.Bytes == "" {
		t.Errorf("relay = %+v", relay)
	}

	var open RelayOpen
	mustDecodeStrict(t, dir, "relay-open.v1.json", &open)
	if open.V != ContractV || open.Type != "relay_open" || open.SID != relay.SID || open.ClientID == "" {
		t.Errorf("relay_open = %+v", open)
	}
	var closed RelayClose
	mustDecodeStrict(t, dir, "relay-close.v1.json", &closed)
	if closed.Type != "relay_close" || closed.SID != relay.SID || closed.Reason != "client_gone" {
		t.Errorf("relay_close = %+v", closed)
	}
	var revoked ClientRevoked
	mustDecodeStrict(t, dir, "client-revoked.v1.json", &revoked)
	if revoked.Type != "client_revoked" || revoked.ClientID != open.ClientID {
		t.Errorf("client_revoked = %+v", revoked)
	}
	var reqAlert Alert
	mustDecodeStrict(t, dir, "alert-request.v1.json", &reqAlert)
	if reqAlert.Kind != "request" || reqAlert.MessageID == "" {
		t.Errorf("alert request = %+v", reqAlert)
	}
	var claimReq ClaimMintRequest
	mustDecodeStrict(t, dir, "claim-mint-request.v1.json", &claimReq)
	if claimReq.Uses != 1 || claimReq.TTLSeconds != 600 {
		t.Errorf("claim mint request = %+v", claimReq)
	}
	var claim Claim
	mustDecodeStrict(t, dir, "claim-mint-response.v1.json", &claim)
	if claim.Claim == "" || claim.ExpiresAt == "" {
		t.Errorf("claim = %+v", claim)
	}
	var phone map[string]json.RawMessage
	mustDecode(t, dir, "phone-api.v1.json", &phone)
	for _, k := range []string{"enroll_request", "enroll_response", "push_token_request", "error_response"} {
		if len(phone[k]) == 0 {
			t.Errorf("phone-api.v1.json missing %s", k)
		}
	}

	var ok HelloOK
	mustDecode(t, dir, "hello-ok.v1.json", &ok)
	if ok.V != ContractV || ok.Plan != "free" || ok.HeartbeatInterval != 60 || len(ok.Features) != 1 || ok.Features[0] != "watchdog" || ok.McpURL == "" {
		t.Errorf("hello_ok = %+v", ok)
	}

	var in Ingress
	mustDecode(t, dir, "ingress.v1.json", &in)
	if in.V != ContractV || in.Type != "ingress" || in.Kind != "email" || in.ID == "" {
		t.Errorf("ingress = %+v", in)
	}
	var payload map[string]any
	if err := json.Unmarshal(in.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["v"] != float64(1) || payload["in_reply_to"] == "" {
		t.Errorf("ingress payload = %+v", payload)
	}

	var ent Entitlement
	mustDecode(t, dir, "entitlement.v1.json", &ent)
	if ent.V != ContractV || ent.Plan != "pro_cloud" || len(ent.Features) != 5 {
		t.Errorf("entitlement = %+v", ent)
	}

	var hook WebhookIngress
	mustDecodeStrict(t, dir, "webhook-ingress.v1.json", &hook)
	if hook.V != ContractV || hook.HookID == "" || hook.Method != "POST" || hook.Path != "/stripe" || hook.Query != "source=test" || hook.Handshake || hook.Headers["stripe-signature"] == "" || hook.Body == "" {
		t.Errorf("webhook = %+v", hook)
	}

	var mint MintHookRequest
	mustDecodeStrict(t, dir, "hook-mint-request.v1.json", &mint)
	if mint.Label == "" || len(mint.Headers) != 3 || len(mint.Handshake) != 1 {
		t.Errorf("hook mint request = %+v", mint)
	}
	var minted Hook
	mustDecodeStrict(t, dir, "hook-mint-response.v1.json", &minted)
	if minted.ID == "" || minted.Token == "" || minted.URL == "" {
		t.Errorf("hook mint response = %+v", minted)
	}

	var mreq McpRequest
	mustDecode(t, dir, "mcp-request.v1.json", &mreq)
	if mreq.V != ContractV || mreq.Type != "mcp_request" || mreq.ID == "" || mreq.Client == "" || len(mreq.Scope) != 2 || mreq.Headers["content-type"] == "" || mreq.Body == "" {
		t.Errorf("mcp_request = %+v", mreq)
	}

	var mres McpResponse
	mustDecode(t, dir, "mcp-response.v1.json", &mres)
	if mres.V != ContractV || mres.Type != "mcp_response" || mres.ID != mreq.ID || mres.Status != 200 || mres.Body == "" {
		t.Errorf("mcp_response = %+v", mres)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "CONTRACT.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, needle := range []string{"X-Trebi-Device", "X-Trebi-Signature", "webhook-ingress.v1.json", "hook-handshake.v1.json", "hook-mint-request.v1.json", "ingress-message.v1.json", "mcp-request.v1.json", "mcp-response.v1.json", "relay-open.v1.json", "relay-close.v1.json", "client-revoked.v1.json", "claim-mint-request.v1.json", "phone-api.v1.json", "4409", "4410"} {
		if !strings.Contains(text, needle) {
			t.Errorf("CONTRACT.md missing %s", needle)
		}
	}
}

// TestDecodeFrameRoundTrip runs every contract frame through the read
// loop's decode step: encode a frame, decode it, and require the same
// values back. Kind webhook rides as the ingress payload. A field, tag,
// or type-drift on either side of the wire fails here.
func TestDecodeFrameRoundTrip(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "contract")

	var hook WebhookIngress
	mustDecode(t, dir, "webhook-ingress.v1.json", &hook)
	hookPayload, err := json.Marshal(hook)
	if err != nil {
		t.Fatal(err)
	}

	var emailIngress Ingress
	mustDecode(t, dir, "ingress.v1.json", &emailIngress)
	var hookIngress Ingress
	mustDecode(t, dir, "ingress.v1.json", &hookIngress)
	hookIngress.ID = "01J8Y7ZQ5W8K3N6P9R2T4V6X8Y0"
	hookIngress.Kind = "webhook"
	hookIngress.Payload = hookPayload

	var hello Hello
	mustDecode(t, dir, "hello.v1.json", &hello)
	var beat Heartbeat
	mustDecode(t, dir, "heartbeat.v1.json", &beat)
	var alert Alert
	mustDecode(t, dir, "alert.v1.json", &alert)
	var ack Ack
	mustDecode(t, dir, "ack.v1.json", &ack)
	var relay RelayFrame
	mustDecode(t, dir, "relay-frame.v1.json", &relay)
	var ok HelloOK
	mustDecode(t, dir, "hello-ok.v1.json", &ok)
	var ent Entitlement
	mustDecode(t, dir, "entitlement.v1.json", &ent)
	var mreq McpRequest
	mustDecode(t, dir, "mcp-request.v1.json", &mreq)
	var mres McpResponse
	mustDecode(t, dir, "mcp-response.v1.json", &mres)
	var open RelayOpen
	mustDecode(t, dir, "relay-open.v1.json", &open)
	var closed RelayClose
	mustDecode(t, dir, "relay-close.v1.json", &closed)
	var revoked ClientRevoked
	mustDecode(t, dir, "client-revoked.v1.json", &revoked)
	var reqAlert Alert
	mustDecode(t, dir, "alert-request.v1.json", &reqAlert)

	for _, tc := range []struct {
		name string
		typ  string
		want any
	}{
		{"hello", "hello", hello},
		{"heartbeat", "heartbeat", beat},
		{"alert", "alert", alert},
		{"ack", "ack", ack},
		{"relay_frame", "relay_frame", relay},
		{"relay_open", "relay_open", open},
		{"relay_close", "relay_close", closed},
		{"client_revoked", "client_revoked", revoked},
		{"alert/request", "alert", reqAlert},
		{"hello_ok", "hello_ok", ok},
		{"entitlement", "entitlement", ent},
		{"mcp_request", "mcp_request", mreq},
		{"mcp_response", "mcp_response", mres},
		{"ingress/email", "ingress", emailIngress},
		{"ingress/webhook", "ingress", hookIngress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			got, typ, err := decodeFrame(raw)
			if err != nil {
				t.Fatalf("decodeFrame: %v", err)
			}
			if typ != tc.typ {
				t.Fatalf("type = %q, want %q", typ, tc.typ)
			}
			wantJSON, _ := json.Marshal(tc.want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("round trip mismatch:\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}

	// An unknown type decodes to nothing and is named for the counter.
	got, typ, err := decodeFrame([]byte(`{"v":1,"type":"mystery"}`))
	if err != nil || got != nil || typ != "mystery" {
		t.Fatalf("unknown frame = (%v, %q, %v), want (nil, mystery, nil)", got, typ, err)
	}
	if _, _, err := decodeFrame([]byte(`not json`)); err == nil {
		t.Fatal("malformed frame decoded without error")
	}
}

func mustDecode(t *testing.T, dir, name string, dest any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func mustDecodeStrict(t *testing.T, dir, name string, dest any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}
