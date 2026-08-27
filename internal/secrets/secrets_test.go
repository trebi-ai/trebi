package secrets_test

import (
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
