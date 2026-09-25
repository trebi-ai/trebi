package api_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnvsetsEndpoint covers plan H.3.7: sets sorted by name, key names
// only (never values), keys sorted, and an empty list for a missing file.
func TestEnvsetsEndpoint(t *testing.T) {
	_, base, dir := startAPI(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/envsets", nil)
	if code != 200 {
		t.Fatalf("missing file: %d %v", code, body)
	}
	empty, _ := body["envsets"].([]any)
	if len(empty) != 0 {
		t.Fatalf("missing file envsets = %v", body)
	}

	text := "beta:\n  B_KEY: secret-beta\n  A_KEY: secret-beta\nalpha:\n  Z_KEY: secret-alpha\n"
	if err := os.WriteFile(filepath.Join(dir, "envsets.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/envsets", nil)
	if code != 200 {
		t.Fatalf("get %d %v", code, body)
	}
	list, _ := body["envsets"].([]any)
	if len(list) != 2 {
		t.Fatalf("envsets = %v", body)
	}
	first, _ := list[0].(map[string]any)
	if first["name"] != "alpha" {
		t.Fatalf("first = %v, want alpha", first)
	}
	if keys, _ := first["keys"].([]any); len(keys) != 1 || keys[0] != "Z_KEY" {
		t.Fatalf("alpha keys = %v", keys)
	}
	second, _ := list[1].(map[string]any)
	if second["name"] != "beta" {
		t.Fatalf("second = %v, want beta", second)
	}
	if keys, _ := second["keys"].([]any); len(keys) != 2 || keys[0] != "A_KEY" || keys[1] != "B_KEY" {
		t.Fatalf("beta keys = %v", keys)
	}
	// Values can hold secrets: only name and keys may appear.
	for _, row := range list {
		entry, _ := row.(map[string]any)
		if len(entry) != 2 || entry["values"] != nil || entry["value"] != nil {
			t.Fatalf("entry leaks fields: %v", entry)
		}
	}
}
