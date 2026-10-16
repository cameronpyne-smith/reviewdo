package review

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

type grepRepo struct{ treeRepo }

func (r grepRepo) Search(ctx context.Context, pattern, _ string) (string, error) {
	lines, err := r.Grep(ctx, pattern, 1000)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "(no matches)\n", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (r grepRepo) References(ctx context.Context, name, _ string) (string, error) {
	u := usesOfAll(ctx, r, []string{name})[name]
	if u.count == 0 {
		return "(no references)\n", nil
	}
	return fmt.Sprintf("%d references in %d files\n%s\n", u.count, len(u.files), strings.Join(u.files, "\n")), nil
}

func added(path string, lines ...string) *diff.File {
	f := &diff.File{Path: path, Hunks: []diff.Hunk{{}}}
	for i, l := range lines {
		kind := byte('+')
		if strings.HasPrefix(l, "-") || strings.HasPrefix(l, " ") {
			kind = l[0]
			l = l[1:]
		} else {
			l = strings.TrimPrefix(l, "+")
		}
		f.Hunks[0].Lines = append(f.Hunks[0].Lines, diff.Line{Kind: kind, Text: l, NewNo: i + 1})
	}
	return f
}

func TestPathCandidates(t *testing.T) {
	cases := map[string][]string{
		"See [the guide](docs/guide.md#setup) and [site](https://example.com/x.md).": {"docs/guide.md"},
		"resources:\n  - ../../base/deployment.yaml":                                   {"../../base/deployment.yaml"},
		"Edit `.github/workflows/ci.yml` then run ./scripts/run.sh":                     {".github/workflows/ci.yml", "./scripts/run.sh"},
		"import \"github.com/foo/bar/baz.go\" from k8s.io/api":                          nil,
		"GET /api/v1/users and mount /etc/config/app.yaml":                              nil,
		"image: ghcr.io/org/app.svc@sha256:abc and user@host/x.txt":                     nil,
		"glob src/**/*.ts and templates/{{ .Values.x }}.yaml":                           nil,
		"the repo remundo-xml/Remundo.Api":                                              nil,
	}
	for line, want := range cases {
		got := pathCandidates(line)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q: got %v want %v", line, got, want)
		}
	}
}

