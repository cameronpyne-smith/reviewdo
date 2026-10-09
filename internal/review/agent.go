package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
)

type Repo interface {
	ReadFile(ctx context.Context, path string) (string, error)
	ListDir(ctx context.Context, path string) (string, error)
	Search(ctx context.Context, pattern, path string) (string, error)
	SearchBase(ctx context.Context, pattern, path string) (string, error)
	Definition(ctx context.Context, name, path string) (string, error)
	References(ctx context.Context, name, path string) (string, error)
	Outline(ctx context.Context, path string) (string, error)
}

const ToolPrompt = `

You have read-only access to the full repository at the pull request's head commit through these tools:
- read_file(path, start, end): the contents of one file, or only lines start to end; on a large file call outline first and read the ranges you need
- list_dir(path): entries in a directory ("" for the root)
- search(pattern, path): extended-regex grep across the repository, optionally limited to a path
- search_base(pattern, path): the same grep on the base branch, which is already merged and running; a construct found there is known to work
- definition(name, path): where a class, function, method, type, variable or resource with that exact name is declared, with the lines that follow
- references(name, path): every use of that exact name, grouped by file with counts
- outline(path): the declarations in one file with their line numbers, to orient in a large file before reading the part you need

A review based on the diff alone is incomplete: the diff shows a few lines of each change and none of what they depend on. Work like this:
1. Read the diff and write down, for yourself, the questions it raises: what calls this, what defines that, where else is this name used, does this path exist, what does the sibling file say.
2. Open every modified file in full with read_file so you see each change in its real context. New files are already shown whole in the diff. You may make several tool calls in one turn; do so.
3. Answer each question with the tools: definition for what a changed line calls or extends, references for callers and other users of a changed function, setting or name, the base or parent configuration a change builds on, files the diff references by name, every path or link it mentions, and the equivalent file in a sibling environment when one exists. If a search finds nothing, retry once with a simpler pattern, and use list_dir rather than guessing paths.
4. Raise only what you confirmed or could not settle after looking; drop a concern that a read settled. Do not read more than the questions need.
When you have finished, call submit_review exactly once with your final review. Never write the review as plain text.`

const VerifyPrompt = `You are checking one finding from an automated code review before it is posted. You have read-only access to the repository at the pull request's head commit through read_file, list_dir, search, definition, references and outline, and to the base branch through search_base. The base branch is merged and running, so anything found there is known to work.

Re-read the lines the finding points at and whatever else is needed to decide whether it is true. Be sceptical. A finding is rejected if it misreads the code, describes something that is already handled, or is speculative. If the finding claims a syntax error, an invalid or unsupported argument, option, field, metric or API, or that something does not exist, you must call search_base for the same construct before deciding: the reviewer's knowledge of languages and external tools may be out of date, and if the base branch uses the construct, the finding is wrong and must be rejected. If the base branch does not use it and you cannot prove the claim from the repository, reject it unless it concerns an external fact the repository cannot settle, in which case downgrade it to minor with a body that says exactly what would be wrong and what it would cause, never a request to verify. A finding that rests on how a library, client or broker behaves (what a method throws, which interface a type implements, what happens on failure) is confirmed only if the repository shows that behaviour in a test, a comment or other code handling the same case; the reviewer's memory of a library is not evidence, and neither is yours. Otherwise reject it. A finding is downgraded if the problem is real but less severe than stated or needs rewording to be accurate. A finding is confirmed only if you have checked it against the files and it holds as written.

Call submit_verdict exactly once with: verdict (confirmed, downgraded or rejected), severity (critical, major, minor or nit; the severity it should be posted at), body, and reason (one sentence for the log). The body is the comment the author will read. Write it to the author about the code, never as a judgement on "the finding" or "the reviewer", and never mention this check. Remove any narration of what the reviewer did, searched or could not find. When the verdict is confirmed, return the original body unchanged. When it is downgraded, rewrite the body so it states accurately what is wrong and why it matters.`

