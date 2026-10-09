package review

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
)

const TriagePrompt = `You are the editor of an automated code review. You are given the pull request details and the draft findings. Decide for each finding whether it is worth the author's attention. You are not judging whether a finding is true; a separate check does that. You are judging whether it says anything.

Drop a finding when it:
- concludes there is no problem, or only describes or explains what the code does;
- asks the author to confirm, verify, check or double-check something, or says a value "may" or "might" be wrong without saying what would be wrong and why it matters;
- is generic advice that would apply to any codebase and is not tied to something specific in this change;
- praises, restates the description, or suggests an optional alternative with no concrete downside to the current code;
- is about formatting, whitespace, naming preference or anything a linter would catch;
- is the same issue as another finding at the same place.

Keep every finding that names a concrete problem at a concrete place, including the same issue reported at several places, and including anything you are unsure about. Keep is the default. Respond with JSON only: {"decisions":[{"id":"F1","keep":true,"reason":"one short sentence"}]} with one decision per finding.`

var TriageSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "decisions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "keep": {"type": "boolean"},
          "reason": {"type": "string"}
        },
        "required": ["id", "keep", "reason"]
      }
    }
  },
  "required": ["decisions"]
}`)

var hollow = regexp.MustCompile(`(?i)\b(no (issue|problem|action|change)s? (is |are )?(needed|required|here|found)|nothing to (fix|change)|looks (good|fine|correct)|is correct as written|works as intended)\b`)

func Hollow(body string) bool {
	return hollow.MatchString(body)
}

type Triaged struct {
	Kept    []Comment
	Dropped []Comment
	Reasons []string
}

func TriageInput(header string, comments []Comment) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\nDraft findings:\n")
	for i, c := range comments {
		fmt.Fprintf(&b, "\nF%d (%s) %s line %d:\n%s\n", i+1, c.Severity, c.Path, c.Line, strings.TrimSpace(c.Body))
	}
	return b.String()
}

func Triage(ctx context.Context, llm *ollama.Client, header string, comments []Comment) (Triaged, Stats, error) {
	var t Triaged
	var candidates []Comment
	for _, c := range comments {
		if Hollow(c.Body) {
			t.Dropped = append(t.Dropped, c)
			t.Reasons = append(t.Reasons, "rule: says there is no problem")
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return t, Stats{}, nil
	}
	raw, st, err := RunSingle(ctx, llm, TriagePrompt, TriageInput(header, candidates), TriageSchema)
	if err != nil {
		t.Kept = append(t.Kept, candidates...)
		return t, st, err
	}
	var out struct {
		Decisions []struct {
			ID     string `json:"id"`
			Keep   bool   `json:"keep"`
			Reason string `json:"reason"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Kept = append(t.Kept, candidates...)
		return t, st, fmt.Errorf("triage output invalid: %w", err)
	}
	drop := map[string]string{}
	for _, d := range out.Decisions {
		if !d.Keep {
			drop[strings.ToUpper(strings.TrimSpace(d.ID))] = d.Reason
		}
	}
	for i, c := range candidates {
		if reason, ok := drop[fmt.Sprintf("F%d", i+1)]; ok {
			t.Dropped = append(t.Dropped, c)
			t.Reasons = append(t.Reasons, reason)
			continue
		}
		t.Kept = append(t.Kept, c)
	}
	return t, st, nil
}
