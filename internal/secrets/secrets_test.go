package secrets_test

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
)

func TestSecretRedactionAndRoundTrip(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })

	if err := secrets.Set("api-key", "super-secret"); err != nil {
		t.Fatal(err)
	}
	names, err := secrets.List()
	if err != nil || len(names) != 1 || names[0] != "api-key" {
		t.Fatalf("%v %v", names, err)
	}
	v, err := secrets.Get("api-key")
	if err != nil || string(v) != "super-secret" {
		t.Fatal(err)
	}
	// LogValue must not leak
	if v.LogValue().String() == "super-secret" {
		t.Fatal("leaked")
	}
	if v.LogValue().Equal(slog.StringValue("[secret]")) == false {
		// just ensure it is not the raw secret
		if v.LogValue().String() != "[secret]" {
			t.Fatalf("%v", v.LogValue())
		}
	}
	if err := secrets.Delete("api-key"); err != nil {
		t.Fatal(err)
	}
}

func TestExpandRefs(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	if err := secrets.Set("eval_api_key", "sk-secret"); err != nil {
		t.Fatal(err)
	}
	out, missing := secrets.ExpandRefs(map[string]string{
		"EVAL_API_KEY": "eval_api_key",
		"PLAIN":        "literal",
	})
	if len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if out["EVAL_API_KEY"] != "sk-secret" || out["PLAIN"] != "literal" {
		t.Fatalf("%v", out)
	}
	out, missing = secrets.ExpandRefs(map[string]string{"X": "no-such-secret"})
	if len(missing) != 0 || out["X"] != "no-such-secret" {
		t.Fatalf("%v %v", out, missing)
	}

	out, missing = secrets.ExpandRefs(map[string]string{
		"EVAL_API_KEY": "secret:eval_api_key",
		"MISSING":      "secret:no-such-secret",
		"PLAIN":        "literal",
	})
	if out["EVAL_API_KEY"] != "sk-secret" || out["PLAIN"] != "literal" || out["MISSING"] != "secret:no-such-secret" {
		t.Fatalf("%v", out)
	}
	if len(missing) != 1 || missing[0] != "no-such-secret" {
		t.Fatalf("missing %v", missing)
	}
}

// TestSecretCheckName proves a name that could leave the secrets directory is
// refused by every file operation.
func TestSecretCheckName(t *testing.T) {
	home.SetBase(t.TempDir())
	t.Cleanup(func() { home.SetBase("") })
	for _, good := range []string{"api-key", "assistant.api_key", "docker-mcp_TOKEN", "GH_TOKEN"} {
		if err := secrets.CheckName(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "..", "x_../../y"} {
		if err := secrets.CheckName(bad); !errors.Is(err, secrets.ErrBadName) {
			t.Errorf("%q passed", bad)
		}
		if err := secrets.Set(bad, "v"); !errors.Is(err, secrets.ErrBadName) {
			t.Errorf("set %q: %v", bad, err)
		}
		if _, err := secrets.Get(bad); !errors.Is(err, secrets.ErrBadName) {
			t.Errorf("get %q: %v", bad, err)
		}
		if err := secrets.Delete(bad); !errors.Is(err, secrets.ErrBadName) {
			t.Errorf("delete %q: %v", bad, err)
		}
	}
}

func TestOwnerOf(t *testing.T) {
	label := func(id string) string { return "L:" + id }
	for name, want := range map[string]secrets.Owner{
		"harness.claude-zai.ANTHROPIC_AUTH_TOKEN": {Kind: secrets.OwnerHarness, Label: "L:claude-zai", Link: "/settings/harnesses?configure=claude-zai"},
		"harness.a.b.KEY":                         {Kind: secrets.OwnerHarness, Label: "L:a.b", Link: "/settings/harnesses?configure=a.b"},
		"cloud_device_key":                        {Kind: secrets.OwnerCloud, Label: "Trebi account", Link: "/settings/cloud"},
		"mobile_relay_key":                        {Kind: secrets.OwnerMobile, Label: "Phones and tablets", Link: "/settings/mobile"},
		"machine_link_01ABC":                      {Kind: secrets.OwnerMachine, Label: "Machines", Link: "/settings/machines"},
		"assistant.api_key":                       {Kind: secrets.OwnerAssistant, Label: "Assistant", Link: "/settings/assistant"},
	} {
		if got, ok := secrets.OwnerOf(name, label); !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", name, got, ok, want)
		}
	}
	for _, name := range []string{"GITHUB_TOKEN", "harness.KEY", "oauth.github"} {
		if got, ok := secrets.OwnerOf(name, label); ok {
			t.Errorf("%s: owner %+v, want none", name, got)
		}
	}
}
