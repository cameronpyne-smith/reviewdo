package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/config"
	"github.com/cameronpyne-smith/reviewdo/internal/diff"
	"github.com/cameronpyne-smith/reviewdo/internal/github"
	"github.com/cameronpyne-smith/reviewdo/internal/gitrepo"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/review"
	"github.com/cameronpyne-smith/reviewdo/internal/state"
)

type Poller struct {
	cfg      *config.Config
	gh       *github.Client
	llm      *ollama.Client
	store    *gitrepo.Store
	st       *state.State
	fresh    bool
	botSlug  string
	botLogin string
	mention  *regexp.Regexp
	log      *slog.Logger
	DryRun   bool
}

var trustedAssociations = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

func New(cfg *config.Config, gh *github.Client, llm *ollama.Client, st *state.State, fresh bool, botSlug string, log *slog.Logger) *Poller {
	var store *gitrepo.Store
	if cfg.CloneDir != "" {
		store = &gitrepo.Store{Dir: cfg.CloneDir, Token: gh.Token}
	}
	return &Poller{
		cfg:      cfg,
		gh:       gh,
		llm:      llm,
		store:    store,
		st:       st,
		fresh:    fresh,
		botSlug:  botSlug,
		botLogin: botSlug + "[bot]",
		mention:  regexp.MustCompile(`(?i)(^|\s)@` + regexp.QuoteMeta(botSlug) + `\s+review\b`),
		log:      log,
	}
}

