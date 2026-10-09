package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/config"
	"github.com/cameronpyne-smith/reviewdo/internal/github"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/poller"
)

type RunFinding struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
	Dropped  bool   `json:"dropped,omitempty"`
}

type RunResult struct {
	Repo         string        `json:"repo"`
	Number       int           `json:"number"`
	Commit       string        `json:"commit"`
	Model        string        `json:"model"`
	Error        string        `json:"error,omitempty"`
	DiffOnly     bool          `json:"diff_only,omitempty"`
	Verdict      string        `json:"verdict"`
	Summary      string        `json:"summary"`
	Body         string        `json:"body"`
	Findings     []RunFinding  `json:"findings"`
	Rejected     []RunFinding  `json:"rejected,omitempty"`
	Unverified   int           `json:"unverified,omitempty"`
	Parts        int           `json:"parts"`
	Rounds       int           `json:"rounds"`
	ToolCalls    int           `json:"tool_calls"`
	PromptTokens int           `json:"prompt_tokens"`
	OutputTokens int           `json:"output_tokens"`
	Took         time.Duration `json:"took"`
	StartedAt    time.Time     `json:"started_at"`
}

func (r *RunResult) Ref() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

type RunOptions struct {
	Set    string
	Label  string
	Only   string
	Budget time.Duration
}

func runPath(dir Dir, label, repo string, number int) string {
	return filepath.Join(dir.Root, "runs", label, strings.ReplaceAll(repo, "/", "__")+fmt.Sprintf("__%d.json", number))
}

