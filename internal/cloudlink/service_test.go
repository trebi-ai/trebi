package cloudlink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/email"
	"github.com/trebi-ai/trebi/internal/home"
)

// TestLinkAndUnlink walks the link state machine: the row, the key, the
// bus frames, the cached status, and the offline unlink.
func TestLinkAndUnlink(t *testing.T) {
	testHome(t)
	st := openStore(t)
	fc := newFakeCloud(t, nil)

	changes := &changeLog{}
	svc := New(st)
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: false})
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
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: false})
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
	svc.Configure(Settings{APIURLs: []string{fc.srv.URL}, Enabled: true})
	if got := svc.Status(); got.Status != statusLinked {
		t.Fatalf("enabled, not started = %+v", got)
	}
	// Running but not connected is degraded.
	sk := &socket{svc: svc, downSince: time.Now()}
	svc.sock = sk
	if got := svc.Status(); got.Status != statusDegraded || got.Connected || got.DownSince == "" {
		t.Fatalf("running, disconnected = %+v", got)
	}
	sk.markUp(ctx)
	if got := svc.Status(); got.Status != statusConnected || !got.Connected || got.DownSince != "" {
		t.Fatalf("connected = %+v", got)
	}
	sk.markDown(ctx)
	if got := svc.Status(); got.Status != statusDegraded || got.DownSince == "" {
		t.Fatalf("lost = %+v", got)
	}
	sk.connected = true
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
	svc.Configure(Settings{APIURLs: []string{"https://api.example.com"}, Enabled: false})
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
	if _, err := svc.Roster(ctx); !errors.Is(err, ErrNotLinked) {
		t.Errorf("Roster error = %v, want ErrNotLinked", err)
	}
	if err := svc.PublishWorkspaces(ctx, nil); !errors.Is(err, ErrNotLinked) {
		t.Errorf("PublishWorkspaces error = %v, want ErrNotLinked", err)
	}
	// The cache says nothing is granted.
	if got := svc.Entitlement(); got.Linked || got.Has("hosted_ingress") {
		t.Errorf("entitlement = %+v", got)
	}
}

// TestRequestLinkApproved walks pending to approved: the pending row, the
// 0600 file, the bus frames, and the same tail as Link.
func TestRequestLinkApproved(t *testing.T) {
	testHome(t)
	lc := newLinkCloud(t)
	svc := newLinkService(t, lc)
	changes := &changeLog{}
	svc.WithOnChange(changes.fn(svc))
	ctx := context.Background()

	st, err := svc.RequestLink(ctx, linkEmail, "mac-mini")
	if err != nil {
		t.Fatal(err)
	}
	if st.Linked || st.Pending == nil || st.Pending.Email != linkEmail || st.Pending.Status != LinkPending || st.Pending.RequestID == "" || st.Pending.ExpiresAt.IsZero() {
		t.Fatalf("status after request = %+v / %+v", st, st.Pending)
	}
	fi, err := os.Stat(home.CloudLinkPendingPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pending.json = %v, %v", fi, err)
	}
	if n := busCount(t, svc.st, "cloud.link_requested"); n != 1 {
		t.Errorf("cloud.link_requested events = %d, want 1", n)
	}
	if _, err := svc.RequestLink(ctx, "not-an-email", "x"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("bad email: %v", err)
	}

	lc.set("link-status-approved.v1.json")
	waitFor(t, "linked", func() bool { return svc.Status().Linked })
	got := svc.Status()
	if got.Pending != nil || got.AccountEmail != linkEmail || got.DeviceName != "mac-mini" || got.Status != statusLinked {
		t.Fatalf("status after approval = %+v", got)
	}
	if svc.DeviceID() != "01j9a2b3c4d5e6f7g8h9j0k1m2" {
		t.Errorf("device id = %q", svc.DeviceID())
	}
	if _, err := LoadIdentity(svc.DeviceID()); err != nil {
		t.Errorf("device key after link: %v", err)
	}
	if _, err := os.Stat(home.CloudLinkPendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending.json survived the link: %v", err)
	}
	if n := busCount(t, svc.st, "cloud.linked"); n != 1 {
		t.Errorf("cloud.linked events = %d, want 1", n)
	}
	if n := len(changes.snapshot()); n != 2 { // request, then link
		t.Errorf("onChange fires = %d, want 2", n)
	}
	if _, err := svc.RequestLink(ctx, linkEmail, "mac-mini"); !errors.Is(err, ErrAlreadyLinked) {
		t.Errorf("request while linked: %v", err)
	}
	if lc.secret != "8m3k0v2s9d7f1q4w6e5r2t8y0u3i1o9p" {
		t.Errorf("poll secret = %q", lc.secret)
	}
}

