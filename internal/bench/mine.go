package bench

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/github"
)

type MineOptions struct {
	Org     string
	Authors []string
	Refresh bool
}

func Mine(ctx context.Context, gh *github.Client, dir Dir, opt MineOptions, log *slog.Logger) error {
	seen, err := dir.seen()
	if err != nil {
		return err
	}
	stats := struct{ listed, fetched, kept, skipped int }{}
	defer func() {
		log.Info("mining finished", "listed", stats.listed, "fetched", stats.fetched, "with_copilot", stats.kept, "already_known", stats.skipped)
	}()
	for _, author := range opt.Authors {
		before := time.Time{}
		for {
			q := fmt.Sprintf("is:pr org:%s author:%s sort:created-desc", opt.Org, author)
			if !before.IsZero() {
				q += " created:<=" + before.Format("2006-01-02")
			}
			var oldest time.Time
			n := 0
			total, err := gh.SearchPulls(ctx, q, func(sp github.SearchedPull) error {
				n++
				stats.listed++
				if oldest.IsZero() || sp.CreatedAt.Before(oldest) {
					oldest = sp.CreatedAt
				}
				key := fmt.Sprintf("%s#%d", sp.Repo, sp.Number)
				if !opt.Refresh && (dir.HasPR(sp.Repo, sp.Number) || seen.m[key]) {
					stats.skipped++
					return nil
				}
				detail, err := fetchDetail(ctx, gh, sp.Repo, sp.Number)
				if err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				stats.fetched++
				p := toPR(sp, detail)
				if len(p.Reviews) == 0 {
					seen.m[key] = true
					if stats.fetched%25 == 0 {
						seen.save()
					}
					return nil
				}
				stats.kept++
				log.Info("mined", "pr", key, "copilot_reviews", len(p.Reviews), "threads", len(p.Threads))
				return dir.SavePR(p)
			})
			if err != nil {
				seen.save()
				return err
			}
			log.Info("search page done", "author", author, "query", q, "returned", n, "total", total)
			if n < 1000 || oldest.IsZero() || (!before.IsZero() && !oldest.Before(before)) {
				break
			}
			before = oldest
		}
	}
	return seen.save()
}

func toPR(sp github.SearchedPull, d *github.PullDetail) *PR {
	p := &PR{
		Repo: sp.Repo, Number: sp.Number, Title: sp.Title, Author: sp.Author, State: sp.State,
		CreatedAt: sp.CreatedAt, MergedAt: sp.MergedAt, BaseRef: sp.BaseRef, BaseSHA: sp.BaseSHA, HeadSHA: sp.HeadSHA,
		Additions: sp.Additions, Deletions: sp.Deletions, ChangedFiles: sp.ChangedFiles, Body: d.Body, MinedAt: time.Now(),
	}
	for _, r := range d.Reviews {
		if isCopilot(r.Author) {
			p.Reviews = append(p.Reviews, r)
		}
	}
	for _, t := range d.Threads {
		if len(t.Comments) > 0 && isCopilot(t.Comments[0].Author) {
			p.Threads = append(p.Threads, t)
		}
	}
	return p
}

func authorReplies(t github.ReviewThread) string {
	var parts []string
	for _, c := range t.Comments[1:] {
		if isCopilot(c.Author) {
			continue
		}
		parts = append(parts, fmt.Sprintf("@%s: %s", c.Author, strings.TrimSpace(c.Body)))
	}
	return strings.Join(parts, "\n\n")
}

func fetchDetail(ctx context.Context, gh *github.Client, repo string, number int) (*github.PullDetail, error) {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			}
		}
		var d *github.PullDetail
		d, err = gh.PullDetail(ctx, repo, number)
		if err == nil {
			return d, nil
		}
		var ae *github.APIError
		if !errors.As(err, &ae) || ae.Status < 500 {
			return nil, err
		}
	}
	return nil, err
}
