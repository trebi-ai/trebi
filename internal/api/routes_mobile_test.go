package api_test

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/mobile"
	"github.com/trebi-ai/trebi/internal/store"
)

type phoneCloud struct {
	target  mobile.Target
	revoked []string
}

func (c *phoneCloud) PairTarget() mobile.Target { return c.target }

func (c *phoneCloud) MintClaim(_ context.Context, _ int, ttl time.Duration) (string, time.Time, error) {
	return "trcl_test", time.Now().Add(ttl), nil
}

func (c *phoneCloud) RevokeClient(_ context.Context, id string) error {
	c.revoked = append(c.revoked, id)
	return nil
}

// TestMobileRoutes walks the B6 routes: pair, cancel, list, update, and the
// confirm-gated remove, with their errors and bus events.
func TestMobileRoutes(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr

	code, body, _ := doJSON(t, "GET", base+"/v1/mobile/clients", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("no service: %d %v", code, body)
	}

	cloud := &phoneCloud{target: mobile.Target{APIHost: "api.trebi.ai", DeviceID: "dev-1", Machine: "Mac"}}
	svc := mobile.New(st, cloud)
	s.BindMobile(svc)

	for _, tc := range []struct {
		target mobile.Target
		code   string
	}{
		{mobile.Target{}, "not_linked"},
		{mobile.Target{Linked: true}, "remote_access_required"},
		{mobile.Target{Linked: true, RemoteAccess: true}, "mobile_off"},
	} {
		cloud.target = tc.target
		code, body, _ = doJSON(t, "POST", base+"/v1/mobile/pairings", map[string]any{})
		if code != http.StatusConflict || errCode(body) != tc.code {
			t.Fatalf("want 409 %s: %d %v", tc.code, code, body)
		}
	}
	cloud.target = mobile.Target{APIHost: "api.trebi.ai", DeviceID: "dev-1", Machine: "Mac", Linked: true, RemoteAccess: true, MobileOn: true}

	for _, in := range []map[string]any{{"ttl": "soon"}, {"ttl": "1s"}, {"uses": 11}} {
		code, body, _ = doJSON(t, "POST", base+"/v1/mobile/pairings", in)
		if code != http.StatusBadRequest || errCode(body) != "validation" {
			t.Fatalf("bad input %v: %d %v", in, code, body)
		}
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/mobile/pairings", map[string]any{"name": "iPad", "ttl": "15m", "uses": 2})
	if code != http.StatusCreated {
		t.Fatalf("pair %d %v", code, body)
	}
	offerID, _ := body["id"].(string)
	link, _ := body["link"].(string)
	if offerID == "" || !strings.HasPrefix(link, mobile.PairBase+"#v=1&") || body["uses"] != float64(2) {
		t.Fatalf("offer shape: %v", body)
	}

	code, _, _ = doJSON(t, "DELETE", base+"/v1/mobile/pairings/"+offerID, nil)
	if code != http.StatusNoContent {
		t.Fatalf("cancel %d", code)
	}
	code, body, _ = doJSON(t, "DELETE", base+"/v1/mobile/pairings/"+offerID, nil)
	if code != http.StatusNotFound || errCode(body) != "not_found" {
		t.Fatalf("cancel again %d %v", code, body)
	}

	o, err := svc.Pair(t.Context(), mobile.PairInput{})
	if err != nil {
		t.Fatal(err)
	}
	psk, _ := svc.OfferPSK(o.ID)
	phone, err := svc.Redeem(t.Context(), o.ID, mobile.Enrollment{CloudClientID: "cl_1", Name: "Pixel", Platform: mobile.PlatformAndroid, PublicKey: bytes.Repeat([]byte{7}, 32)}, psk)
	if err != nil {
		t.Fatal(err)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/mobile/clients", nil)
	items, _ := body["items"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list %d %v", code, body)
	}
	row, _ := items[0].(map[string]any)
	if row["id"] != phone.ID || row["scope"] != "full" || row["platform"] != "android" {
		t.Fatalf("row: %v", row)
	}
	for _, k := range []string{"psk", "public_key", "cloud_client_id"} {
		if _, ok := row[k]; ok {
			t.Fatalf("list serves %s: %v", k, row)
		}
	}

	code, body, _ = doJSON(t, "PATCH", base+"/v1/mobile/clients/"+phone.ID, map[string]any{"name": "Work phone", "scope": "read"})
	if code != http.StatusOK || body["name"] != "Work phone" || body["scope"] != "read" {
		t.Fatalf("update %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/mobile/clients/"+phone.ID, map[string]any{"scope": "admin"})
	if code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("bad scope %d %v", code, body)
	}
	code, body, _ = doJSON(t, "PATCH", base+"/v1/mobile/clients/nope", map[string]any{"name": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("update unknown %d %v", code, body)
	}

	code, body, _ = doJSON(t, "DELETE", base+"/v1/mobile/clients/"+phone.ID, nil)
	if code != http.StatusBadRequest || errCode(body) != "validation" {
		t.Fatalf("remove without confirm %d %v", code, body)
	}
	code, _, _ = doJSON(t, "DELETE", base+"/v1/mobile/clients/"+phone.ID+"?confirm=1", nil)
	if code != http.StatusNoContent {
		t.Fatalf("remove %d", code)
	}
	if len(cloud.revoked) != 1 || cloud.revoked[0] != "cl_1" {
		t.Fatalf("cloud revoke: %v", cloud.revoked)
	}

	for topic, want := range map[string]int{"mobile.paired": 1, "mobile.updated": 1, "mobile.removed": 1} {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = ?`, topic).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s: %d events, want %d", topic, n, want)
		}
	}
}