// TestRequestLinkEnds covers the two ends the cloud sets: expired clears
// the key, and machine_limit keeps the row with the message.
func TestRequestLinkEnds(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		status  LinkStatus
		message string
	}{
		{"link-status-expired.v1.json", LinkExpired, ""},
		{"link-status-limit.v1.json", LinkMachineLimit, "This plan allows 1 machine. Remove a machine in your Trebi account, or upgrade to Pro Cloud."},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			testHome(t)
			lc := newLinkCloud(t)
			svc := newLinkService(t, lc)
			if _, err := svc.RequestLink(context.Background(), linkEmail, "mac-mini"); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadIdentity(""); err != nil {
				t.Fatalf("key after request: %v", err)
			}
			lc.set(tc.fixture)
			waitFor(t, "ended", func() bool {
				p := svc.Status().Pending
				return p != nil && p.Status == tc.status
			})
			got := svc.Status()
			if got.Linked || got.Pending.Message != tc.message || got.Pending.Email != linkEmail {
				t.Fatalf("status = %+v / %+v", got, got.Pending)
			}
			if _, err := LoadIdentity(""); err == nil {
				t.Error("device key survived the end of the request")
			}
			if p := readPending(); p == nil || p.Status != tc.status {
				t.Errorf("pending.json = %+v", p)
			}
			if n := busCount(t, svc.st, "cloud.link_failed"); n != 1 {
				t.Errorf("cloud.link_failed events = %d, want 1", n)
			}
			// A new request replaces the ended one.
			lc.set("link-status-pending.v1.json")
			if _, err := svc.RequestLink(context.Background(), linkEmail, "mac-mini"); err != nil {
				t.Fatal(err)
			}
			if p := svc.Status().Pending; p == nil || p.Status != LinkPending || p.Message != "" {
				t.Errorf("pending after new request = %+v", p)
			}
		})
	}
}

// TestRequestLinkResume: a restart with pending.json polls again and links.
func TestRequestLinkResume(t *testing.T) {
	testHome(t)
	lc := newLinkCloud(t)
	first := newLinkService(t, lc)
	if _, err := first.RequestLink(context.Background(), linkEmail, "mac-mini"); err != nil {
		t.Fatal(err)
	}
	first.Stop() // the daemon stops

	second := New(first.st)
	second.Configure(Settings{APIURLs: []string{lc.srv.URL}, Enabled: false})
	t.Cleanup(second.Stop)
	if p := second.Status().Pending; p == nil || p.Status != LinkPending || p.Email != linkEmail {
		t.Fatalf("pending after restart = %+v", p)
	}
	second.Start(context.Background())
	lc.set("link-status-approved.v1.json")
	waitFor(t, "linked after restart", func() bool { return second.Status().Linked })
	if second.Status().AccountEmail != linkEmail {
		t.Errorf("status = %+v", second.Status())
	}
}

// TestRequestLinkResumeExpired: a request that ran out while the daemon
// was off ends as expired at start, without a call to the cloud.
func TestRequestLinkResumeExpired(t *testing.T) {
	testHome(t)
	lc := newLinkCloud(t)
	if _, err := MintIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := writePending(pendingRow{
		RequestID: "01j9a2b3c4d5e6f7g8h9j0k1m4", Secret: "s", Email: linkEmail, Name: "m", Status: LinkPending,
		RequestedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-30 * time.Minute), PollAfterMS: 20,
	}); err != nil {
		t.Fatal(err)
	}
	svc := newLinkService(t, lc)
	svc.Start(context.Background())
	if p := svc.Status().Pending; p == nil || p.Status != LinkExpired {
		t.Fatalf("pending = %+v", p)
	}
	if _, err := LoadIdentity(""); err == nil {
		t.Error("device key survived")
	}
}