var reviewTools = []ollama.Tool{
	fn("read_file", "Read one file from the repository at the pull request head commit, optionally only a range of lines.",
		`{"type":"object","properties":{"path":{"type":"string","description":"path relative to the repository root"},"start":{"type":"integer","description":"first line to return, 1-based; omit for the whole file"},"end":{"type":"integer","description":"last line to return"}},"required":["path"]}`),
	fn("list_dir", "List the entries of a directory in the repository. Directories end with a slash.",
		`{"type":"object","properties":{"path":{"type":"string","description":"directory path relative to the repository root, empty for the root"}}}`),
	fn("search", "Search file contents across the repository with an extended regular expression.",
		`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"optional directory or file to limit the search to"}},"required":["pattern"]}`),
	fn("search_base", "Search file contents on the base branch, which is already merged and running, with an extended regular expression. Use it to check whether a construct is already in working use.",
		`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"optional directory or file to limit the search to"}},"required":["pattern"]}`),
	fn("definition", "Find where a class, function, method, type, variable or resource with this exact name is declared, with the following lines.",
		`{"type":"object","properties":{"name":{"type":"string","description":"a single identifier"},"path":{"type":"string","description":"optional directory or file to limit the search to"}},"required":["name"]}`),
	fn("references", "List every use of this exact name in the repository, grouped by file with counts.",
		`{"type":"object","properties":{"name":{"type":"string","description":"a single identifier"},"path":{"type":"string","description":"optional directory or file to limit the search to"}},"required":["name"]}`),
	fn("outline", "List the declarations in one file with their line numbers.",
		`{"type":"object","properties":{"path":{"type":"string","description":"path relative to the repository root"}},"required":["path"]}`),
	fn("submit_review", "Submit the final review. Call exactly once when done.", string(Schema)),
}

var verifyTools = append(append([]ollama.Tool{}, reviewTools[:len(reviewTools)-1]...),
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
	MaxContext   int
	Deadline     time.Time
	MustRead     []string
}

func approxTokens(messages []ollama.Message) int {
	n := 0
	for _, m := range messages {
		n += len(m.Content) + len(m.Thinking)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments)
		}
	}
	return n / 3
}

func shrink(messages []ollama.Message, budget int) []ollama.Message {
	if len(messages) <= 2 || approxTokens(messages) <= budget {
		return messages
	}
	head := messages[:2]
	used := approxTokens(head)
	var tail []ollama.Message
	for i := len(messages) - 1; i >= 2; i-- {
		t := approxTokens(messages[i : i+1])
		if used+t > budget {
			break
		}
		used += t
		tail = append([]ollama.Message{messages[i]}, tail...)
	}
	for len(tail) > 0 && tail[0].Role == "tool" {
		tail = tail[1:]
	}
	note := ollama.Message{Role: "user", Content: "Earlier tool results were removed to fit the context window."}
	return append(append(append([]ollama.Message{}, head...), note), tail...)
}

func unread(must []string, read map[string]bool) []string {
	var out []string
	for _, p := range must {
		if !read[p] {
			out = append(out, p)
		}
	}
	return out
}