func TestMissingPathsResolvesFromRootAndFile(t *testing.T) {
	repo := grepRepo{treeRepo{
		"docs/guide.md":                 "x",
		"apps/web/base/deployment.yaml": "x",
		"apps/web/overlays/prod/kustomization.yaml": "x",
	}}
	files := []*diff.File{
		added("README.md", "See [guide](docs/guide.md) and [old](docs/old.md)."),
		added("apps/web/overlays/prod/kustomization.yaml", "+  - ../../base/deployment.yaml", "+  - ../../base/service.yaml"),
	}
	out := missingPaths(context.Background(), repo, files)
	for _, w := range []string{"README.md:1: docs/old.md does not exist", "kustomization.yaml:2: ../../base/service.yaml does not exist"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	for _, w := range []string{"guide.md does", "deployment.yaml does"} {
		if strings.Contains(out, w) {
			t.Errorf("existing path reported in:\n%s", out)
		}
	}
}

func TestMissingPathsSkipsImportsActionsAndGenerated(t *testing.T) {
	repo := treeRepo{
		"web/src/lib/types.ts":      "export type A = 1\n",
		"web/src/lib/util/index.ts": "export const u = 1\n",
		"docs/guide.md":             "# guide\n",
	}
	files := []*diff.File{
		added("web/src/lib/page.svelte",
			"+import type { A } from './types'",
			"+import { u } from './util'",
			"+import { gone } from './missing'",
			"+import 'flag-icons/css/flag-icons.min.css'",
			"+const x = fetch('./data/local.json')",
		),
		added(".github/workflows/ci.yml",
			"+    - uses: remundo-xml/Remundo.Actions/.github/workflows/test.yml@main",
			"+    - run: cat docs/missing.md",
		),
		added("web/.svelte-kit/generated/app.js", "+import './nodes/2'"),
	}
	out := missingPaths(context.Background(), repo, files)
	for _, want := range []string{"./missing", "./data/local.json", "docs/missing.md"} {
		if !strings.Contains(out, want+" does not exist") {
			t.Errorf("expected %s reported:\n%s", want, out)
		}
	}
	for _, skip := range []string{"./types", "./util", "flag-icons", "Remundo.Actions", "nodes/2"} {
		if strings.Contains(out, skip) {
			t.Errorf("%s should not be reported:\n%s", skip, out)
		}
	}
}

func TestRemovedNamesStillUsed(t *testing.T) {
	repo := grepRepo{treeRepo{
		".github/workflows/deploy.yml": "on:\n  workflow_dispatch:\n    inputs:\n      region:\n        type: string\nsteps:\n  - run: echo deploying ${{ inputs.region }} ${{ inputs.target_env }}\n",
		"docs/deploy.md":               "Pass target_env to choose the environment.\n",
	}}
	files := []*diff.File{added(".github/workflows/deploy.yml",
		"-      target_env:",
		"-        type: string",
		"+      region:",
		"+        type: string",
		"-        description: Deploy target",
		" steps:",
	)}
	out := removedNames(context.Background(), repo, files)
	if !strings.Contains(out, "target_env (removed from .github/workflows/deploy.yml): still used, 2 uses in 2 files") {
		t.Errorf("expected target_env reported:\n%s", out)
	}
	for _, w := range []string{"string", "description", "steps"} {
		if strings.Contains(out, "- "+w+" ") {
			t.Errorf("%s should not be reported:\n%s", w, out)
		}
	}
}

func TestNearMissesFindsDifferentSpelling(t *testing.T) {
	repo := grepRepo{treeRepo{
		"dashboards/rabbit.json": "{\"expr\": \"rabbitmq_acknowledged_total\"}\n{\"expr\": \"rabbitmq_queue_messages\"}\n",
		"exporter/metrics.go":    "const ackMetric = \"rabbitmq_acknowledgements_total\"\nconst depth = \"rabbitmq_queue_messages\"\n",
		"src/settings.cs":        "var x = Configuration[\"FeatureExportEnabled\"];\n",
		"src/Settings.json":      "{\"FeatureExportEnabled\": true}\n",
	}}
	files := []*diff.File{
		added("dashboards/rabbit.json", "{\"expr\": \"rabbitmq_acknowledged_total\"}", "{\"expr\": \"rabbitmq_queue_messages\"}"),
		added("src/new.cs", "var y = 1;", "var z = Configuration[\"feature_export_enable\"];"),
	}
	out := nearMisses(context.Background(), repo, files)
	if !strings.Contains(out, "rabbitmq_acknowledged_total (dashboards/rabbit.json:1): used nowhere else in the repository; it already uses rabbitmq_acknowledgements_total") {
		t.Errorf("expected the metric near miss:\n%s", out)
	}
	if !strings.Contains(out, "feature_export_enable (src/new.cs:2): used nowhere else in the repository; it already uses FeatureExportEnabled") {
		t.Errorf("expected the setting near miss:\n%s", out)
	}
	for _, w := range []string{"rabbitmq_queue_messages", "FeatureExportEnabled (src/new.cs:1)"} {
		if strings.Contains(out, w) {
			t.Errorf("%s is used elsewhere and should not be reported:\n%s", w, out)
		}
	}
}

func TestVariantsAndWords(t *testing.T) {
	got := strings.Join(variants("fooBarBaz"), " ")
	for _, w := range []string{"foo_bar_baz", "foo-bar-baz", "FOO_BAR_BAZ", "FooBarBaz", "foobarbaz"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	if strings.Contains(got, " fooBarBaz") {
		t.Errorf("original listed as a variant: %s", got)
	}
}

func TestEnvironmentsReportsSiblingNames(t *testing.T) {
	repo := grepRepo{treeRepo{
		"overlays/prod/config.yaml":    "x",
		"overlays/staging/config.yaml": "x",
		"overlays/dev/config.yaml":     "x",
	}}
	files := []*diff.File{added("overlays/prod/config.yaml", "note: copied from the staging cluster", "url: https://api.production.example", "env: prod")}
	sets := SiblingSets(context.Background(), repo, files)
	out := environments(files, sets)
	if !strings.Contains(out, `overlays/prod/config.yaml:1 names "staging", a sibling of prod/ under overlays/`) {
		t.Errorf("expected staging reported:\n%s", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected one line:\n%s", out)
	}
}

func TestChecksEmptyWhenNothingFound(t *testing.T) {
	repo := grepRepo{treeRepo{"a.go": "package a\n"}}
	files := []*diff.File{added("a.go", "func x() {}")}
	if out, _ := Checks(context.Background(), repo, files, nil); out != "" {
		t.Errorf("expected empty, got:\n%s", out)
	}
}
