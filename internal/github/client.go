package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	baseURL     = "https://api.github.com"
	userAgent   = "reviewdo (https://github.com/cameronpyne-smith/reviewdo)"
	acceptJSON  = "application/vnd.github+json"
	acceptDiff  = "application/vnd.github.diff"
	apiVersion  = "2022-11-28"
	perPage     = 100
	tokenMargin = 5 * time.Minute
)

type Client struct {
	http           *http.Client
	key            *rsa.PrivateKey
	appID          int64
	installationID int64

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

type APIError struct {
	Status  int
	Message string
	Errors  json.RawMessage
}

func (e *APIError) Error() string {
	if len(e.Errors) > 0 {
		return fmt.Sprintf("github: %d %s %s", e.Status, e.Message, e.Errors)
	}
	return fmt.Sprintf("github: %d %s", e.Status, e.Message)
}

func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

func New(key *rsa.PrivateKey, appID, installationID int64) *Client {
	return &Client{
		http:           &http.Client{Timeout: 60 * time.Second},
		key:            key,
		appID:          appID,
		installationID: installationID,
	}
}

func (c *Client) installationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExp) > tokenMargin {
		return c.token, nil
	}
	jwt, err := appJWT(c.key, c.appID, time.Now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", c.installationID)
	if err := c.request(ctx, http.MethodPost, path, "Bearer "+jwt, acceptJSON, nil, &out); err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	c.token, c.tokenExp = out.Token, out.ExpiresAt
	return c.token, nil
}

func (c *Client) request(ctx context.Context, method, path, auth, accept string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode}
		var msg struct {
			Message string          `json:"message"`
			Errors  json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Message != "" {
			ae.Message, ae.Errors = msg.Message, msg.Errors
		} else {
			ae.Message = strings.TrimSpace(string(data))
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			ae.Message += " (rate limit resets at " + resp.Header.Get("X-RateLimit-Reset") + ")"
		}
		return ae
	}
	switch v := out.(type) {
	case nil:
	case *[]byte:
		*v = data
	default:
		if len(data) > 0 {
			return json.Unmarshal(data, out)
		}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path, accept string, body any, out any) error {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	return c.request(ctx, method, path, "Bearer "+tok, accept, body, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, acceptJSON, nil, out)
}

func (c *Client) getDiff(ctx context.Context, path string) ([]byte, error) {
	var b []byte
	err := c.do(ctx, http.MethodGet, path, acceptDiff, nil, &b)
	return b, err
}

func paged(path string, page int, params url.Values) string {
	params.Set("per_page", strconv.Itoa(perPage))
	params.Set("page", strconv.Itoa(page))
	return path + "?" + params.Encode()
}

type App struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func (c *Client) App(ctx context.Context) (*App, error) {
	jwt, err := appJWT(c.key, c.appID, time.Now())
	if err != nil {
		return nil, err
	}
	var a App
	if err := c.request(ctx, http.MethodGet, "/app", "Bearer "+jwt, acceptJSON, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

type Repository struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

func (c *Client) InstallationRepositories(ctx context.Context) ([]Repository, error) {
	var all []Repository
	for page := 1; ; page++ {
		var out struct {
			Repositories []Repository `json:"repositories"`
		}
		if err := c.get(ctx, paged("/installation/repositories", page, url.Values{}), &out); err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		if len(out.Repositories) < perPage {
			return all, nil
		}
	}
}

type Ref struct {
	SHA string `json:"sha"`
	Ref string `json:"ref"`
}

type Pull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Draft     bool      `json:"draft"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      Ref       `json:"head"`
	Base      Ref       `json:"base"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (p *Pull) HasLabel(name string) bool {
	for _, l := range p.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

func (c *Client) ListOpenPulls(ctx context.Context, repo string) ([]Pull, error) {
	var all []Pull
	for page := 1; ; page++ {
		var out []Pull
		q := url.Values{"state": {"open"}, "sort": {"updated"}, "direction": {"desc"}}
		if err := c.get(ctx, paged("/repos/"+repo+"/pulls", page, q), &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < perPage {
			return all, nil
		}
	}
}

func (c *Client) Pull(ctx context.Context, repo string, number int) (*Pull, error) {
	var p Pull
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) PullDiff(ctx context.Context, repo string, number int) ([]byte, error) {
	return c.getDiff(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number))
}

type Compare struct {
	Status  string `json:"status"`
	Commits []struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	} `json:"commits"`
}

func (c *Client) Compare(ctx context.Context, repo, base, head string) (*Compare, error) {
	var out Compare
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/compare/%s...%s", repo, base, head), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CompareDiff(ctx context.Context, repo, base, head string) ([]byte, error) {
	return c.getDiff(ctx, fmt.Sprintf("/repos/%s/compare/%s...%s", repo, base, head))
}

type IssueComment struct {
	ID                int64     `json:"id"`
	Body              string    `json:"body"`
	HTMLURL           string    `json:"html_url"`
	IssueURL          string    `json:"issue_url"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	User              struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (ic *IssueComment) IssueNumber() int {
	i := strings.LastIndex(ic.IssueURL, "/")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(ic.IssueURL[i+1:])
	return n
}

func (c *Client) IssueCommentsSince(ctx context.Context, repo string, since time.Time) ([]IssueComment, error) {
	var all []IssueComment
	for page := 1; ; page++ {
		var out []IssueComment
		q := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "sort": {"updated"}, "direction": {"asc"}}
		if err := c.get(ctx, paged("/repos/"+repo+"/issues/comments", page, q), &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < perPage {
			return all, nil
		}
	}
}

func (c *Client) ReactToComment(ctx context.Context, repo string, commentID int64, content string) error {
	path := fmt.Sprintf("/repos/%s/issues/comments/%d/reactions", repo, commentID)
	return c.do(ctx, http.MethodPost, path, acceptJSON, map[string]string{"content": content}, nil)
}

func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, url.PathEscape(label))
	err := c.do(ctx, http.MethodDelete, path, acceptJSON, nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type ReviewRequest struct {
	CommitID string          `json:"commit_id"`
	Body     string          `json:"body"`
	Event    string          `json:"event"`
	Comments []ReviewComment `json:"comments,omitempty"`
}

type Review struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

func (c *Client) CreateReview(ctx context.Context, repo string, number int, req ReviewRequest) (*Review, error) {
	var out Review
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number)
	if err := c.do(ctx, http.MethodPost, path, acceptJSON, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
