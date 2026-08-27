package job_test

import (
	"context"
	"testing"

	"github.com/trebi-ai/trebi/internal/job"
)

func TestEvalWhenMatch(t *testing.T) {
	t.Parallel()
	w := &job.WhenYAML{
		Run:   "echo hello",
		Match: "event.stdout.includes('hello')",
	}
	res := job.EvalWhen(context.Background(), w, t.TempDir(), nil)
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	if !res.Fire || res.Skip {
		t.Fatalf("%+v", res)
	}
}

func TestEvalWhenSkip(t *testing.T) {
	t.Parallel()
	w := &job.WhenYAML{
		Run:   "echo no",
		Match: "event.stdout.includes('yes')",
	}
	res := job.EvalWhen(context.Background(), w, t.TempDir(), nil)
	if res.Fire {
		t.Fatalf("should skip: %+v", res)
	}
}
