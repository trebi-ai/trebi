//go:build ui

package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestRootServesUI proves a ui-tagged build serves the embedded fixture
// outside auth while the API keeps requiring the bearer.
func TestRootServesUI(t *testing.T) {
	_, base := startTestServer(t)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "trebi") {
		t.Fatalf("root: status=%d body=%q", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("index cache-control=%q", got)
	}

	api, err := http.Get(base + "/v1/system/health")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Body.Close()
	if api.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api without a token: status=%d", api.StatusCode)
	}
}
