package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/review"
)

const labelPrompt = `You classify one comment left by an automated code reviewer on a pull request, using the reply the pull request author wrote underneath it. Decide whether the author treated the finding as a real problem.

valid: the author accepted it as a real issue. Fixing it, agreeing with it, saying it will be addressed, or mentioning a commit that addresses it all count, even if the fix was deferred.
invalid: the author rejected it: wrong, not applicable, intentional, already handled elsewhere, or outside the scope the reviewer should have understood from the code.
unclear: the reply does not say either way, or only asks a question, or accepts part and rejects part.

Reply with JSON only: {"verdict": "valid" | "invalid" | "unclear", "reason": "one short sentence"}`

var labelSchema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string","enum":["valid","invalid","unclear"]},"reason":{"type":"string"}},"required":["verdict","reason"]}`)

var commitRef = regexp.MustCompile(`(?i)(/commit/[0-9a-f]{7,40}|\b[0-9a-f]{7,40}\b)`)

type LabelOptions struct {
	Since   time.Time
	Relabel bool
	Only    string
}

func BuildGolden(p *PR, existing *Golden) *Golden {
	g := &Golden{Repo: p.Repo, Number: p.Number, Title: p.Title, Author: p.Author, BaseRef: p.BaseRef, BaseSHA: p.BaseSHA, Size: p.Additions + p.Deletions, Files: p.ChangedFiles}
	keep := map[string]Finding{}
	if existing != nil {
		for _, f := range existing.Findings {
			keep[f.ID] = f
		}
	}
	for _, t := range p.Threads {
		first := t.Comments[0]
		f := Finding{ID: t.ID, Source: "copilot", Commit: first.ReviewCommit, Path: t.Path, Line: t.OriginalLine, Body: strings.TrimSpace(first.Body), Reply: authorReplies(t), Resolved: t.Resolved, At: first.ReviewSubmitted}
		if f.Line == 0 {
			f.Line = t.Line
		}
		if old, ok := keep[t.ID]; ok {
			f.Verdict, f.Reason, f.Labelled, f.Locked = old.Verdict, old.Reason, old.Labelled, old.Locked
			delete(keep, t.ID)
		}
		g.Findings = append(g.Findings, f)
	}
	for _, f := range keep {
		if f.Source != "copilot" {
			g.Findings = append(g.Findings, f)
		}
	}
	for _, f := range g.Findings {
		if f.Source == "copilot" && (g.Commit == "" || f.At.Before(g.ReviewedAt)) {
			g.Commit, g.ReviewedAt = f.Commit, f.At
		}
	}
	if g.Commit == "" {
		for _, r := range p.Reviews {
			if g.Commit == "" || r.SubmittedAt.Before(g.ReviewedAt) {
				g.Commit, g.ReviewedAt = r.Commit, r.SubmittedAt
			}
		}
	}
	return g
}

func Label(ctx context.Context, llm *ollama.Client, dir Dir, opt LabelOptions, log *slog.Logger) error {
	prs, err := dir.PRs()
	if err != nil {
		return err
	}
	stats := struct{ prs, rule, model, noreply, skipped int }{}
	defer func() {
		log.Info("labelling finished", "prs", stats.prs, "rule", stats.rule, "model", stats.model, "no_reply", stats.noreply, "kept", stats.skipped)
	}()
	for _, p := range prs {
		if opt.Only != "" && p.Ref() != opt.Only {
			continue
		}
		existing, _ := dir.LoadGolden(p.Repo, p.Number)
		g := BuildGolden(p, existing)
		if !opt.Since.IsZero() && g.ReviewedAt.Before(opt.Since) {
			continue
		}
		stats.prs++
		for i := range g.Findings {
			f := &g.Findings[i]
			if f.Source != "copilot" || f.Locked || (f.Verdict != "" && !opt.Relabel) {
				stats.skipped++
				continue
			}
			switch {
			case strings.TrimSpace(f.Reply) == "":
				f.Verdict, f.Reason, f.Labelled = "unclear", "no reply", "rule:no-reply"
				stats.noreply++
			case commitRef.MatchString(f.Reply):
				f.Verdict, f.Reason, f.Labelled = "valid", "reply references a commit", "rule:commit"
				stats.rule++
			default:
				v, reason, err := labelOne(ctx, llm, f)
				if err != nil {
					log.Warn("label failed", "pr", g.Ref(), "thread", f.ID, "err", err)
					continue
				}
				f.Verdict, f.Reason, f.Labelled = v, reason, "model:"+llm.Model()
				stats.model++
			}
		}
		if err := dir.SaveGolden(g); err != nil {
			return err
		}
		log.Info("labelled", "pr", g.Ref(), "valid", g.Count("valid"), "invalid", g.Count("invalid"), "unclear", g.Count("unclear"), "reviewed", g.ReviewedAt.Format("2006-01-02"))
	}
	return nil
}

func labelOne(ctx context.Context, llm *ollama.Client, f *Finding) (string, string, error) {
	user := fmt.Sprintf("Reviewer comment at %s line %d:\n%s\n\nAuthor's reply:\n%s\n\nThread resolved: %v", f.Path, f.Line, truncate(f.Body, 3000), truncate(f.Reply, 3000), f.Resolved)
	raw, _, err := review.RunSingle(ctx, llm, labelPrompt, user, labelSchema)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("bad label output %q: %w", truncate(string(raw), 200), err)
	}
	switch out.Verdict {
	case "valid", "invalid", "unclear":
		return out.Verdict, out.Reason, nil
	}
	return "", "", fmt.Errorf("unexpected verdict %q", out.Verdict)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
