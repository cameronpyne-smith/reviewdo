package review

import (
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

func TestRenderFindingsSection(t *testing.T) {
	files := []*diff.File{{Path: "a.cs", Hunks: []diff.Hunk{{Lines: []diff.Line{{Kind: '+', Text: "x", NewNo: 10}}}}}}
	res := &Result{
		Summary: "Adds a thing.",
		Files:   []FileSummary{{Path: "a.cs", Description: "adds x"}},
		Comments: []Comment{
			{Path: "a.cs", Line: 10, Severity: "major", Title: "Handle returned messages", Body: "Inline body."},
			{Path: "a.cs", Line: 99, Severity: "minor", Body: "Dead code remains after the switch. Remove it."},
		},
	}
	out := Render(res, files, Scope{ToSHA: "abc1234"}, 15, "reviewdo-bot", "reviewdo")
	if len(out.Comments) != 1 || !strings.HasPrefix(out.Comments[0].Body, "**Major: Handle returned messages**") {
		t.Fatalf("inline comments = %+v", out.Comments)
	}
	for _, want := range []string{
		"## 🟡 Merge with care",
		"<summary><strong>2 findings</strong></summary>",
		"- **Major** · Handle returned messages · `a.cs:10`\n",
		"- **Minor** · Dead code remains after the switch · `a.cs:99`\n\n  Dead code remains after the switch. Remove it.\n",
		"<summary><strong>What changed</strong></summary>",
	} {
		if !strings.Contains(out.Body, want) {
			t.Errorf("body missing %q:\n%s", want, out.Body)
		}
	}
	bare := Render(&Result{Summary: "s"}, files, Scope{ToSHA: "abc1234"}, 15, "b", "l")
	if !strings.Contains(bare.Body, "| `a.cs` | +1 −0 |") {
		t.Errorf("expected file-name fallback rows:\n%s", bare.Body)
	}
	if strings.Contains(out.Body, "Inline body.") {
		t.Errorf("inline body should not be in the review body:\n%s", out.Body)
	}
	linked := out.Linked(map[string]int64{"a.cs:10": 42}, false)
	if !strings.Contains(linked, "[Handle returned messages](#discussion_r42)") {
		t.Errorf("linked body missing link:\n%s", linked)
	}
	folded := out.Linked(nil, true)
	if !strings.Contains(folded, "\n  Inline body.\n") {
		t.Errorf("folded body missing inline body:\n%s", folded)
	}
}
