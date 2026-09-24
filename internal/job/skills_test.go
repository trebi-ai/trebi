package job

import "testing"

func TestJobSkillsFrontmatter(t *testing.T) {
	j, err := ParseBytes("/ws/research/JOB.md", []byte("---\nskills:\n  - research\n  - pdf\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Skills) != 2 || j.Skills[0] != "research" || j.Skills[1] != "pdf" {
		t.Fatalf("skills=%+v", j.Skills)
	}
	j, err = ParseBytes("/ws/research/JOB.md", []byte("---\nskills: research\n---\n\nRun.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Skills) != 1 || j.Skills[0] != "research" {
		t.Fatalf("scalar skills=%+v", j.Skills)
	}
}