func Run(ctx context.Context, cfg *config.Config, gh *github.Client, llm *ollama.Client, dir Dir, slug string, opt RunOptions, log *slog.Logger) error {
	set, err := dir.LoadSet(opt.Set)
	if err != nil {
		return err
	}
	bcfg := *cfg
	bcfg.CloneDir = filepath.Join(dir.Root, "clones")
	if opt.Budget > 0 {
		bcfg.Review.TimeBudget.Duration = opt.Budget
		bcfg.Review.Timeout.Duration = 3 * opt.Budget
	}
	p := poller.New(&bcfg, gh, llm, nil, false, slug, log)
	p.DryRun = true
	for _, e := range set {
		ref := fmt.Sprintf("%s#%d", e.Repo, e.Number)
		if opt.Only != "" && ref != opt.Only {
			continue
		}
		out := runPath(dir, opt.Label, e.Repo, e.Number)
		if _, err := os.Stat(out); err == nil {
			log.Info("already run, skipping", "pr", ref)
			continue
		}
		g, err := dir.LoadGolden(e.Repo, e.Number)
		if err != nil {
			return fmt.Errorf("%s: golden: %w", ref, err)
		}
		res := runOne(ctx, p, &bcfg, gh, llm, g, log.With("pr", ref))
		if err := writeJSON(out, res); err != nil {
			return err
		}
		if res.Error != "" {
			log.Warn("review failed", "pr", ref, "err", res.Error)
		} else {
			log.Info("review done", "pr", ref, "findings", len(res.Findings), "took", res.Took.Round(time.Second))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func runOne(ctx context.Context, p *poller.Poller, cfg *config.Config, gh *github.Client, llm *ollama.Client, g *Golden, log *slog.Logger) *RunResult {
	res := &RunResult{Repo: g.Repo, Number: g.Number, Commit: g.Commit, Model: llm.Model(), StartedAt: time.Now()}
	repoCfg := config.Repo{Name: g.Repo}
	for _, r := range cfg.Repos {
		if strings.EqualFold(r.Name, g.Repo) {
			repoCfg = r
		}
	}
	pull, err := gh.Pull(ctx, g.Repo, g.Number)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	rctx, cancel := context.WithTimeout(ctx, cfg.Review.Timeout.Duration)
	defer cancel()
	oc, err := p.ReviewAt(rctx, repoCfg, pull, g.Commit, g.BaseSHA, "", log)
	res.Took = time.Since(res.StartedAt)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if oc.Empty {
		res.Verdict, res.Summary = "empty", "nothing reviewable"
		return res
	}
	res.DiffOnly = oc.DiffOnly
	res.Verdict, res.Summary, res.Body = oc.Result.Verdict, oc.Result.Summary, oc.Output.Body
	res.Parts, res.Rounds, res.ToolCalls = oc.Parts, oc.Stats.Rounds, oc.Stats.ToolCalls
	res.PromptTokens, res.OutputTokens, res.Unverified = oc.Stats.PromptTokens, oc.Stats.OutputTokens, oc.Result.Unverified
	posted := map[string]bool{}
	for _, c := range oc.Output.Comments {
		posted[fmt.Sprintf("%s:%d", c.Path, c.Line)] = true
	}
	n := 0
	seen := map[string]bool{}
	for _, c := range oc.Result.Comments {
		key := fmt.Sprintf("%s:%d", c.Path, c.Line)
		if seen[key] {
			continue
		}
		seen[key] = true
		n++
		res.Findings = append(res.Findings, RunFinding{ID: fmt.Sprintf("rd-%d", n), Path: c.Path, Line: c.Line, Severity: c.Severity, Body: c.Body, Dropped: !posted[key]})
	}
	for i, c := range oc.Rejected {
		res.Rejected = append(res.Rejected, RunFinding{ID: fmt.Sprintf("rej-%d", i+1), Path: c.Path, Line: c.Line, Severity: c.Severity, Body: c.Body})
	}
	return res
}

type PRScore struct {
	Ref        string   `json:"ref"`
	Valid      int      `json:"valid"`
	Hit        int      `json:"hit"`
	HitDet     int      `json:"hit_det"`
	Invalid    int      `json:"invalid"`
	Reproduced int      `json:"reproduced"`
	Posted     int      `json:"posted"`
	Unmatched  int      `json:"unmatched"`
	Dropped    int      `json:"dropped"`
	Took       string   `json:"took"`
	ToolCalls  int      `json:"tool_calls"`
	Output     int      `json:"output_tokens"`
	Error      string   `json:"error,omitempty"`
	DiffOnly   bool     `json:"diff_only,omitempty"`
	Hits       []string `json:"hits,omitempty"`
	Misses     []string `json:"misses,omitempty"`
}

type Score struct {
	Label string    `json:"label"`
	Model string    `json:"model"`
	PRs   []PRScore `json:"prs"`
	Total PRScore   `json:"total"`
}

func LoadRun(dir Dir, label string) ([]*RunResult, error) {
	var out []*RunResult
	root := filepath.Join(dir.Root, "runs", label)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "score.json" {
			continue
		}
		var r RunResult
		if err := readJSON(filepath.Join(root, e.Name()), &r); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref() < out[j].Ref() })
	return out, nil
}

func ScoreRun(ctx context.Context, llm *ollama.Client, dir Dir, label string, log *slog.Logger) (*Score, error) {
	runs, err := LoadRun(dir, label)
	if err != nil {
		return nil, err
	}
	sc := &Score{Label: label}
	var triage strings.Builder
	for _, r := range runs {
		sc.Model = r.Model
		g, err := dir.LoadGolden(r.Repo, r.Number)
		if err != nil {
			return nil, err
		}
		ps := PRScore{Ref: r.Ref(), Error: r.Error, DiffOnly: r.DiffOnly, Took: r.Took.Round(time.Second).String(), ToolCalls: r.ToolCalls, Output: r.OutputTokens}
		var golden []Finding
		for _, f := range g.Scored() {
			if f.Verdict == "valid" || f.Verdict == "invalid" {
				golden = append(golden, f)
			}
		}
		var posted []RunFinding
		for _, f := range r.Findings {
			if f.Dropped {
				ps.Dropped++
			} else {
				posted = append(posted, f)
			}
		}
		ps.Posted = len(posted)
		matched := map[string]string{}
		pairs, err := judge(ctx, llm, golden, posted)
		if err != nil {
			log.Warn("judge failed, using deterministic matches only", "pr", r.Ref(), "err", err)
		}
		for _, p := range pairs {
			if _, ok := matched[p.Reference]; !ok {
				matched[p.Reference] = p.Candidate
			}
		}
		det := map[string]string{}
		for _, gf := range golden {
			for _, rf := range posted {
				if rf.Path == gf.Path && abs(rf.Line-gf.Line) <= 5 {
					det[gf.ID] = rf.ID
					break
				}
			}
		}
		usedCandidates := map[string]bool{}
		for _, gf := range golden {
			cand, ok := matched[gf.ID]
			if ok {
				usedCandidates[cand] = true
			}
			switch gf.Verdict {
			case "valid":
				ps.Valid++
				title := firstLine(gf.Body)
				if ok {
					ps.Hit++
					ps.Hits = append(ps.Hits, title)
				} else {
					ps.Misses = append(ps.Misses, title)
				}
				if _, ok := det[gf.ID]; ok {
					ps.HitDet++
				}
			case "invalid":
				ps.Invalid++
				if ok {
					ps.Reproduced++
				}
			}
		}
		for _, rf := range posted {
			if !usedCandidates[rf.ID] {
				ps.Unmatched++
				fmt.Fprintf(&triage, "## %s %s:%d (%s) [%s]\n\n%s\n\n", r.Ref(), rf.Path, rf.Line, rf.Severity, rf.ID, rf.Body)
			}
		}
		sc.PRs = append(sc.PRs, ps)
		sc.Total.Valid += ps.Valid
		sc.Total.Hit += ps.Hit
		sc.Total.HitDet += ps.HitDet
		sc.Total.Invalid += ps.Invalid
		sc.Total.Reproduced += ps.Reproduced
		sc.Total.Posted += ps.Posted
		sc.Total.Unmatched += ps.Unmatched
		sc.Total.Dropped += ps.Dropped
		sc.Total.ToolCalls += ps.ToolCalls
		sc.Total.Output += ps.Output
	}
	var took time.Duration
	for _, r := range runs {
		took += r.Took
	}
	sc.Total.Ref = "total"
	sc.Total.Took = took.Round(time.Second).String()
	if err := writeJSON(filepath.Join(dir.Root, "runs", label, "score.json"), sc); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir.Root, "runs", label, "triage.md"), []byte(triage.String()), 0o644); err != nil {
		return nil, err
	}
	return sc, nil
}

