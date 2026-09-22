package cloudlink

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

// knownSeed is the fixed Ed25519 seed behind the known-answer fixture.
var knownSeed = []byte("0123456789abcdef0123456789abcdef") //nolint:gochecknoglobals // static test key material, never mutated

// TestSignKnownAnswer pins the canonical payload and the signature
// CONTRACT.md defines. A change to the signing input or the encoding fails
// here before any worker does.
func TestSignKnownAnswer(t *testing.T) {
	t.Parallel()
	id := Identity{key: ed25519.NewKeyFromSeed(knownSeed)}

	const (
		timestamp = "1758518400000"
		nonce     = "01HZY7ZQ5W8K3N6P9R2T4V6X8Y"
	)
	wantPayload := "1758518400000\n01HZY7ZQ5W8K3N6P9R2T4V6X8Y\nGET\n/v1/send"
	if got := signPayload(timestamp, nonce, "GET", "/v1/send"); got != wantPayload {
		t.Fatalf("signPayload = %q, want %q", got, wantPayload)
	}
	// The method is uppercase in the payload whatever the caller wrote.
	if got := signPayload(timestamp, nonce, "get", "/v1/send"); got != wantPayload {
		t.Fatalf("lowercase method signPayload = %q, want %q", got, wantPayload)
	}

	const (
		wantSig = "djtQ14wFluCToMlM++vbC7TwcTivCbMJ2y9FCf/8n1WawMuZfNySrqTkPIVFW0ugdFO7u6yJ3K09Koid9Fv9Bw=="
		wantPub = "I7xUkSwebpLEqGglyGfif/3FVb/71CRPF6Jqv//ull0="
	)
	if got := id.Sign([]byte(wantPayload)); got != wantSig {
		t.Fatalf("Sign = %q, want %q", got, wantSig)
	}
	if got := id.PublicKeyB64(); got != wantPub {
		t.Fatalf("PublicKeyB64 = %q, want %q", got, wantPub)
	}
}

// TestSignRequestSetsFourHeaders proves SignRequest wires the headers the
// worker verifies: the signature must check out against the canonical
// payload rebuilt from the headers alone, with no query and no host.
func TestSignRequestSetsFourHeaders(t *testing.T) {
	t.Parallel()
	id := Identity{deviceID: "dev-1", key: ed25519.NewKeyFromSeed(knownSeed)}
	now := time.UnixMilli(1758518400000).UTC()

	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/send?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	SignRequest(req, id, now)

	if got := req.Header.Get(headerDevice); got != "dev-1" {
		t.Errorf("%s = %q", headerDevice, got)
	}
	if got := req.Header.Get(headerTimestamp); got != "1758518400000" {
		t.Errorf("%s = %q, want unix milliseconds", headerTimestamp, got)
	}
	nonce := req.Header.Get(headerNonce)
	if nonce == "" || strings.ContainsAny(nonce, "\r\n\x00") {
		t.Fatalf("%s = %q: must be non-empty with no CR, LF, or NUL", headerNonce, nonce)
	}
	sig := req.Header.Get(headerSignature)
	if sig == "" {
		t.Fatalf("%s is empty", headerSignature)
	}

	payload := signPayload(req.Header.Get(headerTimestamp), nonce, req.Method, req.URL.Path)
	wantPayload := "1758518400000\n" + nonce + "\nPOST\n/v1/send"
	if payload != wantPayload {
		t.Fatalf("payload = %q, want %q (no query, no host)", payload, wantPayload)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	pub := id.key.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, []byte(payload), raw) {
		t.Fatal("signature does not verify against the canonical payload")
	}
}
