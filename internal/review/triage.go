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
- is the same issue as another finding at the same place;
- rests on how a library, client or broker behaves (what a method throws, which interface a type implements, what happens on failure) without pointing at evidence in this repository.

A kept finding's body must speak to the author about the code. If it narrates what the reviewer did ("I searched", "I could not find", "the tools show"), keep the finding but set "body" to the same finding with that narration removed; otherwise omit "body".

Keep every finding that names a concrete problem at a concrete place, including the same issue reported at several places, and including anything you are unsure about. Keep is the default.

For each kept finding also set its severity from these definitions, ignoring the draft's own: critical means it will certainly break, lose data or open a security hole; major means a likely bug, or a clear mismatch between what the change says and what it does, or a stale statement that will mislead whoever relies on it; minor means worth fixing but not blocking; nit means optional. When unsure between two, choose the lower.

Respond with JSON only: {"decisions":[{"id":"F1","keep":true,"severity":"minor","reason":"one short sentence","body":"only when rewritten"}]} with one decision per finding.`

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
          "severity": {"type": "string", "enum": ["critical", "major", "minor", "nit"]},
          "reason": {"type": "string"},
          "body": {"type": "string"}
        },
        "required": ["id", "keep", "severity", "reason"]
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
	Reasons  []string
	Rerated  int
	Reworded int
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
			ID       string `json:"id"`
			Keep     bool   `json:"keep"`
			Severity string `json:"severity"`
			Reason   string `json:"reason"`
			Body     string `json:"body"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Kept = append(t.Kept, candidates...)
		return t, st, fmt.Errorf("triage output invalid: %w", err)
	}
	drop := map[string]string{}
	severity := map[string]string{}
	bodies := map[string]string{}
	for _, d := range out.Decisions {
		id := strings.ToUpper(strings.TrimSpace(d.ID))
		if !d.Keep {
			drop[id] = d.Reason
			continue
		}
		if _, ok := severityRank[d.Severity]; ok {
			severity[id] = d.Severity
		}
		if b := strings.TrimSpace(d.Body); b != "" {
			bodies[id] = b
		}
	}
	for i, c := range candidates {
		id := fmt.Sprintf("F%d", i+1)
		if reason, ok := drop[id]; ok {
			t.Dropped = append(t.Dropped, c)
			t.Reasons = append(t.Reasons, reason)
			continue
		}
		if sev, ok := severity[id]; ok && sev != c.Severity {
			t.Rerated++
			c.Severity = sev
		}
		if b, ok := bodies[id]; ok && b != strings.TrimSpace(c.Body) {
			t.Reworded++
			c.Body = b
		}
		t.Kept = append(t.Kept, c)
	}
	return t, st, nil
}
