package poller

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
	"github.com/cameronpyne-smith/reviewdo/internal/github"
	"github.com/cameronpyne-smith/reviewdo/internal/state"
)

type statusAPI interface {
	CompareDiff(ctx context.Context, repo, base, head string) ([]byte, error)
	CreateStatus(ctx context.Context, repo, sha string, st github.CommitStatus) error
}

var errUnverifiable = errors.New("the change cannot be compared")

func transient(err error) bool {
	if errors.Is(err, errUnverifiable) {
		return false
	}
	var ae *github.APIError
	return !errors.As(err, &ae) || ae.Status >= 500 || ae.Status == 429
}

func changeKey(ctx context.Context, gh statusAPI, repo, base, head string) (string, error) {
	d, err := gh.CompareDiff(ctx, repo, base, head)
	if err != nil {
		return "", err
	}
	key, ok := diff.Fingerprint(d)
	if !ok {
		return "", errUnverifiable
	}
	return key, nil
}

func carryStatus(ctx context.Context, gh statusAPI, repo string, pull *github.Pull, ps *state.Pull, log *slog.Logger) {
	from := ps.Status
	head := pull.Head.SHA
	if from == nil || from.SHA == head || ps.CarryChecked == head {
		return
	}
	base := pull.Base.SHA
	if base == "" {
		base = pull.Base.Ref
	}
	plog := log.With("pr", pull.Number, "from", short(from.SHA), "to", short(head))
	reviewed, err := changeKey(ctx, gh, repo, base, from.SHA)
	if err == nil {
		var current string
		current, err = changeKey(ctx, gh, repo, base, head)
		if err == nil && current != reviewed {
			ps.CarryChecked = head
			plog.Info("status not carried forward: the change differs from the reviewed head")
			return
		}
	}
	if err != nil {
		if transient(err) {
			plog.Warn("could not compare with the reviewed head; will try again", "err", err)
			return
		}
		ps.CarryChecked = head
		plog.Info("status not carried forward", "reason", err)
		return
	}
	st := github.CommitStatus{State: from.State, Context: from.Context, Description: from.Description, TargetURL: from.TargetURL}
	if err := gh.CreateStatus(ctx, repo, head, st); err != nil {
		if transient(err) {
			plog.Warn("could not carry the commit status forward; will try again", "err", err)
			return
		}
		ps.CarryChecked = head
		plog.Warn("could not carry the commit status forward", "err", err)
		return
	}
	ps.CarryChecked = head
	ps.Status = &state.Status{SHA: head, Context: from.Context, State: from.State, Description: from.Description, TargetURL: from.TargetURL}
	plog.Info("status carried forward", "context", st.Context, "state", st.State)
}
