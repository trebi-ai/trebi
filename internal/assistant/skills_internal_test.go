package assistant

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSkillsForSkipsArchived proves the assistant gets no archived global
// skill, and still gets the others.
func TestSkillsForSkipsArchived(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, md := range map[string]string{
		"live":     "# Live\n",
		"archived": "---\narchived_at: 2026-11-18T09:00:00Z\n---\n# Old\n",
	} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{opts: Options{SkillsDir: dir}}
	got := m.skillsFor(context.Background(), "", []string{"live", "archived"})
	if len(got) != 1 || got[0].Name != "live" {
		t.Fatalf("skills %+v", got)
	}
}
