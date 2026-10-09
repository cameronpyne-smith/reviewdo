package review

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

type fakeRepo map[string]string

func (f fakeRepo) ReadFile(_ context.Context, p string) (string, error) {
	if s, ok := f[p]; ok {
		return s, nil
	}
	return "", errors.New("missing")
}
func (f fakeRepo) ListDir(context.Context, string) (string, error)            { return "", nil }
func (f fakeRepo) Search(context.Context, string, string) (string, error)     { return "", nil }
func (f fakeRepo) SearchBase(context.Context, string, string) (string, error) { return "", nil }

func TestAnchor(t *testing.T) {
	files := []*diff.File{{Path: "a.yaml", Hunks: []diff.Hunk{{Lines: []diff.Line{
		{Kind: '+', Text: "  replicas: 3", NewNo: 10},
		{Kind: '+', Text: "  image: nginx", NewNo: 11},
	}}}}}
	repo := fakeRepo{"a.yaml": "kind: Deployment\nname: x\n\n\n\n\n\n\n\n  replicas: 3\n  image: nginx\n  port: 80\n"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	in := []Comment{
		{Path: "a.yaml", Line: 10, Quote: "replicas: 3", Body: "ok as is"},
		{Path: "a.yaml", Line: 10, Quote: "image: nginx", Body: "moves to 11"},
		{Path: "a.yaml", Line: 5, Quote: "port: 80", Body: "moves to 12 from the file"},
		{Path: "a.yaml", Line: 10, Quote: "does not exist anywhere", Body: "dropped"},
		{Path: "a.yaml", Line: 10, Body: "no quote, kept"},
	}
	kept, dropped := Anchor(context.Background(), repo, files, in, log)
	if len(dropped) != 1 || dropped[0].Body != "dropped" {
		t.Fatalf("dropped = %+v", dropped)
	}
	want := map[string]int{"ok as is": 10, "moves to 11": 11, "moves to 12 from the file": 12, "no quote, kept": 10}
	if len(kept) != len(want) {
		t.Fatalf("kept = %+v", kept)
	}
	for _, c := range kept {
		if want[c.Body] != c.Line {
			t.Errorf("%s: line %d, want %d", c.Body, c.Line, want[c.Body])
		}
	}
}
