package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
)

const RulesPrompt = `You are checking a pull request against the written conventions of its repository. You are given the convention documents and the changed lines of each file, with line numbers. Report every added line that breaks a rule the documents state.

Rules:
- Only rules written in the documents count, never your own preferences or general best practice.
- Only added lines (marked with +) count. Lines marked with a space are unchanged context, shown so you can read a statement whole.
- Do not report something a document explicitly allows, and do not report a rule the document itself says applies only to other kinds of file.
- A rule that names a replacement (use X instead of Y, always use the Z alias) is broken by any added line that uses Y or omits Z, however small the line.
- Do not report formatting the documents do not mention, and do not report the same rule twice at adjacent lines; report each distinct place once.

For each violation give: path; line (the added line's number); quote (that line, verbatim, so it can be located); rule (the sentence or phrase from the documents that states the rule, copied verbatim, at least six words, nothing paraphrased); title (at most eight words naming the rule broken); problem (one or two sentences to the author saying what the line does, what the rule requires and what to change).

Respond with JSON only: {"violations":[{"path":"...","line":12,"quote":"...","rule":"...","title":"...","problem":"..."}]}, with an empty list when nothing breaks a stated rule.`

var RulesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "violations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string"},
          "line": {"type": "integer"},
          "quote": {"type": "string"},
          "rule": {"type": "string"},
          "title": {"type": "string"},
          "problem": {"type": "string"}
        },
        "required": ["path", "line", "quote", "rule", "title", "problem"]
      }
    }
  },
  "required": ["violations"]
}`)

const (
	maxRulesDiffBytes = 60 << 10
	minRuleQuoteWords = 5
)

type Violation struct {
	Comment
	Rule   string
	Source string
}

func changedLines(f *diff.File) string {
	var b strings.Builder
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case '+':
				fmt.Fprintf(&b, "+%5d: %s\n", l.NewNo, l.Text)
			case ' ':
				fmt.Fprintf(&b, " %5d: %s\n", l.NewNo, l.Text)
			}
		}
	}
	return b.String()
}

func rulesChunks(files []*diff.File, ignore []string) [][]string {
	var chunks [][]string
	var cur []string
	size := 0
	for _, f := range files {
		if f.Binary || f.Deleted || diff.Ignored(f.Path, ignore) {
			continue
		}
		text := changedLines(f)
		if text == "" {
			continue
		}
		if len(text) > maxRulesDiffBytes {
			text = text[:maxRulesDiffBytes] + "\n[rest of the file's changes omitted]\n"
		}
		entry := "### " + f.Path + "\n" + text
		if size > 0 && size+len(entry) > maxRulesDiffBytes {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, entry)
		size += len(entry)
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

func plainText(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '`' || r == '*' || r == '_' || r == '"' || r == '\'' || r == '“' || r == '”' || r == '’':
		case r == ' ' || r == '\n' || r == '\t' || r == '\r':
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

func ruleSource(rule string, docs []Instruction) string {
	q := plainText(rule)
	if len(strings.Fields(q)) < minRuleQuoteWords {
		return ""
	}
	for _, d := range docs {
		if strings.Contains(plainText(d.Text), q) {
			return d.Source
		}
	}
	return ""
}

func RulesInput(docs []Instruction, chunk []string) string {
	var b strings.Builder
	b.WriteString("## Conventions\n")
	for _, d := range docs {
		fmt.Fprintf(&b, "\n### %s\n\n%s\n", d.Source, d.Text)
	}
	b.WriteString("\n## Changed lines\n\n")
	b.WriteString(strings.Join(chunk, "\n"))
	return b.String()
}

func Rules(ctx context.Context, llm *ollama.Client, docs []Instruction, files []*diff.File, ignore []string) ([]Violation, Stats, error) {
	var total Stats
	var all []Violation
	var withText []Instruction
	for _, d := range docs {
		if strings.TrimSpace(d.Text) != "" {
			withText = append(withText, d)
		}
	}
	if len(withText) == 0 {
		return nil, total, nil
	}
	seen := map[string]bool{}
	var errs []string
	for _, chunk := range rulesChunks(files, ignore) {
		if ctx.Err() != nil {
			break
		}
		raw, st, err := RunSingle(ctx, llm, RulesPrompt, RulesInput(withText, chunk), RulesSchema)
		total.Merge(st)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var out struct {
			Violations []struct {
				Path    string `json:"path"`
				Line    int    `json:"line"`
				Quote   string `json:"quote"`
				Rule    string `json:"rule"`
				Title   string `json:"title"`
				Problem string `json:"problem"`
			} `json:"violations"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			errs = append(errs, "rules output invalid: "+err.Error())
			continue
		}
		for _, v := range out.Violations {
			source := ruleSource(v.Rule, withText)
			v.Path = strings.TrimSpace(v.Path)
			if source == "" || v.Path == "" || v.Line <= 0 || strings.TrimSpace(v.Problem) == "" {
				continue
			}
			key := fmt.Sprintf("%s:%d", v.Path, v.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			title := strings.TrimSpace(v.Title)
			if title == "" {
				title = "Breaks a repository convention"
			}
			body := strings.TrimSpace(v.Problem) + "\n\nThe repository's " + source + " says: \"" + strings.TrimSpace(v.Rule) + "\""
			all = append(all, Violation{
				Comment: Comment{Path: v.Path, Line: v.Line, Quote: v.Quote, Title: title, Severity: "minor", Body: body},
				Rule:    v.Rule, Source: source,
			})
		}
	}
	if len(errs) > 0 {
		return all, total, fmt.Errorf("rules: %s", strings.Join(errs, "; "))
	}
	return all, total, nil
}

func AddViolations(comments []Comment, violations []Violation) ([]Comment, int) {
	added := 0
	for _, v := range violations {
		dup := false
		for _, c := range comments {
			if c.Path == v.Path && abs(c.Line-v.Line) <= 2 {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		comments = append(comments, v.Comment)
		added++
	}
	return comments, added
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
