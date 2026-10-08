package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/config"
	"github.com/cameronpyne-smith/reviewdo/internal/github"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/poller"
	"github.com/cameronpyne-smith/reviewdo/internal/state"
)

const usage = `Usage: reviewdo [-config path] [-v] <command>

Commands:
  run                      poll configured repositories and review pull requests (default)
  check                    verify key, GitHub access, installation repos and Ollama
  pulls owner/repo         list open pull requests
  review owner/repo#N      review one pull request and print the result
  review -post owner/repo#N
                           review one pull request and post it to GitHub
  review -model m -think true|false owner/repo#N
                           review with a different model or thinking setting
`

func main() {
	fs := flag.NewFlagSet("reviewdo", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/reviewdo/config.json", "config file")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(os.Args[1:])

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cmd := "run"
	if fs.NArg() > 0 {
		cmd = fs.Arg(0)
	}
	args := fs.Args()
	if len(args) > 0 {
		args = args[1:]
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "run":
		err = run(ctx, *cfgPath, log)
	case "check":
		err = check(ctx, *cfgPath)
	case "pulls":
		err = listPulls(ctx, *cfgPath, args)
	case "review":
		err = reviewOne(ctx, *cfgPath, args, log)
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type app struct {
	cfg  *config.Config
	gh   *github.Client
	llm  *ollama.Client
	slug string
}

func setup(ctx context.Context, cfgPath string) (*app, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	key, err := github.LoadPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	gh := github.New(key, cfg.AppID, cfg.InstallationID)
	ga, err := gh.App(ctx)
	if err != nil {
		return nil, fmt.Errorf("authenticate as app: %w", err)
	}
	o := cfg.Ollama
	llm := ollama.New(o.URL, o.Model, o.NumCtx, o.Temperature, o.Think, o.Timeout.Duration)
	return &app{cfg: cfg, gh: gh, llm: llm, slug: ga.Slug}, nil
}

func run(ctx context.Context, cfgPath string, log *slog.Logger) error {
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	st, existed, err := state.Load(a.cfg.StatePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	log.Info("starting", "app", a.slug, "model", a.cfg.Ollama.Model, "repos", len(a.cfg.Repos), "interval", a.cfg.PollInterval.Duration, "state", a.cfg.StatePath)
	return poller.New(a.cfg, a.gh, a.llm, st, !existed, a.slug, log).Run(ctx)
}

func check(ctx context.Context, cfgPath string) error {
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("app:            %s (id %d)\n", a.slug, a.cfg.AppID)
	repos, err := a.gh.InstallationRepositories(ctx)
	if err != nil {
		return fmt.Errorf("installation %d: %w", a.cfg.InstallationID, err)
	}
	have := map[string]bool{}
	for _, r := range repos {
		have[strings.ToLower(r.FullName)] = true
	}
	fmt.Printf("installation:   %d, %d repositories accessible\n", a.cfg.InstallationID, len(repos))
	var problems []error
	for _, r := range a.cfg.Repos {
		if have[strings.ToLower(r.Name)] {
			fmt.Printf("repo:           %s ok\n", r.Name)
		} else {
			fmt.Printf("repo:           %s NOT accessible to this installation\n", r.Name)
			problems = append(problems, fmt.Errorf("repo %s not accessible", r.Name))
		}
	}
	mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	models, err := a.llm.Models(mctx)
	if err != nil {
		return err
	}
	found := false
	for _, m := range models {
		if m == a.cfg.Ollama.Model || m == a.cfg.Ollama.Model+":latest" {
			found = true
		}
	}
	if found {
		fmt.Printf("ollama:         %s, model %s present\n", a.cfg.Ollama.URL, a.cfg.Ollama.Model)
	} else {
		fmt.Printf("ollama:         %s reachable, model %s NOT found (have: %s)\n", a.cfg.Ollama.URL, a.cfg.Ollama.Model, strings.Join(models, ", "))
		problems = append(problems, fmt.Errorf("model %s not found", a.cfg.Ollama.Model))
	}
	fmt.Printf("state:          %s\n", a.cfg.StatePath)
	return errors.Join(problems...)
}

func listPulls(ctx context.Context, cfgPath string, args []string) error {
	if len(args) != 1 || strings.Count(args[0], "/") != 1 {
		return errors.New("usage: reviewdo pulls owner/repo")
	}
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	pulls, err := a.gh.ListOpenPulls(ctx, args[0])
	if err != nil {
		return err
	}
	for _, p := range pulls {
		draft := ""
		if p.Draft {
			draft = " (draft)"
		}
		fmt.Printf("#%-5d %-8s %-20s %s%s\n", p.Number, p.Head.SHA[:7], p.User.Login, p.Title, draft)
	}
	return nil
}

func reviewOne(ctx context.Context, cfgPath string, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	post := fs.Bool("post", false, "post the review to GitHub instead of printing it")
	model := fs.String("model", "", "override ollama.model")
	think := fs.String("think", "", "override ollama.think (true or false)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: reviewdo review [-post] owner/repo#N")
	}
	repoName, number, err := poller.ParseRef(fs.Arg(0))
	if err != nil {
		return err
	}
	a, err := setup(ctx, cfgPath)
	if err != nil {
		return err
	}
	if *model != "" || *think != "" {
		o := a.cfg.Ollama
		if *model != "" {
			o.Model = *model
		}
		if *think != "" {
			v := *think == "true"
			o.Think = &v
		}
		a.llm = ollama.New(o.URL, o.Model, o.NumCtx, o.Temperature, o.Think, o.Timeout.Duration)
	}
	repo := config.Repo{Name: repoName}
	for _, r := range a.cfg.Repos {
		if strings.EqualFold(r.Name, repoName) {
			repo = r
		}
	}
	pull, err := a.gh.Pull(ctx, repoName, number)
	if err != nil {
		return err
	}
	p := poller.New(a.cfg, a.gh, a.llm, &state.State{}, false, a.slug, log)
	p.DryRun = !*post
	return p.Review(ctx, repo, pull, "")
}
