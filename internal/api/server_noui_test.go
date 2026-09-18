//go:build !ui

package api_test

import (
	"net/http"
	"testing"
)

// TestRootWithoutUI pins the plain-build contract: / is not a route and the
// daemon answers 404. See the ui-tagged companion in server_ui_test.go.
func TestRootWithoutUI(t *testing.T) {
	s, base := startTestServer(t)
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("plain build must 404 at /, got %d", resp.StatusCode)
	}
}
