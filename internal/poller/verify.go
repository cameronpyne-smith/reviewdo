package poller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/config"
	"github.com/cameronpyne-smith/reviewdo/internal/diff"
	"github.com/cameronpyne-smith/reviewdo/internal/github"
	"github.com/cameronpyne-smith/reviewdo/internal/review"
)

type Verified struct {
	Comment review.Comment
	Verdict review.Verdict
	Err     string
	Took    time.Duration
}

func (p *Poller) VerifyAt(ctx context.Context, repoCfg config.Repo, pull *github.Pull, head, base string, comments []review.Comment, log *slog.Logger) ([]Verified, review.Stats, error) {
	var total review.Stats
	if p.store == nil {
		return nil, total, fmt.Errorf("clone_dir is not set")
	}
	var raw []byte
	var err error
	if head == pull.Head.SHA {
		raw, err = p.gh.PullDiff(ctx, repoCfg.Name, pull.Number)
	} else {
		raw, err = p.gh.CompareDiff(ctx, repoCfg.Name, base, head)
	}
	if err != nil {
		return nil, total, fmt.Errorf("fetch diff: %w", err)
	}
	files := diff.Parse(raw)
	repo, err := p.store.Ensure(ctx, repoCfg.Name, pull.Number, head, base)
	if err != nil {
		return nil, total, err
	}
	header := review.Header(repoCfg.Name, pull)
	dependencies := review.Dependencies(ctx, repo, files)
	fileDiffs := map[string]string{}
	for _, f := range files {
		fileDiffs[f.Path] = f.Render()
	}
	changed := review.ChangedSpans(files)
	maxContext := p.cfg.Ollama.NumCtx * 4 / 5
	var out []Verified
	for _, c := range comments {
		lim := review.Limits{MaxToolCalls: 12, MaxOutput: p.cfg.Review.MaxOutput / 2, MaxContext: maxContext, Deadline: time.Now().Add(10 * time.Minute), Changed: changed}
		v, st, err := review.VerifyFinding(ctx, p.llm, repo, header+"\n"+dependencies, c, fileDiffs[c.Path], lim, log.With("verify", c.Path))
		total.Merge(st)
		item := Verified{Comment: c, Verdict: v, Took: st.Duration}
		if err != nil {
			item.Err = err.Error()
			log.Warn("verification failed", "path", c.Path, "line", c.Line, "err", err)
		} else {
			log.Info("verified finding", "path", c.Path, "line", c.Line, "was", c.Severity, "verdict", v.Verdict, "now", v.Severity, "basis", v.Basis, "reason", v.Reason, "took", st.Duration.Round(time.Second))
		}
		out = append(out, item)
		if ctx.Err() != nil {
			break
		}
	}
	return out, total, nil
}
