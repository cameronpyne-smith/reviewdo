package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/review"
)

const dedupePrompt = `You are given the findings one automated reviewer left on a single pull request. Group the findings that report the same problem: the same mistake repeated at several places, a comment that says "same as above" or "as noted earlier", or the same remark made on several files that share one change. Findings that touch the same lines but describe different problems are not a group. A finding that stands alone belongs to no group.

Reply with JSON only: {"groups": [["<id>", "<id>", ...], ...]}. List each finding in at most one group, and only groups of two or more.`

var dedupeSchema = json.RawMessage(`{"type":"object","properties":{"groups":{"type":"array","items":{"type":"array","items":{"type":"string"}}}},"required":["groups"]}`)

type DedupeOptions struct {
	Since time.Time
	Only  string
}

func Dedupe(ctx context.Context, llm *ollama.Client, dir Dir, opt DedupeOptions, log *slog.Logger) error {
	goldens, err := dir.Goldens()
	if err != nil {
		return err
	}
	for _, g := range goldens {
		if opt.Only != "" && g.Ref() != opt.Only {
			continue
		}
		if !opt.Since.IsZero() && g.ReviewedAt.Before(opt.Since) {
			continue
		}
		var scored []*Finding
		for i := range g.Findings {
			f := &g.Findings[i]
			if f.Source != "copilot" || f.Commit != g.Commit || (f.Verdict != "valid" && f.Verdict != "invalid") {
				continue
			}
			scored = append(scored, f)
		}
		if len(scored) < 2 {
			continue
		}
		groups, err := dedupeOne(ctx, llm, scored)
		if err != nil {
			log.Warn("dedupe failed", "pr", g.Ref(), "err", err)
			continue
		}
		dups := 0
		for _, f := range scored {
			if !f.Locked {
				f.Dup = ""
			}
		}
		for _, members := range groups {
			first := members[0]
			for _, f := range members[1:] {
				if f.Locked || f.Verdict != first.Verdict {
					continue
				}
				f.Dup = first.ID
				dups++
			}
		}
		if err := dir.SaveGolden(g); err != nil {
			return err
		}
		log.Info("deduplicated", "pr", g.Ref(), "findings", len(scored), "duplicates", dups, "groups", len(groups))
	}
	return nil
}

func dedupeOne(ctx context.Context, llm *ollama.Client, findings []*Finding) ([][]*Finding, error) {
	var b strings.Builder
	for i, f := range findings {
		fmt.Fprintf(&b, "\n[R%d] %s line %d\n%s\n", i+1, f.Path, f.Line, truncate(f.Body, 1500))
	}
	raw, _, err := review.RunSingle(ctx, llm, dedupePrompt, b.String(), dedupeSchema)
	if err != nil {
		return nil, err
	}
	var out struct {
		Groups [][]string `json:"groups"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bad dedupe output %q: %w", truncate(string(raw), 200), err)
	}
	used := map[int]bool{}
	var groups [][]*Finding
	for _, ids := range out.Groups {
		var members []*Finding
		for _, id := range ids {
			i := index(id, "R")
			if i < 0 || i >= len(findings) || used[i] {
				continue
			}
			used[i] = true
			members = append(members, findings[i])
		}
		if len(members) > 1 {
			groups = append(groups, members)
		}
	}
	return groups, nil
}

func groupGolden(golden []Finding) [][]Finding {
	byID := map[string]int{}
	var groups [][]Finding
	for _, f := range golden {
		if f.Dup == "" {
			byID[f.ID] = len(groups)
			groups = append(groups, []Finding{f})
		}
	}
	for _, f := range golden {
		if f.Dup == "" {
			continue
		}
		if i, ok := byID[f.Dup]; ok && groups[i][0].Verdict == f.Verdict {
			groups[i] = append(groups[i], f)
		} else {
			groups = append(groups, []Finding{f})
		}
	}
	return groups
}
