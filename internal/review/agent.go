package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
)

type Repo interface {
	ReadFile(ctx context.Context, path string) (string, error)
	ListDir(ctx context.Context, path string) (string, error)
	Search(ctx context.Context, pattern, path string) (string, error)
	SearchBase(ctx context.Context, pattern, path string) (string, error)
}

const ToolPrompt = `

You have read-only access to the full repository at the pull request's head commit through these tools:
- read_file(path): the contents of one file
- list_dir(path): entries in a directory ("" for the root)
- search(pattern, path): extended-regex grep across the repository, optionally limited to a path
- search_base(pattern, path): the same grep on the base branch, which is already merged and running; a construct found there is known to work

A review based on the diff alone is incomplete. Before submitting, open the files this change depends on: the base or parent configuration a change builds on, files the diff references by name, callers of a changed function, and the equivalent file in a sibling environment when one exists. Use what you find to confirm or drop each concern; do not raise a concern that a quick read could have settled, and do not read more than you need. When you have finished, call submit_review exactly once with your final review. Never write the review as plain text.`

const VerifyPrompt = `You are checking one finding from an automated code review before it is posted. You have read-only access to the repository at the pull request's head commit through read_file, list_dir and search, and to the base branch through search_base. The base branch is merged and running, so anything found there is known to work.

Re-read the lines the finding points at and whatever else is needed to decide whether it is true. Be sceptical. A finding is rejected if it misreads the code, describes something that is already handled, or is speculative. If the finding claims a syntax error, an invalid or unsupported argument, option, field, metric or API, or that something does not exist, you must call search_base for the same construct before deciding: the reviewer's knowledge of languages and external tools may be out of date, and if the base branch uses the construct, the finding is wrong and must be rejected. If the base branch does not use it and you cannot prove the claim from the repository, downgrade it to a minor "please verify" note rather than confirming it. A finding is downgraded if the problem is real but less severe than stated or needs rewording to be accurate. A finding is confirmed only if you have checked it against the files and it holds as written.

Call submit_verdict exactly once with: verdict (confirmed, downgraded or rejected), severity (critical, major, minor or nit; the severity it should be posted at), body (the finding text to post, corrected if needed), and reason (one sentence for the log).`

var reviewTools = []ollama.Tool{
	fn("read_file", "Read one file from the repository at the pull request head commit.",
		`{"type":"object","properties":{"path":{"type":"string","description":"path relative to the repository root"}},"required":["path"]}`),
	fn("list_dir", "List the entries of a directory in the repository. Directories end with a slash.",
		`{"type":"object","properties":{"path":{"type":"string","description":"directory path relative to the repository root, empty for the root"}}}`),
	fn("search", "Search file contents across the repository with an extended regular expression.",
		`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"optional directory or file to limit the search to"}},"required":["pattern"]}`),
	fn("search_base", "Search file contents on the base branch, which is already merged and running, with an extended regular expression. Use it to check whether a construct is already in working use.",
		`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"optional directory or file to limit the search to"}},"required":["pattern"]}`),
	fn("submit_review", "Submit the final review. Call exactly once when done.", string(Schema)),
}

var verifyTools = append(append([]ollama.Tool{}, reviewTools[:4]...),
	fn("submit_verdict", "Submit the verdict on the finding. Call exactly once when done.", `{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["confirmed", "downgraded", "rejected"]},
    "severity": {"type": "string", "enum": ["critical", "major", "minor", "nit"]},
    "body": {"type": "string"},
    "reason": {"type": "string"}
  },
  "required": ["verdict", "severity", "body", "reason"]
}`))

func fn(name, desc, params string) ollama.Tool {
	return ollama.Tool{Type: "function", Function: ollama.ToolFunction{Name: name, Description: desc, Parameters: json.RawMessage(params)}}
}

type Stats struct {
	ollama.Usage
	ToolCalls int
	Rounds    int
}

func (s *Stats) Merge(o Stats) {
	s.Usage.Add(o.Usage)
	s.ToolCalls += o.ToolCalls
	s.Rounds += o.Rounds
}

