package review

import (
	"context"
	"fmt"
	"path"
	"strings"
)

type RefReader interface {
	ReadFileAt(ctx context.Context, ref, path string) (string, error)
	ListDirAt(ctx context.Context, ref, path string) (string, error)
}

type Instruction struct {
	Source string
	Text   string
}

const maxInstructionBytes = 48 << 10

func RepoInstructions(ctx context.Context, repo RefReader, ref string, changed []string) []Instruction {
	var out []Instruction
	if text, err := repo.ReadFileAt(ctx, ref, ".github/copilot-instructions.md"); err == nil {
		out = append(out, Instruction{Source: ".github/copilot-instructions.md", Text: clip(text)})
	}
	listing, err := repo.ListDirAt(ctx, ref, ".github/instructions")
	if err != nil {
		return out
	}
	for _, name := range strings.Split(strings.TrimSpace(listing), "\n") {
		if !strings.HasSuffix(name, ".instructions.md") {
			continue
		}
		p := ".github/instructions/" + name
		text, err := repo.ReadFileAt(ctx, ref, p)
		if err != nil {
			continue
		}
		applyTo, body := frontmatter(text)
		if applyTo != "" && !anyMatch(applyTo, changed) {
			continue
		}
		out = append(out, Instruction{Source: p, Text: clip(body)})
	}
	return out
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxInstructionBytes {
		return s[:maxInstructionBytes] + "\n[truncated]"
	}
	return s
}

func frontmatter(text string) (applyTo, body string) {
	if !strings.HasPrefix(text, "---\n") {
		return "", text
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", text
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "applyTo" {
			applyTo = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	body = rest[end+4:]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	}
	return applyTo, body
}

func splitPatterns(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func anyMatch(patterns string, files []string) bool {
	for _, pat := range splitPatterns(patterns) {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		for _, expanded := range expandBraces(pat) {
			for _, f := range files {
				if globMatch(expanded, f) {
					return true
				}
			}
		}
	}
	return false
}

func expandBraces(pat string) []string {
	open := strings.IndexByte(pat, '{')
	if open < 0 {
		return []string{pat}
	}
	close := strings.IndexByte(pat[open:], '}')
	if close < 0 {
		return []string{pat}
	}
	close += open
	var out []string
	for _, alt := range strings.Split(pat[open+1:close], ",") {
		out = append(out, expandBraces(pat[:open]+alt+pat[close+1:])...)
	}
	return out
}

func globMatch(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func RenderInstructions(global, repo string, files []Instruction) string {
	var b strings.Builder
	if strings.TrimSpace(global) != "" {
		b.WriteString("\n## Review guidance\n\n")
		b.WriteString(strings.TrimSpace(global))
		b.WriteString("\n")
	}
	if strings.TrimSpace(repo) != "" {
		if b.Len() == 0 {
			b.WriteString("\n## Review guidance\n")
		}
		b.WriteString("\n### For this repository\n\n")
		b.WriteString(strings.TrimSpace(repo))
		b.WriteString("\n")
	}
	for _, f := range files {
		if b.Len() == 0 {
			b.WriteString("\n## Review guidance\n")
		}
		fmt.Fprintf(&b, "\n### From the repository's %s\n\n%s\n", f.Source, f.Text)
	}
	return b.String()
}
