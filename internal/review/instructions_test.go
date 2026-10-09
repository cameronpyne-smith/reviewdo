package review

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"cluster-state-management/argocd-project/{secrets,sealed-secrets}/**", "cluster-state-management/argocd-project/secrets/overlays/prod/kustomization.yaml", true},
		{"cluster-state-management/argocd-project/{secrets,sealed-secrets}/**", "cluster-state-management/argocd-project/apps/rabbitmq/base/kustomization.yaml", false},
		{"**/*.yaml", "a/b/c.yaml", true},
		{"**/*.yaml", "c.yaml", true},
		{"**", "anything/at/all", true},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/pkg/main.go", false},
		{"*.md", "docs/readme.md", false},
	}
	for _, c := range cases {
		if got := anyMatch(c.pat, []string{c.name}); got != c.want {
			t.Errorf("anyMatch(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestFrontmatter(t *testing.T) {
	applyTo, body := frontmatter("---\napplyTo: \"a/**,b/*.go\"\n---\n\n# Title\nbody\n")
	if applyTo != "a/**,b/*.go" {
		t.Errorf("applyTo = %q", applyTo)
	}
	if body != "\n# Title\nbody\n" {
		t.Errorf("body = %q", body)
	}
	applyTo, body = frontmatter("# No frontmatter\n")
	if applyTo != "" || body != "# No frontmatter\n" {
		t.Errorf("plain = %q %q", applyTo, body)
	}
}

func TestResultLenientFiles(t *testing.T) {
	var r Result
	err := json.Unmarshal([]byte(`{"verdict":"ready","summary":"ok","files":"not an array","comments":[]}`), &r)
	if err != nil || r.Summary != "ok" || len(r.Files) != 0 {
		t.Errorf("lenient files: err=%v r=%+v", err, r)
	}
	if err := json.Unmarshal([]byte(`{"verdict":"ready","summary":"ok","comments":"nope"}`), &r); err == nil {
		t.Errorf("bad comments should fail")
	}
}

func TestBudgetCapsTotalInstructionBytes(t *testing.T) {
	big := strings.Repeat("rule\n", 12000)
	docs := budget([]Instruction{{Source: "a", Text: big}, {Source: "b", Text: big}, {Source: "c", Text: "short"}})
	total := 0
	for _, d := range docs {
		total += len(d.Text)
	}
	if total > maxInstructionTotal+200 {
		t.Errorf("total %d over budget", total)
	}
	if len(docs[0].Text) < maxInstructionBytes-100 || !strings.Contains(docs[1].Text, "[truncated") {
		t.Errorf("unexpected clipping: %d, %d", len(docs[0].Text), len(docs[1].Text))
	}
	if len(docs) != 2 {
		t.Errorf("doc beyond the budget should be dropped, got %d docs", len(docs))
	}
	docs = budget([]Instruction{{Source: "a", Text: "short"}, {Source: "b", Text: big}, {Source: "c", Text: "tail"}})
	if len(docs) != 3 || docs[0].Text != "short" || docs[2].Text != "tail" {
		t.Errorf("small docs changed: %+v", docs)
	}
}
