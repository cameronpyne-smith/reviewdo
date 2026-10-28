package poller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/github"
	"github.com/cameronpyne-smith/reviewdo/internal/state"
)

type fakeStatusAPI struct {
	diffs     map[string]string
	errs      map[string]error
	statusErr error
	set       []string
	statuses  []github.CommitStatus
}

func (f *fakeStatusAPI) CompareDiff(_ context.Context, _, base, head string) ([]byte, error) {
	if err := f.errs[head]; err != nil {
		return nil, err
	}
	return []byte(f.diffs[head]), nil
}

func (f *fakeStatusAPI) CreateStatus(_ context.Context, _, sha string, st github.CommitStatus) error {
	if f.statusErr != nil {
		return f.statusErr
	}
	f.set = append(f.set, sha)
	f.statuses = append(f.statuses, st)
	return nil
}

const lockDiff = "diff --git a/package-lock.json b/package-lock.json\nindex 1..2 100644\n--- a/package-lock.json\n+++ b/package-lock.json\n@@ -5,3 +5,3 @@\n a\n-\"version\": \"1.0.0\"\n+\"version\": \"1.0.1\"\n c\n"

func carryFixture() (*github.Pull, *state.Pull) {
	pull := &github.Pull{Number: 7, Head: github.Ref{SHA: "new"}, Base: github.Ref{SHA: "main"}}
	ps := &state.Pull{Status: &state.Status{SHA: "old", Context: "review", State: "success", Description: "Ready to merge", TargetURL: "https://example.com/acme-org/acme-api/pull/7#review"}}
	return pull, ps
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestCarryStatusCopiesTheReviewedStatusToAHeadWithTheSameChange(t *testing.T) {
	pull, ps := carryFixture()
	moved := "diff --git a/package-lock.json b/package-lock.json\nindex 3..4 100644\n--- a/package-lock.json\n+++ b/package-lock.json\n@@ -90,3 +90,3 @@\n a\n-\"version\": \"1.0.0\"\n+\"version\": \"1.0.1\"\n c\n"
	gh := &fakeStatusAPI{diffs: map[string]string{"old": lockDiff, "new": moved}}
	carryStatus(context.Background(), gh, "acme-org/acme-api", pull, ps, quiet)
	if len(gh.set) != 1 || gh.set[0] != "new" {
		t.Fatalf("status set on %v", gh.set)
	}
	want := github.CommitStatus{State: "success", Context: "review", Description: "Ready to merge", TargetURL: "https://example.com/acme-org/acme-api/pull/7#review"}
	if gh.statuses[0] != want {
		t.Fatalf("got %+v", gh.statuses[0])
	}
	if ps.Status.SHA != "new" || ps.CarryChecked != "new" {
		t.Fatalf("state not updated: %+v", ps)
	}
	carryStatus(context.Background(), gh, "acme-org/acme-api", pull, ps, quiet)
	if len(gh.set) != 1 {
		t.Fatal("a head that already has the status should not be set again")
	}
}

func TestCarryStatusLeavesAHeadWithADifferentChange(t *testing.T) {
	pull, ps := carryFixture()
	gh := &fakeStatusAPI{diffs: map[string]string{"old": lockDiff, "new": lockDiff + "diff --git a/package.json b/package.json\n--- a/package.json\n+++ b/package.json\n@@ -1 +1 @@\n-x\n+y\n"}}
	carryStatus(context.Background(), gh, "acme-org/acme-api", pull, ps, quiet)
	if len(gh.set) != 0 || ps.Status.SHA != "old" || ps.CarryChecked != "new" {
		t.Fatalf("set %v, state %+v", gh.set, ps)
	}
}

func TestCarryStatusIsConservative(t *testing.T) {
	binary := "diff --git a/i.png b/i.png\nBinary files a/i.png and b/i.png differ\n"
	cases := map[string]struct {
		gh      *fakeStatusAPI
		checked string
	}{
		"binary":         {&fakeStatusAPI{diffs: map[string]string{"old": binary, "new": binary}}, "new"},
		"diff too large": {&fakeStatusAPI{diffs: map[string]string{"old": lockDiff}, errs: map[string]error{"new": &github.APIError{Status: 406}}}, "new"},
		"server error":   {&fakeStatusAPI{diffs: map[string]string{"old": lockDiff}, errs: map[string]error{"new": &github.APIError{Status: 502}}}, ""},
		"network error":  {&fakeStatusAPI{diffs: map[string]string{"new": lockDiff}, errs: map[string]error{"old": errors.New("connection reset")}}, ""},
		"status refused": {&fakeStatusAPI{diffs: map[string]string{"old": lockDiff, "new": lockDiff}, statusErr: &github.APIError{Status: 403}}, "new"},
	}
	for name, c := range cases {
		pull, ps := carryFixture()
		carryStatus(context.Background(), c.gh, "acme-org/acme-api", pull, ps, quiet)
		if len(c.gh.set) != 0 || ps.Status.SHA != "old" || ps.CarryChecked != c.checked {
			t.Errorf("%s: set %v, state %+v", name, c.gh.set, ps)
		}
	}
}

func TestCarryStatusNeedsAStatusItSet(t *testing.T) {
	pull, _ := carryFixture()
	gh := &fakeStatusAPI{diffs: map[string]string{"old": lockDiff, "new": lockDiff}}
	ps := &state.Pull{}
	carryStatus(context.Background(), gh, "acme-org/acme-api", pull, ps, quiet)
	if len(gh.set) != 0 || ps.CarryChecked != "" {
		t.Fatalf("set %v, state %+v", gh.set, ps)
	}
}
