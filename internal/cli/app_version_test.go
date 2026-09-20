package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/api"
)

func TestVersionFlag(t *testing.T) {
	old := api.Version
	t.Cleanup(func() { api.Version = old })
	api.Version = "0.2.9"

	t.Run("free", func(t *testing.T) {
		var buf bytes.Buffer
		app := NewApp(Options{})
		app.Writer = &buf
		if err := app.Run(context.Background(), []string{"trebi", "--version"}); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, "0.2.9") {
			t.Fatalf("version %q", got)
		}
		if strings.Contains(got, "(pro)") {
			t.Fatalf("free edition leaked pro: %q", got)
		}
	})

	t.Run("pro", func(t *testing.T) {
		var buf bytes.Buffer
		app := NewApp(Options{Ext: ext.Set{Edition: "pro"}})
		app.Writer = &buf
		if err := app.Run(context.Background(), []string{"trebi", "--version"}); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, "0.2.9 (pro)") {
			t.Fatalf("version %q", got)
		}
	})
}
