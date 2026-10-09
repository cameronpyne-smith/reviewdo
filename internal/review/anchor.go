package review

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

type fileLines struct {
	lines map[int]string
	all   []string
}

func normalise(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func matches(quote, line string) bool {
	q, l := normalise(quote), normalise(line)
	if q == "" || l == "" {
		return false
	}
	return strings.Contains(l, q) || strings.Contains(q, l)
}

func Anchor(ctx context.Context, repo Repo, files []*diff.File, comments []Comment, log *slog.Logger) (kept, dropped []Comment) {
	diffText := map[string]map[int]string{}
	for _, f := range files {
		if !f.Deleted && !f.Binary {
			diffText[f.Path] = f.RightText()
		}
	}
	whole := map[string][]string{}
	read := func(path string) []string {
		if lines, ok := whole[path]; ok {
			return lines
		}
		var lines []string
		if repo != nil {
			if text, err := repo.ReadFile(ctx, path); err == nil {
				lines = strings.Split(text, "\n")
			}
		}
		whole[path] = lines
		return lines
	}
	for _, c := range comments {
		q := strings.TrimSpace(c.Quote)
		if q == "" {
			kept = append(kept, c)
			continue
		}
		if line, ok := diffText[c.Path][c.Line]; ok && matches(q, line) {
			kept = append(kept, c)
			continue
		}
		lines := read(c.Path)
		if c.Line >= 1 && c.Line <= len(lines) && matches(q, lines[c.Line-1]) {
			kept = append(kept, c)
			continue
		}
		best, bestDist := 0, 0
		for i, l := range lines {
			if !matches(q, l) {
				continue
			}
			d := i + 1 - c.Line
			if d < 0 {
				d = -d
			}
			if best == 0 || d < bestDist {
				best, bestDist = i+1, d
			}
		}
		if best == 0 {
			for n, l := range diffText[c.Path] {
				if !matches(q, l) {
					continue
				}
				d := n - c.Line
				if d < 0 {
					d = -d
				}
				if best == 0 || d < bestDist {
					best, bestDist = n, d
				}
			}
		}
		if best == 0 {
			log.Info("dropping finding whose quoted line is not in the file", "path", c.Path, "line", c.Line, "quote", truncate(q, 80))
			dropped = append(dropped, c)
			continue
		}
		log.Info("moved finding to the line it quotes", "path", c.Path, "from", c.Line, "to", best)
		c.Line = best
		kept = append(kept, c)
	}
	return kept, dropped
}
