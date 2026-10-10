package review

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

type treeRepo map[string]string

func (r treeRepo) ReadFile(_ context.Context, p string) (string, error) {
	if s, ok := r[p]; ok {
		return s, nil
	}
	return "", errors.New("not found")
}
func (r treeRepo) Outline(ctx context.Context, p string) (string, error) { return r.ReadFile(ctx, p) }
func (r treeRepo) ListDir(_ context.Context, p string) (string, error) {
	seen := map[string]bool{}
	var out []string
	for k := range r {
		if p != "" && !strings.HasPrefix(k, p+"/") {
			continue
		}
		rest := strings.TrimPrefix(k, p+"/")
		if p == "" {
			rest = k
		}
		name, _, isDir := strings.Cut(rest, "/")
		if isDir {
			name += "/"
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return strings.Join(out, "\n"), nil
}
func (r treeRepo) Search(context.Context, string, string) (string, error)     { return "", nil }
func (r treeRepo) SearchBase(context.Context, string, string) (string, error) { return "", nil }
func (r treeRepo) Grep(_ context.Context, pattern string, max int) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range r {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		for i, l := range strings.Split(r[p], "\n") {
			if re.MatchString(l) && len(out) < max {
				out = append(out, fmt.Sprintf("%s:%d:%s", p, i+1, l))
			}
		}
	}
	return out, nil
}
func (r treeRepo) Definition(context.Context, string, string) (string, error) { return "", nil }
func (r treeRepo) References(context.Context, string, string) (string, error) { return "", nil }

func TestSiblingsReportsKeysMostSiblingsSet(t *testing.T) {
	repo := treeRepo{
		"apps/api-signalr/overlays/prod/patches/deployment-replicas.yaml":  "spec:\n  replicas: 2\n",
		"apps/api-baas/overlays/prod/patches/deployment-replicas.yaml":     "spec:\n  replicas: 2\n  template:\n    spec:\n      nodeSelector:\n        pool: prod\n",
		"apps/api-notifier/overlays/prod/patches/deployment-replicas.yaml": "spec:\n  replicas: 1\n  template:\n    spec:\n      nodeSelector:\n        pool: prod\n      tolerations: []\n",
		"apps/admin-ui/overlays/prod/patches/deployment-replicas.yaml":     "spec:\n  replicas: 1\n  template:\n    spec:\n      nodeSelector:\n        pool: prod\n",
		"apps/api-signalr/base/deployment.yaml":                            "kind: Deployment\n",
		"docs/readme.md":                                                   "# hi\n",
	}
	files := []*diff.File{
		{Path: "apps/api-signalr/overlays/prod/patches/deployment-replicas.yaml", Added: true},
		{Path: "docs/readme.md"},
	}
	out := Siblings(context.Background(), repo, files, SiblingSets(context.Background(), repo, files))
	for _, want := range []string{"3 sibling files with the same path under apps/<other>/", "api-baas, api-notifier", "nodeSelector (3 of 3)", "pool (3 of 3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tolerations") {
		t.Errorf("a key only one sibling sets was reported:\n%s", out)
	}
	if strings.Contains(out, "readme") {
		t.Errorf("file without siblings listed:\n%s", out)
	}
	if Siblings(context.Background(), repo, []*diff.File{{Path: "docs/readme.md"}}, nil) != "" {
		t.Error("expected empty output when nothing has siblings")
	}
}
