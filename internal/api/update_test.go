package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestProAssetName(t *testing.T) {
	t.Parallel()
	got := proAssetName("v0.2.9", "darwin", "arm64")
	want := "trebi_0.2.9_darwin_arm64.tar.gz"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestVersionNewer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.3.0", "0.2.9", true},
		{"0.2.9", "0.2.9", false},
		{"0.2.8", "0.2.9", false},
		{"v0.3.0", "0.2.9", true},
		{"0.3.0", "dev", false},
		{"0.3.0", "0.2.9-3-gabc", true},
		{"", "0.2.9", false},
	}
	for _, tc := range cases {
		if got := versionNewer(tc.latest, tc.current); got != tc.want {
			t.Errorf("versionNewer(%q, %q)=%v want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestLookupGitHubLatest(t *testing.T) {
	t.Parallel()
	want := proAssetName("v0.3.0", runtime.GOOS, runtime.GOARCH)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Accept=%q", r.Header.Get("Accept"))
		}
		fmt.Fprintf(w, `{"tag_name":"v0.3.0","assets":[{"name":%q,"browser_download_url":"https://example.com/%s"}]}`, want, want)
	}))
	t.Cleanup(ts.Close)
	tag, url, err := lookupGitHubLatest(t.Context(), ts.Client(), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0.3.0" {
		t.Fatalf("tag=%q", tag)
	}
	if url != "https://example.com/"+want {
		t.Fatalf("url=%q", url)
	}
}

func TestLookupGitHubLatestCanceled(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	_, _, err := lookupGitHubLatest(ctx, ts.Client(), ts.URL)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLatestCacheTTL(t *testing.T) {
	t.Parallel()
	var n int
	fetch := func(context.Context) (string, string, error) {
		n++
		return "v0.3.0", "https://example.com/a.tar.gz", nil
	}
	var c latestCache
	if _, _, err := c.get(t.Context(), fetch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.get(t.Context(), fetch); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("lookups=%d want 1", n)
	}
	c.fetched = time.Now().Add(-githubLatestTTL - time.Second)
	if _, _, err := c.get(t.Context(), fetch); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("lookups=%d want 2 after TTL", n)
	}
}

func TestLatestCacheSkipsCanceled(t *testing.T) {
	t.Parallel()
	var n int
	var c latestCache
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := c.get(ctx, func(ctx context.Context) (string, string, error) {
		n++
		return "", "", ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	tag, _, err := c.get(t.Context(), func(context.Context) (string, string, error) {
		n++
		return "v1", "u", nil
	})
	if err != nil || tag != "v1" {
		t.Fatalf("tag=%q err=%v", tag, err)
	}
	if n != 2 {
		t.Fatalf("lookups=%d want 2", n)
	}
}

func TestLatestCacheErrorTTL(t *testing.T) {
	t.Parallel()
	var n int
	boom := errors.New("github down")
	fetch := func(context.Context) (string, string, error) {
		n++
		return "", "", boom
	}
	var c latestCache
	if _, _, err := c.get(t.Context(), fetch); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if _, _, err := c.get(t.Context(), fetch); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Fatalf("lookups=%d want 1", n)
	}
	c.fetched = time.Now().Add(-githubLatestErrTTL - time.Second)
	if _, _, err := c.get(t.Context(), fetch); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Fatalf("lookups=%d want 2 after error TTL", n)
	}
}
