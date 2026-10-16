package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/github"
)

type ExportOptions struct {
	Out     string
	Since   time.Time
	Refresh bool
}

type ExportedCommit struct {
	SHA     string    `json:"sha"`
	Date    time.Time `json:"date"`
	Author  string    `json:"author"`
	Message string    `json:"message"`
}

type ExportedPR struct {
	Repo         string                     `json:"repo"`
	Number       int                        `json:"number"`
	Title        string                     `json:"title"`
	Author       string                     `json:"author"`
	State        string                     `json:"state"`
	Draft        bool                       `json:"draft"`
	CreatedAt    time.Time                  `json:"created_at"`
	MergedAt     time.Time                  `json:"merged_at,omitempty"`
	ClosedAt     time.Time                  `json:"closed_at,omitempty"`
	BaseRef      string                     `json:"base_ref"`
	BaseSHA      string                     `json:"base_sha"`
	MergeBase    string                     `json:"merge_base,omitempty"`
	HeadSHA      string                     `json:"head_sha"`
	MergeCommit  string                     `json:"merge_commit,omitempty"`
	Additions    int                        `json:"additions"`
	Deletions    int                        `json:"deletions"`
	ChangedFiles int                        `json:"changed_files"`
	Body         string                     `json:"body"`
	Reviews      []github.PullReviewSummary `json:"reviews"`
	Threads      []github.ReviewThread      `json:"threads"`
	Commits      []ExportedCommit           `json:"commits"`
	TotalCommits int                        `json:"total_commits"`
	Diffs        map[string]string          `json:"diffs"`
	ExportedAt   time.Time                  `json:"exported_at"`
}

const listPullsQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: 50, after: $after, orderBy: {field: CREATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title state createdAt mergedAt closedAt isDraft
        author { login }
        baseRefName baseRefOid headRefOid
        mergeCommit { oid }
        additions deletions changedFiles
        commits(first: 100) {
          totalCount
          nodes { commit { oid committedDate messageHeadline author { name user { login } } parents(first: 1) { nodes { oid } } } }
        }
      }
    }
  }
}`

type listedPull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	MergedAt  time.Time `json:"mergedAt"`
	ClosedAt  time.Time `json:"closedAt"`
	IsDraft   bool      `json:"isDraft"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	BaseRefName string `json:"baseRefName"`
	BaseRefOid  string `json:"baseRefOid"`
	HeadRefOid  string `json:"headRefOid"`
	MergeCommit struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changedFiles"`
	Commits      struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Commit struct {
				Oid             string    `json:"oid"`
				CommittedDate   time.Time `json:"committedDate"`
				MessageHeadline string    `json:"messageHeadline"`
				Author          struct {
					Name string `json:"name"`
					User struct {
						Login string `json:"login"`
					} `json:"user"`
				} `json:"author"`
				Parents struct {
					Nodes []struct {
						Oid string `json:"oid"`
					} `json:"nodes"`
				} `json:"parents"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func Export(ctx context.Context, gh *github.Client, opt ExportOptions, log *slog.Logger) error {
	if opt.Out == "" {
		return errors.New("export: output directory is required")
	}
	repos, err := gh.InstallationRepositories(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opt.Out, 0o755); err != nil {
		return err
	}
	index, err := os.Create(filepath.Join(opt.Out, "index.jsonl"))
	if err != nil {
		return err
	}
	defer index.Close()
	w := bufio.NewWriter(index)
	defer w.Flush()
	enc := json.NewEncoder(w)
	total, fetched, failed := 0, 0, 0
	for _, r := range repos {
		owner, name, ok := strings.Cut(r.FullName, "/")
		if !ok {
			continue
		}
		var after any
		count := 0
	pages:
		for {
			var out struct {
				Repository struct {
					PullRequests struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []listedPull `json:"nodes"`
					} `json:"pullRequests"`
				} `json:"repository"`
			}
			err := retry(ctx, func() error {
				return gh.GraphQL(ctx, listPullsQuery, map[string]any{"owner": owner, "name": name, "after": after}, &out)
			})
			if err != nil {
				log.Warn("list failed", "repo", r.FullName, "err", err)
				failed++
				break
			}
			for _, lp := range out.Repository.PullRequests.Nodes {
				if lp.CreatedAt.Before(opt.Since) {
					break pages
				}
				count++
				total++
				p, err := exportOne(ctx, gh, r.FullName, lp, opt, log)
				if err != nil {
					log.Warn("export failed", "pr", fmt.Sprintf("%s#%d", r.FullName, lp.Number), "err", err)
					failed++
					continue
				}
				if p.ExportedAt.After(time.Now().Add(-time.Minute)) {
					fetched++
				}
				enc.Encode(indexRow(p))
			}
			if !out.Repository.PullRequests.PageInfo.HasNextPage {
				break
			}
			after = out.Repository.PullRequests.PageInfo.EndCursor
		}
		log.Info("repository exported", "repo", r.FullName, "prs", count, "archived", r.Archived)
	}
	log.Info("export done", "prs", total, "fetched", fetched, "failed", failed, "out", opt.Out)
	return nil
}

func exportOne(ctx context.Context, gh *github.Client, repo string, lp listedPull, opt ExportOptions, log *slog.Logger) (*ExportedPR, error) {
	dir := filepath.Join(opt.Out, filepath.FromSlash(repo), fmt.Sprint(lp.Number))
	path := filepath.Join(dir, "pr.json")
	if !opt.Refresh {
		var old ExportedPR
		if err := readJSON(path, &old); err == nil && old.HeadSHA == lp.HeadRefOid {
			return &old, nil
		}
	}
	p := &ExportedPR{
		Repo: repo, Number: lp.Number, Title: lp.Title, Author: lp.Author.Login, State: lp.State, Draft: lp.IsDraft,
		CreatedAt: lp.CreatedAt, MergedAt: lp.MergedAt, ClosedAt: lp.ClosedAt,
		BaseRef: lp.BaseRefName, BaseSHA: lp.BaseRefOid, HeadSHA: lp.HeadRefOid, MergeCommit: lp.MergeCommit.Oid,
		Additions: lp.Additions, Deletions: lp.Deletions, ChangedFiles: lp.ChangedFiles,
		TotalCommits: lp.Commits.TotalCount, Diffs: map[string]string{}, ExportedAt: time.Now(),
	}
	for _, n := range lp.Commits.Nodes {
		author := n.Commit.Author.User.Login
		if author == "" {
			author = n.Commit.Author.Name
		}
		p.Commits = append(p.Commits, ExportedCommit{SHA: n.Commit.Oid, Date: n.Commit.CommittedDate, Author: author, Message: n.Commit.MessageHeadline})
	}
	if len(lp.Commits.Nodes) > 0 && len(lp.Commits.Nodes[0].Commit.Parents.Nodes) > 0 {
		p.MergeBase = lp.Commits.Nodes[0].Commit.Parents.Nodes[0].Oid
	}
	var detail *github.PullDetail
	if err := retry(ctx, func() error {
		var err error
		detail, err = gh.PullDetail(ctx, repo, lp.Number)
		return err
	}); err != nil {
		return nil, fmt.Errorf("detail: %w", err)
	}
	p.Body, p.Reviews, p.Threads = detail.Body, detail.Reviews, detail.Threads
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var raw []byte
	if err := retry(ctx, func() error {
		var err error
		raw, err = gh.PullDiff(ctx, repo, lp.Number)
		return err
	}); err != nil {
		log.Warn("head diff unavailable", "pr", fmt.Sprintf("%s#%d", repo, lp.Number), "err", err)
	} else if err := os.WriteFile(filepath.Join(dir, "head.diff"), raw, 0o644); err != nil {
		return nil, err
	} else {
		p.Diffs[lp.HeadRefOid] = "head.diff"
	}
	base := p.MergeBase
	if base == "" {
		base = p.BaseSHA
	}
	for _, rv := range p.Reviews {
		if rv.Commit == "" || rv.Commit == lp.HeadRefOid {
			continue
		}
		if _, done := p.Diffs[rv.Commit]; done {
			continue
		}
		name := "review-" + rv.Commit[:7] + ".diff"
		var raw []byte
		if err := retry(ctx, func() error {
			var err error
			raw, err = gh.CompareDiff(ctx, repo, base, rv.Commit)
			return err
		}); err != nil {
			log.Warn("review diff unavailable", "pr", fmt.Sprintf("%s#%d", repo, lp.Number), "commit", rv.Commit[:7], "err", err)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			return nil, err
		}
		p.Diffs[rv.Commit] = name
	}
	if err := writeJSON(path, p); err != nil {
		return nil, err
	}
	return p, nil
}

func indexRow(p *ExportedPR) map[string]any {
	copilot := 0
	for _, r := range p.Reviews {
		if isCopilot(r.Author) {
			copilot++
		}
	}
	return map[string]any{
		"repo": p.Repo, "number": p.Number, "author": p.Author, "state": p.State, "created_at": p.CreatedAt,
		"head_sha": p.HeadSHA, "additions": p.Additions, "deletions": p.Deletions, "changed_files": p.ChangedFiles,
		"reviews": len(p.Reviews), "copilot_reviews": copilot, "threads": len(p.Threads), "diffs": len(p.Diffs),
	}
}

func retry(ctx context.Context, call func() error) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = call(); err == nil {
			return nil
		}
		var ae *github.APIError
		if !errors.As(err, &ae) || (ae.Status != 403 && ae.Status != 429 && ae.Status < 500) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Minute):
		}
	}
	return err
}