type Limits struct {
	MaxToolCalls int
	MaxOutput    int
}

func runLoop(ctx context.Context, llm *ollama.Client, repo Repo, messages []ollama.Message, tools []ollama.Tool, final string, finalSchema json.RawMessage, validate func(json.RawMessage) error, lim Limits, log *slog.Logger) (json.RawMessage, Stats, error) {
	var st Stats
	nudges := 0
	for st.Rounds = 1; st.Rounds <= lim.MaxToolCalls+5; st.Rounds++ {
		msg, u, err := llm.Chat(ctx, messages, tools, nil)
		if err != nil {
			return nil, st, err
		}
		st.Usage.Add(u)
		messages = append(messages, msg)
		log.Debug("round", "n", st.Rounds, "output_tokens", u.OutputTokens, "truncated", u.Truncated, "tool_calls", len(msg.ToolCalls))

		if st.OutputTokens > lim.MaxOutput {
			log.Warn("output budget exhausted, forcing the final answer", "output_tokens", st.OutputTokens)
			return finalise(ctx, llm, messages, final, finalSchema, &st)
		}
		mustRead := final == "submit_review" && st.ToolCalls == 0 && nudges == 0
		if len(msg.ToolCalls) == 0 {
			if looksLikeJSON(msg.Content) && !mustRead {
				return json.RawMessage(extractJSON(msg.Content)), st, nil
			}
			nudges++
			if nudges > 2 {
				return finalise(ctx, llm, messages, final, finalSchema, &st)
			}
			text := fmt.Sprintf("Call %s now with your final answer.", final)
			if u.Truncated && msg.Content == "" {
				text = fmt.Sprintf("Your reasoning was cut off. Decide now with what you have: call a tool or %s, without further deliberation.", final)
			} else if mustRead {
				text = "You have not read anything from the repository. Use the tools to open the files this change depends on and check the consistency points, then call submit_review."
			}
			messages = append(messages, ollama.Message{Role: "user", Content: text})
			continue
		}
		for _, call := range msg.ToolCalls {
			name := call.Function.Name
			if name == final {
				if mustRead {
					nudges++
					messages = append(messages, ollama.Message{Role: "tool", ToolName: name, ToolCallID: call.ID, Content: "rejected: you have not read anything from the repository yet. Open the files this change depends on and check the consistency points first, then call submit_review again."})
					continue
				}
				if err := validate(call.Function.Arguments); err != nil {
					nudges++
					log.Warn("final tool call rejected, asking for a resubmit", "err", err)
					messages = append(messages, ollama.Message{Role: "tool", ToolName: name, ToolCallID: call.ID, Content: "invalid arguments: " + err.Error() + ". Call " + name + " again with corrected arguments."})
					continue
				}
				return call.Function.Arguments, st, nil
			}
			st.ToolCalls++
			var content string
			if st.ToolCalls > lim.MaxToolCalls {
				content = fmt.Sprintf("tool budget exhausted; call %s now with what you know", final)
			} else {
				var args map[string]any
				_ = json.Unmarshal(call.Function.Arguments, &args)
				content = runTool(ctx, repo, name, args)
				log.Debug("tool", "name", name, "args", args, "bytes", len(content))
			}
			messages = append(messages, ollama.Message{Role: "tool", ToolName: name, ToolCallID: call.ID, Content: content})
		}
	}
	return finalise(ctx, llm, messages, final, finalSchema, &st)
}

func finalise(ctx context.Context, llm *ollama.Client, messages []ollama.Message, final string, schema json.RawMessage, st *Stats) (json.RawMessage, Stats, error) {
	messages = append(messages, ollama.Message{Role: "user", Content: fmt.Sprintf("Produce the %s arguments now as a JSON object. No tool calls, no prose.", final)})
	msg, u, err := llm.Chat(ctx, messages, nil, schema)
	if err != nil {
		return nil, *st, err
	}
	st.Usage.Add(u)
	st.Rounds++
	return json.RawMessage(extractJSON(msg.Content)), *st, nil
}

