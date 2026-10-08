package review

import (
	"encoding/json"
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
