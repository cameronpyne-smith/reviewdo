package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type graphQLError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": vars}
	if err := c.request(ctx, http.MethodPost, "/graphql", "Bearer "+tok, acceptJSON, body, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		var msgs []string
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

type SearchedPull struct {
	Repo         string    `json:"repo"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	Author       string    `json:"author"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"created_at"`
	MergedAt     time.Time `json:"merged_at"`
	BaseRef      string    `json:"base_ref"`
	BaseSHA      string    `json:"base_sha"`
	HeadSHA      string    `json:"head_sha"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
}

const searchPullsQuery = `query($q: String!, $after: String) {
  search(type: ISSUE, query: $q, first: 100, after: $after) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        number title state createdAt mergedAt baseRefName baseRefOid headRefOid additions deletions changedFiles
        author { login }
        repository { nameWithOwner }
      }
    }
  }
}`

func (c *Client) SearchPulls(ctx context.Context, query string, visit func(SearchedPull) error) (int, error) {
	var after any
	count := 0
	for {
		var out struct {
			Search struct {
				IssueCount int `json:"issueCount"`
				PageInfo   struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []struct {
					Number       int       `json:"number"`
					Title        string    `json:"title"`
					State        string    `json:"state"`
					CreatedAt    time.Time `json:"createdAt"`
					MergedAt     time.Time `json:"mergedAt"`
					BaseRefName  string    `json:"baseRefName"`
					BaseRefOid   string    `json:"baseRefOid"`
					HeadRefOid   string    `json:"headRefOid"`
					Additions    int       `json:"additions"`
					Deletions    int       `json:"deletions"`
					ChangedFiles int       `json:"changedFiles"`
					Author       struct {
						Login string `json:"login"`
					} `json:"author"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"nodes"`
			} `json:"search"`
		}
		if err := c.GraphQL(ctx, searchPullsQuery, map[string]any{"q": query, "after": after}, &out); err != nil {
			return count, err
		}
		for _, n := range out.Search.Nodes {
			if n.Number == 0 {
				continue
			}
			count++
			if err := visit(SearchedPull{
				Repo: n.Repository.NameWithOwner, Number: n.Number, Title: n.Title, Author: n.Author.Login, State: n.State,
				CreatedAt: n.CreatedAt, MergedAt: n.MergedAt, BaseRef: n.BaseRefName, BaseSHA: n.BaseRefOid, HeadSHA: n.HeadRefOid,
				Additions: n.Additions, Deletions: n.Deletions, ChangedFiles: n.ChangedFiles,
			}); err != nil {
				return count, err
			}
		}
		if !out.Search.PageInfo.HasNextPage || count >= 1000 {
			return out.Search.IssueCount, nil
		}
		after = out.Search.PageInfo.EndCursor
	}
}

type ThreadComment struct {
	ID              int64     `json:"id"`
	Author          string    `json:"author"`
	Body            string    `json:"body"`
	CreatedAt       time.Time `json:"created_at"`
	ReviewID        int64     `json:"review_id"`
	ReviewCommit    string    `json:"review_commit"`
	ReviewSubmitted time.Time `json:"review_submitted"`
}

type ReviewThread struct {
	ID           string          `json:"id"`
	Path         string          `json:"path"`
	Line         int             `json:"line"`
	OriginalLine int             `json:"original_line"`
	StartLine    int             `json:"start_line"`
	Resolved     bool            `json:"resolved"`
	Outdated     bool            `json:"outdated"`
	Comments     []ThreadComment `json:"comments"`
}

type PullReviewSummary struct {
	ID          int64     `json:"id"`
	Author      string    `json:"author"`
	State       string    `json:"state"`
	Commit      string    `json:"commit"`
	SubmittedAt time.Time `json:"submitted_at"`
	Body        string    `json:"body"`
}

type PullDetail struct {
	Body    string              `json:"body"`
	Reviews []PullReviewSummary `json:"reviews"`
	Threads []ReviewThread      `json:"threads"`
}

const pullDetailQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      body
      reviews(first: 100) {
        nodes { databaseId state submittedAt body author { login } commit { oid } }
      }
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id path line originalLine startLine isResolved isOutdated
          comments(first: 50) {
            nodes {
              databaseId body createdAt author { login }
              pullRequestReview { databaseId submittedAt commit { oid } }
            }
          }
        }
      }
    }
  }
}`

func (c *Client) PullDetail(ctx context.Context, repo string, number int) (*PullDetail, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("bad repo %q", repo)
	}
	var detail PullDetail
	var after any
	for {
		var out struct {
			Repository struct {
				PullRequest struct {
					Body    string `json:"body"`
					Reviews struct {
						Nodes []struct {
							DatabaseID  int64     `json:"databaseId"`
							State       string    `json:"state"`
							SubmittedAt time.Time `json:"submittedAt"`
							Body        string    `json:"body"`
							Author      struct {
								Login string `json:"login"`
							} `json:"author"`
							Commit struct {
								Oid string `json:"oid"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"reviews"`
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID           string `json:"id"`
							Path         string `json:"path"`
							Line         int    `json:"line"`
							OriginalLine int    `json:"originalLine"`
							StartLine    int    `json:"startLine"`
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Comments     struct {
								Nodes []struct {
									DatabaseID int64     `json:"databaseId"`
									Body       string    `json:"body"`
									CreatedAt  time.Time `json:"createdAt"`
									Author     struct {
										Login string `json:"login"`
									} `json:"author"`
									PullRequestReview struct {
										DatabaseID  int64     `json:"databaseId"`
										SubmittedAt time.Time `json:"submittedAt"`
										Commit      struct {
											Oid string `json:"oid"`
										} `json:"commit"`
									} `json:"pullRequestReview"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": name, "number": number, "after": after}
		if err := c.GraphQL(ctx, pullDetailQuery, vars, &out); err != nil {
			return nil, err
		}
		pr := out.Repository.PullRequest
		if after == nil {
			detail.Body = pr.Body
			for _, r := range pr.Reviews.Nodes {
				detail.Reviews = append(detail.Reviews, PullReviewSummary{ID: r.DatabaseID, Author: r.Author.Login, State: r.State, Commit: r.Commit.Oid, SubmittedAt: r.SubmittedAt, Body: r.Body})
			}
		}
		for _, t := range pr.ReviewThreads.Nodes {
			th := ReviewThread{ID: t.ID, Path: t.Path, Line: t.Line, OriginalLine: t.OriginalLine, StartLine: t.StartLine, Resolved: t.IsResolved, Outdated: t.IsOutdated}
			for _, cm := range t.Comments.Nodes {
				th.Comments = append(th.Comments, ThreadComment{
					ID: cm.DatabaseID, Author: cm.Author.Login, Body: cm.Body, CreatedAt: cm.CreatedAt,
					ReviewID: cm.PullRequestReview.DatabaseID, ReviewCommit: cm.PullRequestReview.Commit.Oid, ReviewSubmitted: cm.PullRequestReview.SubmittedAt,
				})
			}
			detail.Threads = append(detail.Threads, th)
		}
		if !pr.ReviewThreads.PageInfo.HasNextPage {
			return &detail, nil
		}
		after = pr.ReviewThreads.PageInfo.EndCursor
	}
}
