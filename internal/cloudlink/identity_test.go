package cloudlink

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

// TestIdentityMintLoadDelete proves the key file format: standard base64
// of the 32-byte seed on one line, no device id anywhere in it, and one
// keypair shared by every device name.
func TestIdentityMintLoadDelete(t *testing.T) {
	testHome(t)

	minted, err := MintIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if minted.DeviceID() != "" {
		t.Fatalf("fresh identity has device id %q", minted.DeviceID())
	}

	raw, err := os.ReadFile(filepath.Join(home.SecretsDir(), deviceKeySecret))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.ContainsAny(text, "\n\r") {
		t.Fatalf("seed file must be one line: %q", text)
	}
	seed, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(seed) != 32 {
		t.Fatalf("seed file is not standard base64 of 32 raw bytes: %q", text)
	}

	loaded, err := LoadIdentity("dev-9")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DeviceID() != "dev-9" {
		t.Errorf("DeviceID = %q", loaded.DeviceID())
	}
	if loaded.PublicKeyB64() != minted.PublicKeyB64() {
		t.Error("loaded keypair differs from the minted one")
	}
	if loaded.Sign([]byte("payload")) != minted.Sign([]byte("payload")) {
		t.Error("signatures differ across mint and load")
	}

	named := minted.ForDevice("dev-2")
	if named.DeviceID() != "dev-2" || named.PublicKeyB64() != minted.PublicKeyB64() {
		t.Error("ForDevice renamed or replaced the keypair")
	}

	if err := DeleteIdentity(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity("dev-9"); err == nil {
		t.Fatal("LoadIdentity succeeded after DeleteIdentity")
	}
}
