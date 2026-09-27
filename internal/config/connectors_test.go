package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConnectorsRegistriesDefault proves a missing file yields the official
// registry only, so the catalog has a source out of the box.
func TestConnectorsRegistriesDefault(t *testing.T) {
	withTempBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Connectors.Registries) != 1 {
		t.Fatalf("registries = %+v", cfg.Connectors.Registries)
	}
	r := cfg.Connectors.Registries[0]
	if r.Name != "official" || r.URL != "https://registry.modelcontextprotocol.io" {
		t.Fatalf("registry = %+v", r)
	}
}

// TestConnectorsRegistriesOverride proves an explicit list replaces the
// default and is validated.
func TestConnectorsRegistriesOverride(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
connectors:
  registries:
    - { name: official, url: https://registry.modelcontextprotocol.io }
    - { name: internal, url: https://mcp.internal.example }
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Connectors.Registries) != 2 || cfg.Connectors.Registries[1].Name != "internal" {
		t.Fatalf("registries = %+v", cfg.Connectors.Registries)
	}
}

// TestConnectorsRegistriesDuplicate proves a repeated name fails the load.
func TestConnectorsRegistriesDuplicate(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
connectors:
  registries:
    - { name: official, url: https://a.example }
    - { name: official, url: https://b.example }
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected duplicate registry name error")
	}
}

// TestConnectorsRegistriesMissingURL proves a registry without a URL fails.
func TestConnectorsRegistriesMissingURL(t *testing.T) {
	dir := withTempBase(t)
	yaml := `
connectors:
  registries:
    - { name: official }
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected missing url error")
	}
}
