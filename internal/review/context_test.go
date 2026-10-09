package review

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
)

func TestShrinkKeepsPromptAndRecentTurns(t *testing.T) {
	big := strings.Repeat("x", 3000)
	msgs := []ollama.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "prompt"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", Content: big},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", Content: big},
		{Role: "assistant", Content: "a3"},
		{Role: "tool", Content: "small"},
	}
	out := shrink(msgs, 1500)
	if out[0].Role != "system" || out[1].Role != "user" || out[1].Content != "prompt" {
		t.Fatalf("prompt not kept: %+v", out[:2])
	}
	if out[2].Role != "user" || !strings.Contains(out[2].Content, "removed") {
		t.Fatalf("expected note, got %+v", out[2])
	}
	if out[len(out)-1].Content != "small" || out[3].Role != "assistant" {
		t.Fatalf("unexpected tail: %+v", out[3:])
	}
	if approxTokens(out) > 1500 {
		t.Fatalf("still over budget: %d", approxTokens(out))
	}
}

func TestLineRange(t *testing.T) {
	text := "a\nb\nc\nd\n"
	if got := lineRange(text, 2, 3); !strings.HasPrefix(got, "lines 2-3 of 5:\nb\nc\n") {
		t.Errorf("got %q", got)
	}
	if got := lineRange(text, 0, 0); got != text {
		t.Errorf("whole file changed: %q", got)
	}
	if got := lineRange(text, 9, 0); !strings.Contains(got, "file has 5 lines") {
		t.Errorf("got %q", got)
	}
}

type bigRepo struct {
	fakeRepo
	text string
}

func (r bigRepo) ReadFile(_ context.Context, _ string) (string, error) { return r.text, nil }
func (r bigRepo) Outline(_ context.Context, _ string) (string, error) {
	return "10: func A()\n500: func B()\n", nil
}

func bigText(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", 40))
	}
	return b.String()
}

func TestReadFileCapsWholeReads(t *testing.T) {
	repo := bigRepo{text: bigText(2000)}
	changed := map[string][]Span{"big.cs": {{Start: 600, End: 610}, {Start: 1200, End: 1201}}}
	out, err := readFile(context.Background(), repo, "big.cs", 0, 0, changed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"big.cs has 2001 lines", "Outline:\n10: func A()", "lines 570-640 of 2001:", "line 570 ", "line 640 ", "lines 1170-1231 of 2001:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out[:min(len(out), 600)])
		}
	}
	if strings.Contains(out, "line 100 ") || strings.Contains(out, "line 1999 ") {
		t.Error("unchanged region included")
	}
	if len(out) > maxRegionBytes+4096 {
		t.Errorf("view too large: %d bytes", len(out))
	}
	out, _ = readFile(context.Background(), repo, "other.cs", 0, 0, changed)
	if !strings.Contains(out, "Below is its outline") || strings.Contains(out, "Changed regions") {
		t.Errorf("file outside the diff should show only the outline:\n%s", out[:min(len(out), 300)])
	}
	out, _ = readFile(context.Background(), repo, "big.cs", 5, 7, changed)
	if !strings.HasPrefix(out, "lines 5-7 of 2001:\nline 5 ") {
		t.Errorf("ranged read changed: %q", out[:60])
	}
	small := bigRepo{text: bigText(50)}
	out, _ = readFile(context.Background(), small, "small.cs", 0, 0, nil)
	if out != small.text {
		t.Error("small file should come back whole")
	}
}

func TestLargeFileViewOmitsRegionsBeyondTheCap(t *testing.T) {
	repo := bigRepo{text: bigText(20000)}
	var spans []Span
	for i := 100; i < 20000; i += 400 {
		spans = append(spans, Span{Start: i, End: i + 5})
	}
	out := largeFileView(context.Background(), repo, "huge.cs", repo.text, spans)
	if len(out) > maxRegionBytes+8192 {
		t.Errorf("view too large: %d bytes", len(out))
	}
	if !strings.Contains(out, "more changed regions omitted: lines") {
		t.Error("expected an omission note")
	}
}

func TestChangedSpans(t *testing.T) {
	files := []*diff.File{{Path: "a.go", Hunks: []diff.Hunk{
		{Lines: []diff.Line{{Kind: ' ', NewNo: 10}, {Kind: '-', OldNo: 11}, {Kind: '+', NewNo: 11}, {Kind: ' ', NewNo: 12}}},
		{Lines: []diff.Line{{Kind: '-', OldNo: 50}, {Kind: '-', OldNo: 51}}},
		{Lines: []diff.Line{{Kind: '+', NewNo: 90}, {Kind: '+', NewNo: 91}}},
	}}, {Path: "gone.go", Deleted: true, Hunks: []diff.Hunk{{Lines: []diff.Line{{Kind: '-', OldNo: 1}}}}}}
	got := ChangedSpans(files)
	want := map[string][]Span{"a.go": {{10, 12}, {90, 91}}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v want %v", got, want)
	}
}
