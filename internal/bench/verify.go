package bench

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/config"
	"github.com/cameronpyne-smith/reviewdo/internal/github"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/poller"
	"github.com/cameronpyne-smith/reviewdo/internal/review"
)

type VerifyOptions struct {
	From   []string
	Label  string
	Only   string
	Sample int
	Seed   int64
	Prompt string
	List   bool
}

type VerifyItem struct {
	Repo     string        `json:"repo"`
	Number   int           `json:"number"`
	Commit   string        `json:"commit"`
	Source   string        `json:"source"`
	Path     string        `json:"path"`
	Line     int           `json:"line"`
	Severity string        `json:"severity"`
	Body     string        `json:"body"`
	Golden   string        `json:"golden,omitempty"`
	Verdict  string        `json:"verdict,omitempty"`
	Now      string        `json:"now,omitempty"`
	Basis    string        `json:"basis,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Error    string        `json:"error,omitempty"`
	Took     time.Duration `json:"took,omitempty"`
}

func verifyPath(dir Dir, label string) string {
	return filepath.Join(dir.Root, "verify", label+".json")
}

func Verify(ctx context.Context, cfg *config.Config, gh *github.Client, llm *ollama.Client, dir Dir, slug string, opt VerifyOptions, log *slog.Logger) error {
	if opt.Prompt != "" {
		text, err := os.ReadFile(opt.Prompt)
		if err != nil {
			return err
		}
		review.VerifyPrompt = strings.TrimSpace(string(text))
	}
	items, err := verifyItems(dir, opt)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("no findings selected from %v", opt.From)
	}
	if opt.List {
		for _, it := range items {
			mark := " "
			if it.Golden != "" {
				mark = "*"
			}
			fmt.Printf("%s %-14s %s#%d %s:%d %s\n", mark, it.Source, strings.SplitN(it.Repo, "/", 2)[1], it.Number, it.Path, it.Line, truncateTo(strings.SplitN(it.Body, "\n", 2)[0], 80))
		}
		return nil
	}
	bcfg := *cfg
	bcfg.CloneDir = filepath.Join(dir.Root, "clones")
	p := poller.New(&bcfg, gh, llm, nil, false, slug, log)
	p.DryRun = true
	byPR := map[string][]int{}
	var order []string
	for i, it := range items {
		ref := fmt.Sprintf("%s#%d", it.Repo, it.Number)
		if _, ok := byPR[ref]; !ok {
			order = append(order, ref)
		}
		byPR[ref] = append(byPR[ref], i)
	}
	out := verifyPath(dir, opt.Label)
	log.Info("verifying", "findings", len(items), "prs", len(order), "label", opt.Label)
	for _, ref := range order {
		idx := byPR[ref]
		first := items[idx[0]]
		repoCfg := config.Repo{Name: first.Repo}
		for _, r := range cfg.Repos {
			if strings.EqualFold(r.Name, first.Repo) {
				repoCfg = r
			}
		}
		g, err := dir.LoadGolden(first.Repo, first.Number)
		if err != nil {
			return err
		}
		pull, err := gh.Pull(ctx, first.Repo, first.Number)
		if err != nil {
			return err
		}
		var comments []review.Comment
		for _, i := range idx {
			comments = append(comments, review.Comment{Path: items[i].Path, Line: items[i].Line, Severity: items[i].Severity, Body: items[i].Body})
		}
		verified, _, err := p.VerifyAt(ctx, repoCfg, pull, g.Commit, g.BaseSHA, comments, log.With("pr", ref))
		if err != nil {
			log.Warn("verification setup failed", "pr", ref, "err", err)
			continue
		}
		for j, v := range verified {
			it := &items[idx[j]]
			it.Verdict, it.Now, it.Basis, it.Reason, it.Error, it.Took = v.Verdict.Verdict, v.Verdict.Severity, v.Verdict.Basis, v.Verdict.Reason, v.Err, v.Took
		}
		if err := writeJSON(out, items); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	fmt.Print(VerifySummary(items))
	return nil
}

func verifyItems(dir Dir, opt VerifyOptions) ([]VerifyItem, error) {
	seen := map[string]bool{}
	var golden, others []VerifyItem
	for _, label := range opt.From {
		runs, err := LoadRun(dir, label)
		if err != nil {
			return nil, err
		}
		sort.Slice(runs, func(i, j int) bool { return runs[i].Ref() < runs[j].Ref() })
		for _, r := range runs {
			if opt.Only != "" && r.Ref() != opt.Only {
				continue
			}
			g, err := dir.LoadGolden(r.Repo, r.Number)
			if err != nil {
				return nil, err
			}
			valid := map[string][]Finding{}
			for _, f := range g.Scored() {
				if f.Verdict == "valid" {
					valid[f.Path] = append(valid[f.Path], f)
				}
			}
			all := append(append([]RunFinding{}, r.Findings...), r.Rejected...)
			for _, f := range all {
				key := fmt.Sprintf("%s#%d:%s:%d", r.Repo, r.Number, f.Path, f.Line)
				if seen[key] || strings.TrimSpace(f.Body) == "" {
					continue
				}
				seen[key] = true
				it := VerifyItem{Repo: r.Repo, Number: r.Number, Commit: g.Commit, Source: label + "/" + f.ID, Path: f.Path, Line: f.Line, Severity: f.Severity, Body: f.Body}
				for _, v := range valid[f.Path] {
					if abs(v.Line-f.Line) <= 5 {
						it.Golden = v.ID
						break
					}
				}
				if it.Golden != "" {
					golden = append(golden, it)
				} else {
					others = append(others, it)
				}
			}
		}
	}
	rng := rand.New(rand.NewSource(opt.Seed))
	rng.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
	if opt.Sample >= 0 && len(others) > opt.Sample {
		others = others[:opt.Sample]
	}
	items := append(golden, others...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Repo != items[j].Repo {
			return items[i].Repo < items[j].Repo
		}
		return items[i].Number < items[j].Number
	})
	return items, nil
}

func VerifySummary(items []VerifyItem) string {
	var b strings.Builder
	count := func(golden bool) (kept, rejected, failed int) {
		for _, it := range items {
			if (it.Golden != "") != golden {
				continue
			}
			switch {
			case it.Error != "" || it.Verdict == "":
				failed++
			case it.Verdict == "rejected":
				rejected++
			default:
				kept++
			}
		}
		return
	}
	gk, gr, gf := count(true)
	ok, or, of := count(false)
	fmt.Fprintf(&b, "golden findings: %d kept, %d rejected, %d failed\n", gk, gr, gf)
	fmt.Fprintf(&b, "other findings:  %d kept, %d rejected, %d failed\n", ok, or, of)
	for _, it := range items {
		if it.Golden == "" {
			continue
		}
		fmt.Fprintf(&b, "  %-9s %s %s:%d %s\n", it.Verdict, strings.SplitN(it.Repo, "/", 2)[1], it.Path, it.Line, truncateTo(it.Reason, 110))
	}
	return b.String()
}

func truncateTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