func LoadScore(dir Dir, label string) (*Score, error) {
	var sc Score
	err := readJSON(filepath.Join(dir.Root, "runs", label, "score.json"), &sc)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("run %s has not been scored", label)
	}
	return &sc, err
}

func PrintScore(w io.Writer, sc *Score) {
	fmt.Fprintf(w, "run %s  model %s\n\n", sc.Label, sc.Model)
	fmt.Fprintf(w, "%-40s %5s %5s %5s %6s %6s %6s %5s %8s %6s\n", "pr", "valid", "hit", "det", "inval", "repro", "posted", "unmat", "took", "tools")
	rows := append(append([]PRScore{}, sc.PRs...), sc.Total)
	for _, p := range rows {
		ref := p.Ref
		if p.Error != "" {
			ref += " (failed)"
		} else if p.DiffOnly {
			ref += " (diff only)"
		}
		fmt.Fprintf(w, "%-40s %5d %5d %5d %6d %6d %6d %5d %8s %6d\n", ref, p.Valid, p.Hit, p.HitDet, p.Invalid, p.Reproduced, p.Posted, p.Unmatched, p.Took, p.ToolCalls)
	}
	if sc.Total.Valid > 0 {
		fmt.Fprintf(w, "\nrecall %.0f%% (%d/%d valid findings)", 100*float64(sc.Total.Hit)/float64(sc.Total.Valid), sc.Total.Hit, sc.Total.Valid)
	}
	if sc.Total.Invalid > 0 {
		fmt.Fprintf(w, "  reproduced noise %d/%d", sc.Total.Reproduced, sc.Total.Invalid)
	}
	fmt.Fprintf(w, "  unmatched %d of %d posted\n", sc.Total.Unmatched, sc.Total.Posted)
}

func PrintCompare(w io.Writer, a, b *Score) {
	fmt.Fprintf(w, "%-40s %-16s %-16s\n", "pr", a.Label, b.Label)
	bm := map[string]PRScore{}
	for _, p := range b.PRs {
		bm[p.Ref] = p
	}
	cell := func(p PRScore) string {
		return fmt.Sprintf("%d/%d +%d ~%d %s", p.Hit, p.Valid, p.Unmatched, p.Reproduced, p.Took)
	}
	for _, p := range a.PRs {
		fmt.Fprintf(w, "%-40s %-16s %-16s\n", p.Ref, cell(p), cell(bm[p.Ref]))
	}
	fmt.Fprintf(w, "%-40s %-16s %-16s\n", "total", cell(a.Total), cell(b.Total))
	fmt.Fprintln(w, "\ncells: hit/valid +unmatched ~reproduced-noise took")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(strings.TrimLeft(s, "#* "), 100)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
