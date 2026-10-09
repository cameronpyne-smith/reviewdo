package review

import (
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

func TestRuleSourceRequiresAVerbatimQuote(t *testing.T) {
	docs := []Instruction{
		{Source: "a.md", Text: "- **Imports**: Always use the `@src` alias for imports within the `src` folder (e.g., `@src/common/V3/Button.svelte`)."},
		{Source: "b.md", Text: "| `Map` | `SvelteMap` |\n\nNever call importOriginal inside a vi.mock factory."},
	}
	cases := map[string]string{
		"Always use the @src alias for imports within the src folder":        "a.md",
		"always use the `@src` alias for imports\nwithin the `src` folder":   "a.md",
		"Never call importOriginal inside a vi.mock factory.":                "b.md",
		"Use the @src alias everywhere":                                      "",
		"Imports must always use the @src alias for imports within the repo": "",
		"Map SvelteMap": "",
	}
	for rule, want := range cases {
		if got := ruleSource(rule, docs); got != want {
			t.Errorf("%q: got %q want %q", rule, got, want)
		}
	}
}

func TestRulesChunksListAddedAndContextLines(t *testing.T) {
	files := []*diff.File{
		{Path: "a.ts", Hunks: []diff.Hunk{{Lines: []diff.Line{
			{Kind: ' ', NewNo: 1, Text: "import x from './x'"},
			{Kind: '-', OldNo: 2, Text: "old"},
			{Kind: '+', NewNo: 2, Text: "import y from '../y'"},
		}}}},
		{Path: "gone.ts", Deleted: true, Hunks: []diff.Hunk{{Lines: []diff.Line{{Kind: '-', OldNo: 1, Text: "x"}}}}},
		{Path: "package-lock.json", Hunks: []diff.Hunk{{Lines: []diff.Line{{Kind: '+', NewNo: 1, Text: "x"}}}}},
	}
	chunks := rulesChunks(files, []string{"package-lock.json"})
	if len(chunks) != 1 || len(chunks[0]) != 1 {
		t.Fatalf("got %d chunks: %v", len(chunks), chunks)
	}
	got := chunks[0][0]
	for _, want := range []string{"### a.ts", "     1: import x from './x'", "+    2: import y from '../y'"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "old") {
		t.Error("removed line listed")
	}
	big := &diff.File{Path: "big.ts"}
	for i := 0; i < 3000; i++ {
		big.Hunks = append(big.Hunks, diff.Hunk{Lines: []diff.Line{{Kind: '+', NewNo: i + 1, Text: strings.Repeat("y", 30)}}})
	}
	chunks = rulesChunks([]*diff.File{files[0], big, files[0]}, nil)
	if len(chunks) < 2 {
		t.Errorf("large diff not split: %d chunks", len(chunks))
	}
}

func TestAddViolationsSkipsNearbyFindings(t *testing.T) {
	comments := []Comment{{Path: "a.ts", Line: 10, Body: "x"}}
	v := []Violation{
		{Comment: Comment{Path: "a.ts", Line: 11, Body: "dup"}},
		{Comment: Comment{Path: "a.ts", Line: 40, Body: "new"}},
		{Comment: Comment{Path: "b.ts", Line: 10, Body: "other file"}},
	}
	out, added := AddViolations(comments, v)
	if added != 2 || len(out) != 3 {
		t.Fatalf("added %d, total %d", added, len(out))
	}
}
