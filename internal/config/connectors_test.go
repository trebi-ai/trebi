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

// TestConnectorsRegistriesURLScheme proves a registry URL must be http or
// https with a host.
func TestConnectorsRegistriesURLScheme(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "registry.example", "https://", "ftp://r.example"} {
		dir := withTempBase(t)
		yaml := "connectors:\n  registries:\n    - { name: r, url: \"" + u + "\" }\n"
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("%s: expected url error", u)
		}
	}
}

// TestParseRegistries proves the list form splits on ";" when present, so a
// URL may hold a comma, and that the rendered row reads back.
func TestParseRegistries(t *testing.T) {
	regs, err := parseRegistries("a=https://a.example/x?k=1,2; b=https://b.example")
	if err != nil {
		t.Fatal(err)
	}
	want := []Registry{{Name: "a", URL: "https://a.example/x?k=1,2"}, {Name: "b", URL: "https://b.example"}}
	if len(regs) != 2 || regs[0] != want[0] || regs[1] != want[1] {
		t.Fatalf("regs = %+v", regs)
	}
	regs, err = parseRegistries("a=https://a.example,b=https://b.example")
	if err != nil || len(regs) != 2 || regs[1].Name != "b" {
		t.Fatalf("comma form: %+v %v", regs, err)
	}
	back, err := parseRegistries(renderRegistries(want))
	if err != nil || len(back) != 2 || back[0] != want[0] || back[1] != want[1] {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}
