package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyCodexAuthAllowlist(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "auth.json"), []byte(`{"tokens":{"ok":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "config.toml"), []byte("model = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "AGENTS.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", outer)
	dst := t.TempDir()
	if err := copyCodexAuth(dst); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("scratch entries %v", dirNames(entries))
	}
	st, err := os.Stat(filepath.Join(dst, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json mode %s", st.Mode())
	}
}

func TestCopyCodexAuthMissing(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	dst := t.TempDir()
	if err := copyCodexAuth(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("want missing auth.json, err=%v", err)
	}
}

func TestSeedClientHomeCodexPlusConfig(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "auth.json"), []byte(`{"ok":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "config.toml"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer, "AGENTS.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", outer)
	dst := t.TempDir()
	if err := seedClientHome(dst, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexMCPConfig(dst, "http://127.0.0.1:1/mcp", "tok"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if !got["auth.json"] || !got["config.toml"] || got["AGENTS.md"] {
		t.Fatalf("scratch %v", dirNames(entries))
	}
	body, err := os.ReadFile(filepath.Join(dst, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(body)) == "nope" {
		t.Fatal("copied outer config.toml")
	}
}

func TestSeedClaudeOnboarding(t *testing.T) {
	t.Parallel()
	dst := t.TempDir()
	if err := seedClientHome(dst, "claude"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"hasCompletedOnboarding": true`) {
		t.Fatalf("%s", data)
	}
}

func TestWriteCodexMCPConfigCarriesModel(t *testing.T) {
	outer := t.TempDir()
	src := strings.Join([]string{
		`# model = "ignored"`,
		`model_provider = "X"`,
		`model = "glm"`,
		`model_reasoning_effort = "high"`,
		`model_catalog_json = "~/.codex/models.json"`,
		`notify = ["nope"]`,
		`instructions = "nope"`,
		``,
		`[model_providers.X]`,
		`name = "X"`,
		`token = "secret"`,
		``,
		`[projects."/x"]`,
		`trust_level = "trusted"`,
		``,
		`[tui]`,
		`notifications = true`,
		``,
		`[mcp_servers.other]`,
		`command = "other"`,
		``,
	}, "\n")
	if err := os.WriteFile(filepath.Join(outer, "config.toml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", outer)
	dst := t.TempDir()
	if err := writeCodexMCPConfig(dst, "http://127.0.0.1:1/mcp", "tok"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dst, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(userHome, ".codex", "models.json")
	for _, want := range []string{
		`model_provider = "X"`,
		`model = "glm"`,
		`model_reasoning_effort = "high"`,
		`model_catalog_json = "` + catalog + `"`,
		`[model_providers.X]`,
		`token = "secret"`,
		`[mcp_servers.trebi]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, refuse := range []string{
		`notify =`, `instructions =`, `[projects.`, `trust_level`, `[tui]`, `[mcp_servers.other]`, `~/`,
	} {
		if strings.Contains(got, refuse) {
			t.Fatalf("leaked %q in:\n%s", refuse, got)
		}
	}
}

func TestExtractCodexModelConfig(t *testing.T) {
	t.Parallel()
	src := []byte(strings.Join([]string{
		`model = "glm"`,
		`model_provider = "A"`,
		`notify = ["x"]`,
		``,
		`[model_providers.A]`,
		`name = "A"`,
		``,
		`[model_providers.B]`,
		`name = "B"`,
		``,
		`[mcp_servers.other]`,
		`command = "x"`,
	}, "\n"))
	got := extractCodexModelConfig(src, "/home/u")
	if !strings.Contains(got, `[model_providers.A]`) || !strings.Contains(got, `[model_providers.B]`) {
		t.Fatalf("providers:\n%s", got)
	}
	if strings.Contains(got, "notify") || strings.Contains(got, "mcp_servers.other") {
		t.Fatalf("leaked:\n%s", got)
	}
	rewritten := extractCodexModelConfig([]byte(`model_catalog_json = "~/.codex/models.json"`), "/home/u")
	if rewritten != `model_catalog_json = "/home/u/.codex/models.json"` {
		t.Fatalf("catalog %q", rewritten)
	}
}

func TestWriteCodexMCPConfigMissingReal(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	dst := t.TempDir()
	if err := writeCodexMCPConfig(dst, "http://127.0.0.1:1/mcp", "tok"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dst, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(body))
	if !strings.HasPrefix(got, "[mcp_servers.trebi]") {
		t.Fatalf("want only trebi server, got:\n%s", got)
	}
	if strings.Contains(got, "model") {
		t.Fatalf("unexpected model config:\n%s", got)
	}
}

func TestWriteBackCodexAuthNewer(t *testing.T) {
	realHome := t.TempDir()
	scratch := t.TempDir()
	old := []byte(`{"last_refresh":"2026-08-24T16:50:41.665178Z","tokens":{"a":1}}`)
	newer := []byte(`{"last_refresh":"2026-08-31T01:00:00.000000Z","tokens":{"b":2}}`)
	if err := os.WriteFile(filepath.Join(realHome, "auth.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "auth.json"), newer, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", realHome)
	if err := writeBackCodexAuth(scratch); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(realHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newer) {
		t.Fatalf("got %s", got)
	}
	st, err := os.Stat(filepath.Join(realHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %s", st.Mode())
	}
}

func TestWriteBackCodexAuthOlderOrEqual(t *testing.T) {
	realHome := t.TempDir()
	scratch := t.TempDir()
	real := []byte(`{"last_refresh":"2026-08-31T01:00:00Z","tokens":{"keep":true}}`)
	if err := os.WriteFile(filepath.Join(realHome, "auth.json"), real, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", realHome)
	for _, scratchBody := range []string{
		`{"last_refresh":"2026-08-24T16:50:41Z","tokens":{"old":true}}`,
		`{"last_refresh":"2026-08-31T01:00:00Z","tokens":{"eq":true}}`,
	} {
		if err := os.WriteFile(filepath.Join(scratch, "auth.json"), []byte(scratchBody), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeBackCodexAuth(scratch); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(realHome, "auth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(real) {
			t.Fatalf("real mutated for %s: %s", scratchBody, got)
		}
	}
}

func TestWriteBackCodexAuthMalformed(t *testing.T) {
	realHome := t.TempDir()
	scratch := t.TempDir()
	real := []byte(`{"last_refresh":"2026-08-24T16:50:41Z"}`)
	if err := os.WriteFile(filepath.Join(realHome, "auth.json"), real, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", realHome)
	for _, bad := range []string{`{`, `{"tokens":{}}`, `{"last_refresh":""}`} {
		if err := os.WriteFile(filepath.Join(scratch, "auth.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeBackCodexAuth(scratch); err == nil {
			t.Fatalf("want error for %q", bad)
		}
		got, err := os.ReadFile(filepath.Join(realHome, "auth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(real) {
			t.Fatalf("real mutated for %q: %s", bad, got)
		}
	}
}

func dirNames(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}
