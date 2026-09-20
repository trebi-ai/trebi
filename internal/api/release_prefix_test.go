package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Asset names are ownership. This repository owns trebi-free_* and
// SHA256SUMS-free. trebi-pro owns trebi_* and SHA256SUMS on the same release.
func TestReleaseAssetPrefix(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", ".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, "trebi-free_") && !strings.Contains(src, "trebi-free-") {
		t.Fatal("release.yml must name OSS assets trebi-free_*")
	}
	if !strings.Contains(src, "SHA256SUMS-free") {
		t.Fatal("release.yml must write SHA256SUMS-free")
	}
	if !strings.Contains(src, "--latest=false") {
		t.Fatal("release.yml must create the release with --latest=false")
	}
}
