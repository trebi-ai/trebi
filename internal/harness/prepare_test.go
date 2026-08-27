package harness

import (
	"os"
	"path/filepath"
	"testing"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
)

func TestWriteSecretEnv_Mode0600(t *testing.T) {
	home.SetBase(t.TempDir())
	defer home.SetBase("")
	if err := os.MkdirAll(config.SecretsDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretEnv("claude", map[string]string{"ANTHROPIC_API_KEY": "sekrit"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(config.SecretEnvPath("claude"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("got mode %o", st.Mode().Perm())
	}
	env, err := LoadSecretEnv("claude")
	if err != nil || env["ANTHROPIC_API_KEY"] != "sekrit" {
		t.Fatalf("%v %v", env, err)
	}
}

func TestWriteFileIfChanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := WriteFileIfChanged(path, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	// Second write same content is fine.
	if err := WriteFileIfChanged(path, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileIfChanged(path, []byte("v2"), 0644); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "v2" {
		t.Fatalf("%s", data)
	}
}
