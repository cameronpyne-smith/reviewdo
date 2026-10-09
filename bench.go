package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/bench"
	"github.com/cameronpyne-smith/reviewdo/internal/config"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/poller"
)

const benchUsage = `Usage: reviewdo bench [-dir bench] <command>

Commands:
  mine -authors a,b [-org o]       collect pull requests with Copilot reviews into the pool
  label [-since d] [-relabel]      build golden files and label Copilot findings from the author's replies
  propose [-since d] [-n 10] [-include owner/repo#N,...] [-write]
                                   propose a smoke set from the labelled pool
  run -label l [-set smoke.json] [-only owner/repo#N] [-budget d] [-model m] [-think v]
                                   review every PR in the set at the commit Copilot reviewed
  score -label l                   score a run against the golden files
  report -label l [-against l2]    print a run's score, optionally beside another run
`

func benchCmd(ctx context.Context, cfgPath string, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dirFlag := fs.String("dir", "bench", "benchmark data directory")
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchUsage) }
	fs.Parse(args)
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("bench: command required")
	}
	dir := bench.Dir{Root: *dirFlag}
	sub, rest := fs.Arg(0), fs.Args()[1:]
	switch sub {
	case "mine":
		return benchMine(ctx, cfgPath, dir, rest, log)
	case "label":
		return benchLabel(ctx, cfgPath, dir, rest, log)
	case "propose":
		return benchPropose(dir, rest)
	case "run":
		return benchRun(ctx, cfgPath, dir, rest, log)
	case "score":
		return benchScore(ctx, cfgPath, dir, rest, log)
	case "report":
		return benchReport(dir, rest)
	}
	fs.Usage()
	return fmt.Errorf("bench: unknown command %q", sub)
}

func llmFlags(fs *flag.FlagSet) (model, think *string) {
	return fs.String("model", "", "override ollama.model"), fs.String("think", "", "override ollama.think")
}

func withOverrides(a *app, model, think string) *ollama.Client {
	if model == "" && think == "" {
		return a.llm
	}
	o := a.cfg.Ollama
	if model != "" {
		o.Model = model
	}
	if think != "" {
		o.Think = config.ParseThink(think)
	}
	return ollama.New(o.URL, o.Model, o.NumCtx, o.NumPredict, o.Temperature, o.Think.Value(), o.Timeout.Duration)
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", s)
}

func benchMine(ctx context.Context, cfgPath string, dir bench.Dir, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("bench mine", flag.ExitOnError)
	authors := fs.String("authors", "", "comma-separated GitHub logins whose PRs to mine")
	org := fs.String("org", "", "organisation to search (default: owner of the first configured repo)")
	refresh := fs.Bool("refresh", false, "refetch PRs already in the pool")
	fs.Parse(args)
	if *authors == "" {
		return errors.New("bench mine: -authors is required")
	}
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	if *org == "" {
		*org, _, _ = strings.Cut(a.cfg.Repos[0].Name, "/")
	}
	return bench.Mine(ctx, a.gh, dir, bench.MineOptions{Org: *org, Authors: strings.Split(*authors, ","), Refresh: *refresh}, log)
}

func benchLabel(ctx context.Context, cfgPath string, dir bench.Dir, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("bench label", flag.ExitOnError)
	since := fs.String("since", "", "only PRs reviewed on or after this date (YYYY-MM-DD)")
	relabel := fs.Bool("relabel", false, "relabel findings that already have a verdict")
	only := fs.String("only", "", "only this PR (owner/repo#N)")
	model, think := llmFlags(fs)
	fs.Parse(args)
	s, err := parseDate(*since)
	if err != nil {
		return err
	}
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	return bench.Label(ctx, withOverrides(a, *model, *think), dir, bench.LabelOptions{Since: s, Relabel: *relabel, Only: *only}, log)
}

func benchPropose(dir bench.Dir, args []string) error {
	fs := flag.NewFlagSet("bench propose", flag.ExitOnError)
	since := fs.String("since", "2026-03-05", "only PRs reviewed on or after this date")
	n := fs.Int("n", 10, "number of PRs")
	include := fs.String("include", "", "comma-separated owner/repo#N to always include")
	write := fs.Bool("write", false, "write the proposal to smoke.json")
	fs.Parse(args)
	s, err := parseDate(*since)
	if err != nil {
		return err
	}
	var inc []bench.SetEntry
	for _, ref := range strings.Split(*include, ",") {
		if ref == "" {
			continue
		}
		repo, num, err := poller.ParseRef(ref)
		if err != nil {
			return err
		}
		inc = append(inc, bench.SetEntry{Repo: repo, Number: num})
	}
	goldens, err := dir.Goldens()
	if err != nil {
		return err
	}
	set := bench.Propose(goldens, bench.ProposeOptions{Since: s, Count: *n, Include: inc}, os.Stdout)
	if *write {
		return dir.SaveSet("smoke.json", set)
	}
	return nil
}

func benchRun(ctx context.Context, cfgPath string, dir bench.Dir, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("bench run", flag.ExitOnError)
	label := fs.String("label", "", "run label (directory under runs/)")
	set := fs.String("set", "smoke.json", "set file under the bench directory")
	only := fs.String("only", "", "only this PR (owner/repo#N)")
	budget := fs.Duration("budget", 0, "override review.time_budget")
	model, think := llmFlags(fs)
	fs.Parse(args)
	if *label == "" {
		return errors.New("bench run: -label is required")
	}
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	llm := withOverrides(a, *model, *think)
	if err := bench.Run(ctx, a.cfg, a.gh, llm, dir, a.slug, bench.RunOptions{Set: *set, Label: *label, Only: *only, Budget: *budget}, log); err != nil {
		return err
	}
	sc, err := bench.ScoreRun(ctx, llm, dir, *label, log)
	if err != nil {
		return err
	}
	bench.PrintScore(os.Stdout, sc)
	return nil
}

func benchScore(ctx context.Context, cfgPath string, dir bench.Dir, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("bench score", flag.ExitOnError)
	label := fs.String("label", "", "run label")
	model, think := llmFlags(fs)
	fs.Parse(args)
	if *label == "" {
		return errors.New("bench score: -label is required")
	}
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	sc, err := bench.ScoreRun(ctx, withOverrides(a, *model, *think), dir, *label, log)
	if err != nil {
		return err
	}
	bench.PrintScore(os.Stdout, sc)
	return nil
}

func benchReport(dir bench.Dir, args []string) error {
	fs := flag.NewFlagSet("bench report", flag.ExitOnError)
	label := fs.String("label", "", "run label")
	against := fs.String("against", "", "second run label to compare with")
	fs.Parse(args)
	if *label == "" {
		return errors.New("bench report: -label is required")
	}
	sc, err := bench.LoadScore(dir, *label)
	if err != nil {
		return err
	}
	if *against == "" {
		bench.PrintScore(os.Stdout, sc)
		return nil
	}
	other, err := bench.LoadScore(dir, *against)
	if err != nil {
		return err
	}
	bench.PrintCompare(os.Stdout, sc, other)
	return nil
}
