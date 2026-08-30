package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
)

type syncWriter struct{ f *os.File }

func (s syncWriter) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	_ = s.f.Sync()
	return n, err
}

// NewApp builds the trebi-eval command surface.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:      "trebi-eval",
		Usage:     "live LLM-driven CLI eval suite for trebi (dev-only)",
		Writer:    syncWriter{os.Stdout},
		ErrWriter: syncWriter{os.Stderr},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "evals", Value: "evals", Usage: "evals/ directory"},
			&cli.StringFlag{Name: "out", Value: "out", Usage: "report output directory"},
			&cli.StringFlag{Name: "bin", Sources: cli.EnvVars("TREBI_BIN"), Usage: "trebi binary"},
			&cli.StringFlag{Name: "skill", Value: "SKILL.md", Usage: "path to SKILL.md"},
		},
		Commands: []*cli.Command{
			{
				Name:  "run",
				Usage: "run the eval suite",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "task", Usage: "single task id"},
					&cli.StringFlag{Name: "tier", Usage: "onboarding|operate|debug|guardrail"},
					&cli.StringSliceFlag{Name: "client", Usage: "claude and/or codex (repeatable)"},
					&cli.IntFlag{Name: "trials", Usage: "override trials per task"},
					&cli.Float64Flag{Name: "budget", Usage: "abort when cumulative cost_usd reaches this"},
					&cli.StringFlag{Name: "baseline", Usage: "checked-in baseline.json"},
				},
				Action: runEval,
			},
			{
				Name:      "oracle",
				Usage:     "replay the reference script for a task",
				ArgsUsage: "<task-id>",
				Action:    runOracle,
			},
			{
				Name:  "review",
				Usage: "sample trials for human labels, or score labeled agreement",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "run", Required: true, Usage: "out/<runid> directory"},
					&cli.IntFlag{Name: "sample", Value: 8, Usage: "how many verdicts to sample"},
					&cli.StringFlag{Name: "labels", Usage: "labeled JSON; if set, compute agreement"},
					&cli.StringFlag{Name: "queue", Usage: "where to write the unlabeled sample"},
				},
				Action: runReview,
			},
		},
	}
}

func runEval(ctx context.Context, c *cli.Command) error {
	opts, err := optionsFrom(c)
	if err != nil {
		return err
	}
	opts.TaskID = c.String("task")
	opts.Tier = c.String("tier")
	opts.Clients = c.StringSlice("client")
	opts.Trials = c.Int("trials")
	opts.BudgetUSD = c.Float64("budget")
	opts.Baseline = c.String("baseline")
	r, err := NewRunner(opts)
	if err != nil {
		return err
	}
	rep, err := r.Run(ctx)
	if rep != nil {
		fmt.Fprintf(c.Root().Writer, "report %s  cost=%.4f  trials=%d\n",
			filepath.Join(opts.OutDir, rep.ID), rep.CostUSD, len(rep.Trials))
		fmt.Fprint(c.Root().Writer, rep.markdown())
	}
	return err
}

func runOracle(ctx context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return fmt.Errorf("usage: trebi-eval oracle <task-id>")
	}
	opts, err := optionsFrom(c)
	if err != nil {
		return err
	}
	r, err := NewRunner(opts)
	if err != nil {
		return err
	}
	tr, err := r.ReplayOracle(ctx, id)
	if tr != nil {
		fmt.Fprintf(c.Root().Writer, "oracle %s outcome=%s dir=%s\n", id, tr.Outcome, tr.Dir)
		for _, ch := range tr.Checks {
			mark := "FAIL"
			if ch.Pass {
				mark = "PASS"
			}
			fmt.Fprintf(c.Root().Writer, "  %s %s %s\n", mark, ch.Name, ch.Detail)
		}
	}
	return err
}

func runReview(_ context.Context, c *cli.Command) error {
	dir := c.String("run")
	if labels := c.String("labels"); labels != "" {
		items, err := loadReview(labels)
		if err != nil {
			return err
		}
		rate, agree, total, err := agreement(items)
		if err != nil {
			return err
		}
		msg := reviewReport(rate, agree, total)
		fmt.Fprint(c.Root().Writer, msg)
		if rate+1e-9 < reviewAgreeMin {
			return fmt.Errorf("agreement %.2f below %.2f", rate, reviewAgreeMin)
		}
		return nil
	}
	items, err := sampleReview(dir, c.Int("sample"))
	if err != nil {
		return err
	}
	q := c.String("queue")
	if q == "" {
		q = filepath.Join(dir, "review-queue.json")
	}
	if err := writeReviewQueue(q, items); err != nil {
		return err
	}
	fmt.Fprintf(c.Root().Writer, "wrote %d items to %s\nset human_pass on each, then: trebi-eval review --run %s --labels %s\n",
		len(items), q, dir, q)
	return nil
}

func optionsFrom(c *cli.Command) (Options, error) {
	evals := c.String("evals")
	skill := c.String("skill")
	if !filepath.IsAbs(evals) {
		wd, _ := os.Getwd()
		evals = filepath.Join(wd, evals)
	}
	if !filepath.IsAbs(skill) {
		wd, _ := os.Getwd()
		cand := filepath.Join(wd, skill)
		if _, err := os.Stat(cand); err != nil {
			cand = filepath.Join(filepath.Dir(evals), skill)
		}
		skill = cand
	}
	return Options{
		EvalsDir:  evals,
		OutDir:    c.String("out"),
		TrebiBin:  c.String("bin"),
		SkillPath: skill,
	}, nil
}