func runLoop(ctx context.Context, llm *ollama.Client, repo Repo, messages []ollama.Message, tools []ollama.Tool, final string, finalSchema json.RawMessage, validate func(json.RawMessage) error, lim Limits, log *slog.Logger) (json.RawMessage, Stats, error) {
	var st Stats
	nudges := 0
	readNudges := 0
	read := map[string]bool{}
	for st.Rounds = 1; st.Rounds <= lim.MaxToolCalls+5; st.Rounds++ {
		msg, u, err := llm.Chat(ctx, messages, tools, nil)
		if err != nil {
			if lim.MaxContext > 0 && strings.Contains(err.Error(), "no user query found") {
				log.Warn("context window overflowed, forcing the final answer on a shortened history", "rounds", st.Rounds, "tool_calls", st.ToolCalls)
				return finalise(ctx, llm, messages, final, finalSchema, lim, &st)
			}
			return nil, st, err
		}
		st.Usage.Add(u)
		messages = append(messages, msg)
		used := max(u.PromptTokens, approxTokens(messages))
		log.Debug("round", "n", st.Rounds, "context", used, "output_tokens", u.OutputTokens, "truncated", u.Truncated, "tool_calls", len(msg.ToolCalls))
		if lim.MaxContext > 0 && used > lim.MaxContext {
			log.Info("context budget exhausted, forcing the final answer", "context", used, "rounds", st.Rounds, "tool_calls", st.ToolCalls)
			return finalise(ctx, llm, messages, final, finalSchema, lim, &st)
		}

		if st.OutputTokens > lim.MaxOutput {
			log.Warn("output budget exhausted, forcing the final answer", "output_tokens", st.OutputTokens)
			return finalise(ctx, llm, messages, final, finalSchema, lim, &st)
		}
		if !lim.Deadline.IsZero() && time.Now().After(lim.Deadline) && !(len(msg.ToolCalls) == 0 && looksLikeJSON(msg.Content)) {
			log.Info("time budget exhausted, forcing the final answer", "rounds", st.Rounds, "tool_calls", st.ToolCalls)
			return finalise(ctx, llm, messages, final, finalSchema, lim, &st)
		}
		var missing []string
		if final == "submit_review" && readNudges < 2 {
			if len(lim.MustRead) > 0 {
				missing = unread(lim.MustRead, read)
			} else if st.ToolCalls == 0 {
				missing = []string{"the files this change depends on"}
			}
		}
		if len(msg.ToolCalls) == 0 {
			if looksLikeJSON(msg.Content) && len(missing) == 0 {
				return json.RawMessage(extractJSON(msg.Content)), st, nil
			}
			nudges++
			if nudges > 2 {
				return finalise(ctx, llm, messages, final, finalSchema, lim, &st)
			}
			text := fmt.Sprintf("Call %s now with your final answer.", final)
			if u.Truncated && msg.Content == "" {
				text = fmt.Sprintf("Your reasoning was cut off. Decide now with what you have: call a tool or %s, without further deliberation.", final)
			} else if len(missing) > 0 {
				readNudges++
				text = "You have not yet read " + strings.Join(missing, ", ") + ". Open them with read_file, check the consistency points, then call submit_review."
			}
			messages = append(messages, ollama.Message{Role: "user", Content: text})
			continue
		}
		exhausted := false
		for _, call := range msg.ToolCalls {
			name := call.Function.Name
			if name == final {
				if len(missing) > 0 {
					readNudges++
					messages = append(messages, ollama.Message{Role: "tool", ToolName: name, ToolCallID: call.ID, Content: "rejected: you have not yet read " + strings.Join(missing, ", ") + ". Open them with read_file, check the consistency points, then call submit_review again."})
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
				exhausted = true
				content = "tool budget exhausted"
			} else {
				var args map[string]any
				_ = json.Unmarshal(call.Function.Arguments, &args)
				content = runTool(ctx, repo, name, args)
				if name == "read_file" {
					if path, _ := args["path"].(string); path != "" {
						read[path] = true
					}
				}
				log.Debug("tool", "name", name, "args", args, "bytes", len(content))
				if lim.MaxContext > 0 && used+len(content)/3 > lim.MaxContext {
					exhausted = true
					content = "[output omitted: the context window is full]"
				}
			}
			used += len(content) / 3
			messages = append(messages, ollama.Message{Role: "tool", ToolName: name, ToolCallID: call.ID, Content: content})
		}
		if exhausted {
			log.Info("tool or context budget exhausted, forcing the final answer", "rounds", st.Rounds, "tool_calls", st.ToolCalls, "context", used)
			return finalise(ctx, llm, messages, final, finalSchema, lim, &st)
		}
	}
	return finalise(ctx, llm, messages, final, finalSchema, lim, &st)
}

func finalise(ctx context.Context, llm *ollama.Client, messages []ollama.Message, final string, schema json.RawMessage, lim Limits, st *Stats) (json.RawMessage, Stats, error) {
	if lim.MaxContext > 0 {
		messages = shrink(messages, lim.MaxContext)
	}
	messages = append(messages, ollama.Message{Role: "user", Content: fmt.Sprintf("Produce the %s arguments now as a JSON object. No tool calls, no prose, no further deliberation.", final)})
	for attempt, client := range []*ollama.Client{llm, llm.WithThink(false)} {
		msg, u, err := client.Chat(ctx, messages, nil, schema)
		if err != nil {
			return nil, *st, err
		}
		st.Usage.Add(u)
		st.Rounds++
		raw := extractJSON(msg.Content)
		if json.Valid([]byte(raw)) {
			return json.RawMessage(raw), *st, nil
		}
		if attempt == 0 {
			messages = append(messages, msg, ollama.Message{Role: "user", Content: "That was not a JSON object. Output only the JSON object."})
		}
	}
	return nil, *st, errors.New("model did not produce a JSON answer")
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
	num := func(k string) int {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case string:
			n, _ := strconv.Atoi(v)
			return n
		}
		return 0
	}
	switch name {
	case "read_file":
		out, err = repo.ReadFile(ctx, str("path"))
		if err == nil {
			out = lineRange(out, num("start"), num("end"))
		}
	case "list_dir":
		out, err = repo.ListDir(ctx, str("path"))
	case "search":
		out, err = repo.Search(ctx, str("pattern"), str("path"))
	case "search_base":
		out, err = repo.SearchBase(ctx, str("pattern"), str("path"))
	case "definition":
		out, err = repo.Definition(ctx, str("name"), str("path"))
	case "references":
		out, err = repo.References(ctx, str("name"), str("path"))
	case "outline":
		out, err = repo.Outline(ctx, str("path"))
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

func lineRange(text string, start, end int) string {
	if start <= 0 && end <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return fmt.Sprintf("(file has %d lines)\n", len(lines))
	}
	if end < start {
		end = start
	}
	return fmt.Sprintf("lines %d-%d of %d:\n%s\n", start, end, len(lines), strings.Join(lines[start-1:end], "\n"))
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
