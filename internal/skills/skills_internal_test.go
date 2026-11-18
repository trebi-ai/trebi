package skills

import "testing"

func TestSetFrontmatterKey(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, md, key, value, want string }{
		{"add to frontmatter", "---\ndescription: x\n---\n# T\n", "archived_at", "2026", "---\ndescription: x\narchived_at: 2026\n---\n# T\n"},
		{"replace", "---\narchived_at: old\ndescription: x\n---\n# T\n", "archived_at", "new", "---\ndescription: x\narchived_at: new\n---\n# T\n"},
		{"remove", "---\ndescription: x\narchived_at: old\n---\n# T\n", "archived_at", "", "---\ndescription: x\n---\n# T\n"},
		{"remove last key drops the block", "---\narchived_at: old\n---\n# T\n", "archived_at", "", "# T\n"},
		{"no frontmatter adds one", "# T\n", "archived_at", "2026", "---\narchived_at: 2026\n---\n# T\n"},
		{"no frontmatter, remove", "# T\n", "archived_at", "", "# T\n"},
		{"indented key stays", "---\nmetadata:\n  archived_at: x\n---\n# T\n", "archived_at", "", "---\nmetadata:\n  archived_at: x\n---\n# T\n"},
		{"time value", "---\nname: a\n---\nbody\n", "archived_at", "2026-11-18T09:00:00Z", "---\nname: a\narchived_at: 2026-11-18T09:00:00Z\n---\nbody\n"},
	}
	for _, c := range cases {
		if got := setFrontmatterKey(c.md, c.key, c.value); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestKeepArchiveKeys(t *testing.T) {
	t.Parallel()
	old := "---\narchived_at: T\narchived_links: claude\n---\n# Old\n"
	if got := keepArchiveKeys(old, "# New\n"); got != "---\narchived_at: T\narchived_links: claude\n---\n# New\n" {
		t.Fatalf("got %q", got)
	}
	// The new text wins when it names the key.
	if got := keepArchiveKeys(old, "---\narchived_at: U\n---\n# New\n"); got != "---\narchived_at: U\narchived_links: claude\n---\n# New\n" {
		t.Fatalf("got %q", got)
	}
}