func (p *Poller) Run(ctx context.Context) error {
	t := time.NewTicker(p.cfg.PollInterval.Duration)
	defer t.Stop()
	for {
		p.tick(ctx)
		p.fresh = false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	for _, repo := range p.cfg.Repos {
		if ctx.Err() != nil {
			return
		}
		if err := p.pollRepo(ctx, repo); err != nil {
			p.log.Error("poll failed", "repo", repo.Name, "err", err)
		}
		if err := p.st.Save(p.cfg.StatePath); err != nil {
			p.log.Error("save state failed", "err", err)
		}
	}
}

type trigger struct {
	reason    string
	commentID int64
}

func (p *Poller) pollRepo(ctx context.Context, repo config.Repo) error {
	log := p.log.With("repo", repo.Name)
	pulls, err := p.gh.ListOpenPulls(ctx, repo.Name)
	if err != nil {
		return fmt.Errorf("list pulls: %w", err)
	}
	rs, known := p.st.Repo(repo.Name)
	baseline := !known
	if baseline {
		rs.CommentsSince = time.Now()
		if p.fresh {
			log.Info("first run: existing open pull requests will not be auto-reviewed, use a comment or label to review them", "open", len(pulls))
		}
	}

	open := map[int]*github.Pull{}
	for i := range pulls {
		open[pulls[i].Number] = &pulls[i]
	}
	for key := range rs.Pulls {
		n, _ := strconv.Atoi(key)
		if _, ok := open[n]; !ok {
			delete(rs.Pulls, key)
		}
	}

	triggers, err := p.commentTriggers(ctx, repo.Name, rs, open)
	if err != nil {
		log.Error("comment scan failed", "err", err)
	}

	for _, pull := range pulls {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		key := strconv.Itoa(pull.Number)
		ps := rs.Pulls[key]
		if ps == nil {
			ps = &state.Pull{Baselined: baseline && !pull.Draft}
			rs.Pulls[key] = ps
		}
		ps.HeadSHA = pull.Head.SHA

		var tr trigger
		switch {
		case triggers[pull.Number].commentID != 0:
			tr = triggers[pull.Number]
		case pull.HasLabel(p.cfg.Label) && ps.LabelSHA != pull.Head.SHA:
			tr = trigger{reason: "label"}
		case ps.ReviewedSHA == "" && !ps.Baselined && !pull.Draft:
			tr = trigger{reason: "opened"}
		default:
			continue
		}

		if tr.reason == "label" {
			ps.LabelSHA = pull.Head.SHA
		}

		plog := log.With("pr", pull.Number, "reason", tr.reason, "head", short(pull.Head.SHA))
		plog.Info("reviewing")
		start := time.Now()
		err := p.Review(ctx, repo, &pull, ps.ReviewedSHA)
		if ctx.Err() != nil {
			plog.Info("review interrupted by shutdown; it will run again on the next start", "took", time.Since(start).Round(time.Second))
			return ctx.Err()
		}
		if tr.reason == "label" && !p.DryRun {
			if err := p.gh.RemoveLabel(ctx, repo.Name, pull.Number, p.cfg.Label); err != nil {
				log.Warn("could not remove label; the app may need Issues write permission", "pr", pull.Number, "err", err)
			}
		}
		if err != nil {
			plog.Error("review failed", "err", err)
			continue
		}
		ps.ReviewedSHA = pull.Head.SHA
		ps.ReviewedAt = time.Now()
		plog.Info("review posted", "took", time.Since(start).Round(time.Second))
		if err := p.st.Save(p.cfg.StatePath); err != nil {
			log.Error("save state failed", "err", err)
		}
	}
	return nil
}

func (p *Poller) commentTriggers(ctx context.Context, repo string, rs *state.Repo, open map[int]*github.Pull) (map[int]trigger, error) {
	out := map[int]trigger{}
	comments, err := p.gh.IssueCommentsSince(ctx, repo, rs.CommentsSince)
	if err != nil {
		return out, err
	}
	for _, c := range comments {
		if c.UpdatedAt.After(rs.CommentsSince) {
			rs.CommentsSince = c.UpdatedAt
		}
		if c.ID <= rs.LastCommentID {
			continue
		}
		rs.LastCommentID = c.ID
		if c.User.Type == "Bot" || c.User.Login == p.botLogin || !p.mention.MatchString(c.Body) {
			continue
		}
		n := c.IssueNumber()
		if _, ok := open[n]; !ok {
			continue
		}
		if !trustedAssociations[c.AuthorAssociation] {
			p.log.Warn("ignoring review request from untrusted commenter", "repo", repo, "pr", n, "user", c.User.Login, "association", c.AuthorAssociation)
			continue
		}
		out[n] = trigger{reason: "comment", commentID: c.ID}
		if !p.DryRun {
			if err := p.gh.ReactToComment(ctx, repo, c.ID, "eyes"); err != nil {
				p.log.Warn("could not react to comment", "repo", repo, "pr", n, "err", err)
			}
		}
	}
	return out, nil
}

type Outcome struct {
	Result   *review.Result
	Output   review.Output
	Files    []*diff.File
	Scope    review.Scope
	Rejected []review.Comment
	Stats    review.Stats
	Parts    int
	Took     time.Duration
	Empty    bool
	DiffOnly bool
}

func (p *Poller) Review(ctx context.Context, repoCfg config.Repo, pull *github.Pull, previousSHA string) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Review.Timeout.Duration)
	defer cancel()
	log := p.log.With("pr", pull.Number)
	oc, err := p.ReviewAt(ctx, repoCfg, pull, pull.Head.SHA, pull.Base.Ref, previousSHA, log)
	if err != nil {
		return err
	}
	if oc.Empty {
		log.Info("nothing reviewable in diff")
		if p.DryRun {
			return nil
		}
		_, err := p.gh.CreateReview(ctx, repoCfg.Name, pull.Number, github.ReviewRequest{
			CommitID: pull.Head.SHA,
			Event:    "COMMENT",
			Body:     fmt.Sprintf("## 🟢 Ready to merge ✅\n\nNo reviewable changes %s. All changed files are ignored or binary.", oc.Scope),
		})
		return err
	}
	out := oc.Output
	if p.DryRun {
		fmt.Printf("=== %s#%d (%s) ===\n\n%s\n", repoCfg.Name, pull.Number, oc.Scope, out.Body)
		for _, c := range out.Comments {
			fmt.Printf("--- %s:%d\n%s\n\n", c.Path, c.Line, c.Body)
		}
		return nil
	}

	req := github.ReviewRequest{CommitID: pull.Head.SHA, Body: out.Body, Event: "COMMENT", Comments: out.Comments}
	rv, err := p.gh.CreateReview(ctx, repoCfg.Name, pull.Number, req)
	if github.IsStatus(err, http.StatusUnprocessableEntity) && len(out.Comments) > 0 {
		log.Warn("inline comments rejected, posting them in the review body", "err", err)
		req.Comments = nil
		req.Body = review.FoldComments(out)
		rv, err = p.gh.CreateReview(ctx, repoCfg.Name, pull.Number, req)
	}
	if err != nil {
		return fmt.Errorf("post review: %w", err)
	}
	log.Info("review url", "url", rv.HTMLURL, "inline", len(req.Comments))
	return nil
}

func mustRead(group []*diff.File, ignore []string, max int) []string {
	type sized struct {
		path  string
		lines int
	}
	var files []sized
	for _, f := range group {
		if f.Added || f.Deleted || f.Binary || diff.Ignored(f.Path, ignore) {
			continue
		}
		n := 0
		for _, h := range f.Hunks {
			n += len(h.Lines)
		}
		files = append(files, sized{f.Path, n})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].lines > files[j].lines })
	var out []string
	for i, f := range files {
		if i >= max {
			break
		}
		out = append(out, f.path)
	}
	return out
}

