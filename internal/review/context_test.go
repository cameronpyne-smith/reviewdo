package review

import (
	"strings"
	"testing"

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
