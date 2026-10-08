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
}

const ToolPrompt = `

You have read-only access to the full repository at the pull request's head commit through these tools:
- read_file(path): the contents of one file
- list_dir(path): entries in a directory ("" for the root)
- search(pattern, path): extended-regex grep across the repository, optionally limited to a path

A review based on the diff alone is incomplete. Before submitting, open the files this change depends on: the base or parent configuration a change builds on, files the diff references by name, callers of a changed function, and the equivalent file in a sibling environment when one exists. Use what you find to confirm or drop each concern; do not raise a concern that a quick read could have settled, and do not read more than you need. When you have finished, call submit_review exactly once with your final review. Never write the review as plain text.`

var tools = []ollama.Tool{
	fn("read_file", "Read one file from the repository at the pull request head commit.",
		`{"type":"object","properties":{"path":{"type":"string","description":"path relative to the repository root"}},"required":["path"]}`),
	fn("list_dir", "List the entries of a directory in the repository. Directories end with a slash.",
		`{"type":"object","properties":{"path":{"type":"string","description":"directory path relative to the repository root, empty for the root"}}}`),
	fn("search", "Search file contents across the repository with an extended regular expression.",
		`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"optional directory or file to limit the search to"}},"required":["pattern"]}`),
	fn("submit_review", "Submit the final review. Call exactly once when done.", string(Schema)),
}

func fn(name, desc, params string) ollama.Tool {
	return ollama.Tool{Type: "function", Function: ollama.ToolFunction{Name: name, Description: desc, Parameters: json.RawMessage(params)}}
}

type Stats struct {
	ollama.Usage
	ToolCalls int
	Rounds    int
}

func RunAgent(ctx context.Context, llm *ollama.Client, repo Repo, system, user string, maxCalls, maxOutput int, log *slog.Logger) (*Result, Stats, error) {
	var st Stats
	messages := []ollama.Message{
		{Role: "system", Content: system + ToolPrompt},
		{Role: "user", Content: user},
	}
	nudges := 0
	for st.Rounds = 1; st.Rounds <= maxCalls+5; st.Rounds++ {
		msg, u, err := llm.Chat(ctx, messages, tools, nil)
		if err != nil {
			return nil, st, err
		}
		st.Usage.Add(u)
		messages = append(messages, msg)
		log.Debug("round", "n", st.Rounds, "output_tokens", u.OutputTokens, "truncated", u.Truncated, "tool_calls", len(msg.ToolCalls))

		if st.OutputTokens > maxOutput {
			log.Warn("output budget exhausted, forcing the final review", "output_tokens", st.OutputTokens)
			return finalise(ctx, llm, messages, st)
		}
		if len(msg.ToolCalls) == 0 {
			if u.Truncated && msg.Content == "" {
				nudges++
				if nudges > 2 {
					return finalise(ctx, llm, messages, st)
				}
				messages = append(messages, ollama.Message{Role: "user", Content: "Your reasoning was cut off. Decide now with what you have: call a tool or submit_review, without further deliberation."})
				continue
			}
			if st.ToolCalls == 0 && nudges == 0 {
				nudges++
				messages = append(messages, ollama.Message{Role: "user", Content: "You have not read anything from the repository. Use the tools to open the files this change depends on, then call submit_review."})
				continue
			}
			if res, err := ParseResult(msg.Content); err == nil && res.Summary != "" {
				return res, st, nil
			}
			nudges++
			if nudges > 2 {
				return finalise(ctx, llm, messages, st)
			}
			messages = append(messages, ollama.Message{Role: "user", Content: "Call submit_review now with your final review."})
			continue
		}

		for _, call := range msg.ToolCalls {
			name := call.Function.Name
			var args map[string]any
			_ = json.Unmarshal(call.Function.Arguments, &args)
			if name == "submit_review" {
				var res Result
				if err := json.Unmarshal(call.Function.Arguments, &res); err != nil || res.Summary == "" {
					messages = append(messages, ollama.Message{Role: "tool", ToolName: name, ToolCallID: call.ID, Content: "invalid arguments: provide verdict, summary and comments"})
					continue
				}
				return &res, st, nil
			}
			st.ToolCalls++
			var content string
			if st.ToolCalls > maxCalls {
				content = "tool budget exhausted; call submit_review now with what you know"
			} else {
				content = runTool(ctx, repo, name, args)
			}
			log.Debug("tool", "name", name, "args", args, "bytes", len(content))
			messages = append(messages, ollama.Message{Role: "tool", ToolName: name, ToolCallID: call.ID, Content: content})
		}
	}
	return finalise(ctx, llm, messages, st)
}

func finalise(ctx context.Context, llm *ollama.Client, messages []ollama.Message, st Stats) (*Result, Stats, error) {
	messages = append(messages, ollama.Message{Role: "user", Content: "Produce your final review now as a JSON object with verdict, summary and comments. No tool calls."})
	msg, u, err := llm.Chat(ctx, messages, nil, Schema)
	if err != nil {
		return nil, st, err
	}
	st.Usage.Add(u)
	st.Rounds++
	res, err := ParseResult(msg.Content)
	if err != nil {
		return nil, st, err
	}
	return res, st, nil
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

func RunSingle(ctx context.Context, llm *ollama.Client, system, user string) (*Result, Stats, error) {
	var st Stats
	messages := []ollama.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	msg, u, err := llm.Chat(ctx, messages, nil, Schema)
	if err != nil {
		return nil, st, err
	}
	st.Usage.Add(u)
	st.Rounds = 1
	res, err := ParseResult(msg.Content)
	if err != nil {
		return nil, st, fmt.Errorf("%w: %s", err, truncate(msg.Content, 200))
	}
	return res, st, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