func (p *Poller) ReviewAt(ctx context.Context, repoCfg config.Repo, pull *github.Pull, head, base, previousSHA string, log *slog.Logger) (*Outcome, error) {
	var fullDiff []byte
	var err error
	if head == pull.Head.SHA {
		fullDiff, err = p.gh.PullDiff(ctx, repoCfg.Name, pull.Number)
	} else {
		fullDiff, err = p.gh.CompareDiff(ctx, repoCfg.Name, base, head)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch diff: %w", err)
	}
	files := diff.Parse(fullDiff)
	promptFiles := files
	scope := review.Scope{ToSHA: head}
	if previousSHA != "" && previousSHA != head {
		if inc := p.incrementalDiff(ctx, repoCfg.Name, previousSHA, head); inc != nil {
			promptFiles = inc
			scope = review.Scope{Incremental: true, FromSHA: previousSHA, ToSHA: head}
		}
	}

	var repo *gitrepo.Repo
	if p.store != nil {
		repo, err = p.store.Ensure(ctx, repoCfg.Name, pull.Number, head, base)
		if err != nil {
			log.Warn("repository unavailable, reviewing from the diff alone", "err", err)
		}
	}
	var changed, promptPaths []string
	for _, f := range files {
		changed = append(changed, f.Path)
	}
	for _, f := range promptFiles {
		promptPaths = append(promptPaths, f.Path)
	}
	var repoInstructions []review.Instruction
	var layout string
	if repo != nil {
		layout = review.Layout(ctx, repo, promptPaths)
		repoInstructions = review.RepoInstructions(ctx, repo, repo.Base(), changed)
		for _, ins := range repoInstructions {
			log.Debug("using repository instructions", "file", ins.Source, "bytes", len(ins.Text))
		}
	}
	guidance := review.RenderInstructions(p.cfg.Instructions, repoCfg.Instructions, repoInstructions)
	header := review.Header(repoCfg.Name, pull)
	lim := review.Limits{MaxToolCalls: p.cfg.Review.MaxToolCalls, MaxOutput: p.cfg.Review.MaxOutput}

	groups := review.Groups(promptFiles, p.cfg.Review.Ignore, p.cfg.Review.PartBytes)
	if len(groups) == 0 {
		return &Outcome{Files: files, Scope: scope, Empty: true}, nil
	}

	var parts []*review.Result
	var total review.Stats
	start := time.Now()
	budget := p.cfg.Review.TimeBudget.Duration
	reserve := budget * 2 / 5
	perPart := (budget - reserve) / time.Duration(len(groups))
	hard := start.Add(p.cfg.Review.Timeout.Duration)
	if d, ok := ctx.Deadline(); ok && d.Before(hard) {
		hard = d
	}
	for i, group := range groups {
		partStart := time.Now()
		lim.Deadline = start.Add(perPart * time.Duration(i+1))
		if floor := partStart.Add(p.cfg.Review.PartTime.Duration); floor.After(lim.Deadline) {
			lim.Deadline = floor
		}
		if last := hard.Add(-reserve); lim.Deadline.After(last) {
			lim.Deadline = last
		}
		lim.Deadline = lim.Deadline.Add(-20 * time.Second)
		lim.MustRead = mustRead(group, p.cfg.Review.Ignore, 10)
		prompt := review.BuildPrompt(review.Input{
			Repo:     repoCfg.Name,
			Pull:     pull,
			Guidance: guidance,
			Scope:    scope,
			Layout:   layout,
			Files:    group,
			AllFiles: promptPaths,
			Ignore:   p.cfg.Review.Ignore,
			MaxBytes: p.cfg.Review.MaxDiffBytes,
		})
		log.Debug("prompt built", "part", i+1, "of", len(groups), "files", len(prompt.Shown), "bytes", len(prompt.Text))
		var res *review.Result
		var st review.Stats
		if repo != nil {
			res, st, err = review.RunAgent(ctx, p.llm, repo, review.SystemPrompt, prompt.Text, lim, log.With("part", i+1))
		} else {
			var raw json.RawMessage
			raw, st, err = review.RunSingle(ctx, p.llm, review.SystemPrompt, prompt.Text, review.Schema)
			if err == nil {
				res, err = review.ParseResult(raw)
			}
		}
		if err != nil {
			total.Merge(st)
			log.Warn("part failed, continuing without it", "part", i+1, "of", len(groups), "err", err)
			continue
		}
		total.Merge(st)
		log.Info("part reviewed", "part", i+1, "of", len(groups), "verdict", res.Verdict, "findings", len(res.Comments), "rounds", st.Rounds, "tool_calls", st.ToolCalls, "took", st.Duration.Round(time.Second))
		parts = append(parts, res)
	}
	if len(parts) == 0 {
		return nil, errors.New("every part of the review failed")
	}
	res := review.Merge(parts)
	var rejected []review.Comment
	adjusted := false

	if repo != nil && p.cfg.Review.Verify != nil && *p.cfg.Review.Verify {
		fileDiffs := map[string]string{}
		for _, f := range files {
			fileDiffs[f.Path] = f.Render()
		}
		var kept []review.Comment
		rejected = nil
		unverified := 0
		verifyDeadline := start.Add(budget)
		if floor := time.Now().Add(reserve); floor.After(verifyDeadline) {
			verifyDeadline = floor
		}
		if verifyDeadline.After(hard) {
			verifyDeadline = hard
		}
		verifyDeadline = verifyDeadline.Add(-20 * time.Second)
		for _, c := range res.Comments {
			if c.Severity != "critical" && c.Severity != "major" {
				kept = append(kept, c)
				continue
			}
			if time.Now().After(verifyDeadline) {
				log.Warn("no time left to verify, dropping", "path", c.Path, "line", c.Line, "was", c.Severity)
				unverified++
				continue
			}
			v, st, err := review.VerifyFinding(ctx, p.llm, repo, header, c, fileDiffs[c.Path], review.Limits{MaxToolCalls: 12, MaxOutput: p.cfg.Review.MaxOutput / 2, Deadline: verifyDeadline}, log.With("verify", c.Path))
			total.Merge(st)
			if err != nil {
				log.Warn("verification failed, keeping finding", "path", c.Path, "line", c.Line, "err", err)
				kept = append(kept, c)
				continue
			}
			log.Info("verified finding", "path", c.Path, "line", c.Line, "was", c.Severity, "verdict", v.Verdict, "now", v.Severity, "reason", v.Reason, "took", st.Duration.Round(time.Second))
			if v.Verdict == "rejected" {
				rejected = append(rejected, c)
				continue
			}
			if v.Verdict == "downgraded" || v.Severity != c.Severity {
				adjusted = true
				c.Severity = v.Severity
				if strings.TrimSpace(v.Body) != "" {
					c.Body = v.Body
				}
			}
			kept = append(kept, c)
		}
		res.Comments = kept
		if unverified > 0 {
			res.Unverified = unverified
		}
	}

	if len(parts) > 1 || len(rejected) > 0 || adjusted {
		raw, st, err := review.RunSingle(ctx, p.llm, review.SynthesisPrompt, review.SynthesisInput(header, parts, res.Comments, rejected), review.SynthesisSchema)
		total.Merge(st)
		if err != nil {
			return nil, err
		}
		var syn struct {
			Verdict string `json:"verdict"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(raw, &syn); err != nil {
			return nil, fmt.Errorf("synthesis output invalid: %w", err)
		}
		res.Verdict, res.Summary = syn.Verdict, syn.Summary
	}
	log.Info("review complete", "model", p.llm.Model(), "parts", len(parts), "findings", len(res.Comments), "rounds", total.Rounds, "tool_calls", total.ToolCalls, "context_tokens", total.PromptTokens, "output_tokens", total.OutputTokens, "model_time", total.Duration.Round(time.Second), "took", time.Since(start).Round(time.Second))

	out := review.Render(res, files, scope, p.cfg.Review.MaxComments, p.botSlug, p.cfg.Label)
	return &Outcome{Result: res, Output: out, Files: files, Scope: scope, Rejected: rejected, Stats: total, Parts: len(parts), Took: time.Since(start), DiffOnly: repo == nil}, nil
}

func (p *Poller) incrementalDiff(ctx context.Context, repo, from, to string) []*diff.File {
	cmp, err := p.gh.Compare(ctx, repo, from, to)
	if err != nil {
		p.log.Info("compare unavailable, reviewing full diff", "repo", repo, "err", err)
		return nil
	}
	if cmp.Status != "ahead" {
		p.log.Info("history rewritten, reviewing full diff", "repo", repo, "status", cmp.Status)
		return nil
	}
	for _, c := range cmp.Commits {
		if len(c.Parents) > 1 {
			p.log.Info("merge commit since last review, reviewing full diff", "repo", repo, "commit", short(c.SHA))
			return nil
		}
	}
	d, err := p.gh.CompareDiff(ctx, repo, from, to)
	if err != nil {
		p.log.Info("compare diff unavailable, reviewing full diff", "repo", repo, "err", err)
		return nil
	}
	files := diff.Parse(d)
	if len(files) == 0 {
		return nil
	}
	return files
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func ParseRef(s string) (repo string, number int, err error) {
	repo, numStr, ok := strings.Cut(s, "#")
	if !ok || strings.Count(repo, "/") != 1 {
		return "", 0, errors.New("expected owner/repo#number")
	}
	number, err = strconv.Atoi(numStr)
	return repo, number, err
}
