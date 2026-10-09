package cloudlink

import (
	"context"
	"errors"
	"testing"

	"github.com/trebi-ai/trebi/internal/email"
)

// TestLinkAndUnlink walks the link state machine: the row, the key, the
// bus frames, the cached status, and the offline unlink.
func TestLinkAndUnlink(t *testing.T) {
	testHome(t)
	st := openStore(t)
	fc := newFakeCloud(t, nil)

	changes := &changeLog{}
	svc := New(st)
	svc.Configure(Settings{APIURL: fc.srv.URL, Enabled: false})
	svc.WithOnChange(changes.fn(svc))
	ctx := context.Background()

	status, err := svc.Link(ctx, "ABCD-EFGH", "mac-mini")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Linked || status.Status != statusLinked || status.DeviceName != "mac-mini" {
		t.Fatalf("status after link = %+v", status)
	}
	if status.AccountEmail != "" || status.Plan != "" || len(status.Features) != 0 {
		t.Fatalf("fresh link has cloud facts: %+v", status)
	}
	if status.RefreshedAt == "" {
		t.Fatalf("fresh link has no refreshed_at: %+v", status)
	}

	var deviceID, plan, features, linkedAt string
	err = st.DB().QueryRow(`SELECT device_id, plan, features, linked_at FROM cloud_link WHERE id = 1`).
		Scan(&deviceID, &plan, &features, &linkedAt)
	if err != nil {
		t.Fatalf("cloud_link row: %v", err)
	}
	if deviceID != testDeviceID || plan != "" || features != "[]" || linkedAt == "" {
		t.Fatalf("cloud_link row = (%q, %q, %q, %q)", deviceID, plan, features, linkedAt)
	}

	if _, err := svc.Link(ctx, "ABCD-EFGH", "mac-mini"); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("second Link error = %v, want ErrAlreadyLinked", err)
	}

	if n := busCount(t, st, "cloud.linked"); n != 1 {
		t.Errorf("cloud.linked events = %d, want 1", n)
	}

	if err := svc.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	if got := svc.Status(); got.Linked || got.Status != statusNotLinked {
		t.Fatalf("status after unlink = %+v", got)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM cloud_link`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cloud_link rows after unlink = %d (%v), want 0", n, err)
	}
	if _, err := LoadIdentity(testDeviceID); err == nil {
		t.Error("device key survived Unlink")
	}
	if n := busCount(t, st, "cloud.unlinked"); n != 1 {
		t.Errorf("cloud.unlinked events = %d, want 1", n)
	}
	if got := changes.snapshot(); len(got) != 2 {
		t.Errorf("onChange fires = %v, want link and unlink", got)
	}
	// Unlink must work with the cloud gone: no call was made above, and
	// a repeated Unlink is harmless.
	if err := svc.Unlink(ctx); err != nil {
		t.Fatalf("second Unlink: %v", err)
	}
}

// TestStatusMapping pins the status string for every link/socket state.
func TestStatusMapping(t *testing.T) {
	testHome(t)
	st := openStore(t)
	fc := newFakeCloud(t, nil)

	svc := New(st)
	svc.Configure(Settings{APIURL: fc.srv.URL, Enabled: false})
	ctx := context.Background()

	if got := svc.Status(); got.Linked || got.Status != statusNotLinked {
		t.Fatalf("no row = %+v", got)
	}
	if _, err := svc.Link(ctx, "ABCD-EFGH", "mac-mini"); err != nil {
		t.Fatal(err)
	}
	if got := svc.Status(); got.Status != statusLinked {
		t.Fatalf("linked, disabled = %+v", got)
	}
	// Enabled but not started keeps the device merely linked.
	svc.Configure(Settings{APIURL: fc.srv.URL, Enabled: true})
	if got := svc.Status(); got.Status != statusLinked {
		t.Fatalf("enabled, not started = %+v", got)
	}
	// Running but not connected is degraded.
	sk := &socket{svc: svc}
	svc.sock = sk
	if got := svc.Status(); got.Status != statusDegraded || got.Connected {
		t.Fatalf("running, disconnected = %+v", got)
	}
	sk.connected = true
	if got := svc.Status(); got.Status != statusConnected || !got.Connected {
		t.Fatalf("connected = %+v", got)
	}
	sk.connected = false
	sk.unauthorized = true
	if got := svc.Status(); got.Status != statusUnauthorized {
		t.Fatalf("revoked = %+v", got)
	}
	svc.sock = nil
}

// TestCallsRequireLink: every signed call needs the device identity.
func TestCallsRequireLink(t *testing.T) {
	testHome(t)
	st := openStore(t)
	svc := New(st)
	svc.Configure(Settings{APIURL: "https://api.example.com", Enabled: false})
	ctx := context.Background()

	if _, err := svc.MintAddress(ctx, "ws-1", "acme"); !errors.Is(err, ErrNotLinked) {
		t.Errorf("MintAddress error = %v, want ErrNotLinked", err)
	}
	if _, err := svc.MintHook(ctx, MintHookRequest{Label: "deploy"}); !errors.Is(err, ErrNotLinked) {
		t.Errorf("MintHook error = %v, want ErrNotLinked", err)
	}
	if err := svc.DeleteHook(ctx, "hook-1"); !errors.Is(err, ErrNotLinked) {
		t.Errorf("DeleteHook error = %v, want ErrNotLinked", err)
	}
	if _, _, err := svc.Attachment(ctx, "msg/att"); !errors.Is(err, ErrNotLinked) {
		t.Errorf("Attachment error = %v, want ErrNotLinked", err)
	}
	if _, err := svc.Send(ctx, email.SendRequest{V: 1}); !errors.Is(err, ErrNotLinked) {
		t.Errorf("Send error = %v, want ErrNotLinked", err)
	}
	// The cache says nothing is granted.
	if got := svc.Entitlement(); got.Linked || got.Has("hosted_ingress") {
		t.Errorf("entitlement = %+v", got)
	}
}