func looksLikeJSON(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "```")
}

func extractJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = strings.TrimSpace(s[i+len("</think>"):])
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	return s
}

func runTool(ctx context.Context, repo Repo, name string, args map[string]any) string {
	str := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	var out string
	var err error
	switch name {
	case "read_file":
		out, err = repo.ReadFile(ctx, str("path"))
	case "list_dir":
		out, err = repo.ListDir(ctx, str("path"))
	case "search":
		out, err = repo.Search(ctx, str("pattern"), str("path"))
	case "search_base":
		out, err = repo.SearchBase(ctx, str("pattern"), str("path"))
	default:
		err = errors.New("unknown tool " + name)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	if strings.TrimSpace(out) == "" {
		return "(empty)"
	}
	return out
}

func RunAgent(ctx context.Context, llm *ollama.Client, repo Repo, system, user string, lim Limits, log *slog.Logger) (*Result, Stats, error) {
	messages := []ollama.Message{
		{Role: "system", Content: system + ToolPrompt},
		{Role: "user", Content: user},
	}
	validate := func(raw json.RawMessage) error {
		var r Result
		return json.Unmarshal(raw, &r)
	}
	raw, st, err := runLoop(ctx, llm, repo, messages, reviewTools, "submit_review", Schema, validate, lim, log)
	if err != nil {
		return nil, st, err
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, st, fmt.Errorf("model output is not a valid review: %w: %s", err, truncate(string(raw), 200))
	}
	return &res, st, nil
}

func RunSingle(ctx context.Context, llm *ollama.Client, system, user string, schema json.RawMessage) (json.RawMessage, Stats, error) {
	var st Stats
	messages := []ollama.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	msg, u, err := llm.Chat(ctx, messages, nil, schema)
	if err != nil {
		return nil, st, err
	}
	st.Usage.Add(u)
	st.Rounds = 1
	return json.RawMessage(extractJSON(msg.Content)), st, nil
}

type Verdict struct {
	Verdict  string `json:"verdict"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
	Reason   string `json:"reason"`
}

func VerifyFinding(ctx context.Context, llm *ollama.Client, repo Repo, header string, c Comment, fileDiff string, lim Limits, log *slog.Logger) (Verdict, Stats, error) {
	var b strings.Builder
	b.WriteString(header)
	fmt.Fprintf(&b, "\nFinding to check (severity %s) at %s line %d:\n%s\n", c.Severity, c.Path, c.Line, c.Body)
	if fileDiff != "" {
		b.WriteString("\nDiff of that file, each line prefixed with its line number in the new version:\n")
		b.WriteString(fileDiff)
	}
	messages := []ollama.Message{
		{Role: "system", Content: VerifyPrompt},
		{Role: "user", Content: b.String()},
	}
	schema := verifyTools[len(verifyTools)-1].Function.Parameters
	validate := func(raw json.RawMessage) error {
		var v Verdict
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if v.Verdict == "" {
			return errors.New("verdict is required")
		}
		return nil
	}
	raw, st, err := runLoop(ctx, llm, repo, messages, verifyTools, "submit_verdict", schema, validate, lim, log)
	if err != nil {
		return Verdict{}, st, err
	}
	var v Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return Verdict{}, st, fmt.Errorf("verdict is not valid JSON: %w", err)
	}
	return v, st, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func Layout(ctx context.Context, repo Repo, files []string) string {
	seen := map[string]bool{}
	dirs := []string{""}
	for _, f := range files {
		d := ""
		if i := strings.LastIndex(f, "/"); i >= 0 {
			d = f[:i]
		}
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	var b strings.Builder
	for i, d := range dirs {
		if i >= 12 {
			break
		}
		out, err := repo.ListDir(ctx, d)
		if err != nil {
			continue
		}
		if d == "" {
			d = "/"
		}
		fmt.Fprintf(&b, "%s\n", d)
		for _, e := range strings.Split(strings.TrimSpace(out), "\n") {
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}
	return b.String()
}