// TestResendAndCancel: Resend needs an open request, and Cancel drops the
// row, the file, and the key.
func TestResendAndCancel(t *testing.T) {
	testHome(t)
	lc := newLinkCloud(t)
	svc := newLinkService(t, lc)
	ctx := context.Background()

	if _, err := svc.ResendLink(ctx); !errors.Is(err, ErrNoPendingLink) {
		t.Fatalf("resend with nothing pending: %v", err)
	}
	if _, err := svc.RequestLink(ctx, linkEmail, "mac-mini"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResendLink(ctx); err != nil {
		t.Fatal(err)
	}
	if lc.resends != 1 || lc.secret != "8m3k0v2s9d7f1q4w6e5r2t8y0u3i1o9p" {
		t.Errorf("resends = %d, secret = %q", lc.resends, lc.secret)
	}

	st, err := svc.CancelLink(ctx)
	if err != nil || st.Pending != nil {
		t.Fatalf("cancel = %+v, %v", st, err)
	}
	if _, err := os.Stat(home.CloudLinkPendingPath()); !os.IsNotExist(err) {
		t.Errorf("pending.json survived cancel: %v", err)
	}
	if _, err := LoadIdentity(""); err == nil {
		t.Error("device key survived cancel")
	}
	if _, err := svc.ResendLink(ctx); !errors.Is(err, ErrNoPendingLink) {
		t.Errorf("resend after cancel: %v", err)
	}
	if _, err := svc.CancelLink(ctx); err != nil {
		t.Errorf("second cancel: %v", err)
	}
}

// TestRequestLinkCloudError leaves no key and no row.
func TestRequestLinkCloudError(t *testing.T) {
	testHome(t)
	lc := newLinkCloud(t)
	lc.reject = http.StatusBadRequest
	svc := newLinkService(t, lc)
	var apiErr *APIError
	if _, err := svc.RequestLink(context.Background(), linkEmail, "mac-mini"); !errors.As(err, &apiErr) || apiErr.Code != "validation" {
		t.Fatalf("error = %v", err)
	}
	if _, err := LoadIdentity(""); err == nil {
		t.Error("device key survived a failed request")
	}
	if svc.Status().Pending != nil {
		t.Errorf("pending = %+v", svc.Status().Pending)
	}
}

// linkCloud is the hosted side of the link by email: it serves the
// contract fixtures, and a test moves the request through its states.
type linkCloud struct {
	srv *httptest.Server

	mu       sync.Mutex
	state    string // fixture name served by the status route
	requests int
	resends  int
	secret   string // x-request-secret of the last poll or resend
	reject   int    // non-zero: answer the request route with this status
}

const linkEmail = "ellis@example.com"

func newLinkCloud(t *testing.T) *linkCloud {
	t.Helper()
	lc := &linkCloud{state: "link-status-pending.v1.json"}
	fixture := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", "contract", name))
		if err != nil {
			t.Error(err)
		}
		return raw
	}
	lc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lc.mu.Lock()
		defer lc.mu.Unlock()
		if v := r.Header.Get("x-request-secret"); v != "" {
			lc.secret = v
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices/requests":
			if lc.reject != 0 {
				w.WriteHeader(lc.reject)
				_, _ = w.Write([]byte(`{"error":{"code":"validation","message":"email is not valid"}}`))
				return
			}
			lc.requests++
			var out linkCreated
			if err := json.Unmarshal(fixture("link-request-created.v1.json"), &out); err != nil {
				t.Error(err)
			}
			out.PollAfterMS = 20
			out.ExpiresAt = time.Now().Add(30 * time.Minute).UTC()
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices/requests/01j9a2b3c4d5e6f7g8h9j0k1m4":
			_, _ = w.Write(fixture(lc.state))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices/requests/01j9a2b3c4d5e6f7g8h9j0k1m4/resend":
			lc.resends++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(lc.srv.Close)
	return lc
}

func (lc *linkCloud) set(state string) {
	lc.mu.Lock()
	lc.state = state
	lc.mu.Unlock()
}

// newLinkService builds a service on a fresh store and stops it at the end.
func newLinkService(t *testing.T, lc *linkCloud) *Service {
	t.Helper()
	svc := New(openStore(t))
	svc.Configure(Settings{APIURLs: []string{lc.srv.URL}, Enabled: false})
	t.Cleanup(svc.Stop)
	return svc
}
