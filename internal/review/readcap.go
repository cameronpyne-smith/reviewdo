package review

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

const (
	maxWholeRead   = 24 << 10
	maxRangedRead  = 64 << 10
	maxRegionBytes = 32 << 10
	regionMargin   = 30
)

type Span struct{ Start, End int }

func ChangedSpans(files []*diff.File) map[string][]Span {
	out := map[string][]Span{}
	for _, f := range files {
		if f.Binary || f.Deleted {
			continue
		}
		for _, h := range f.Hunks {
			s := Span{}
			for _, l := range h.Lines {
				if l.Kind == '-' || l.NewNo == 0 {
					continue
				}
				if s.Start == 0 || l.NewNo < s.Start {
					s.Start = l.NewNo
				}
				if l.NewNo > s.End {
					s.End = l.NewNo
				}
			}
			if s.Start > 0 {
				out[f.Path] = append(out[f.Path], s)
			}
		}
	}
	return out
}

func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := strings.LastIndexByte(s[:limit], '\n')
	if cut <= 0 {
		cut = limit
	}
	return s[:cut] + fmt.Sprintf("\n[truncated: %d of %d bytes shown; read the rest by line range]\n", cut, len(s))
}

func mergeSpans(spans []Span, margin, last int) []Span {
	var out []Span
	for _, s := range spans {
		s.Start = max(1, s.Start-margin)
		s.End = min(last, s.End+margin)
		if n := len(out); n > 0 && s.Start <= out[n-1].End+1 {
			out[n-1].End = max(out[n-1].End, s.End)
			continue
		}
		out = append(out, s)
	}
	return out
}

func readFile(ctx context.Context, repo Repo, path string, start, end int, changed map[string][]Span) (string, error) {
	text, err := repo.ReadFile(ctx, path)
	if err != nil {
		return "", err
	}
	if start > 0 || end > 0 {
		return capText(lineRange(text, start, end), maxRangedRead), nil
	}
	if len(text) <= maxWholeRead {
		return text, nil
	}
	return largeFileView(ctx, repo, path, text, changed[path]), nil
}

func largeFileView(ctx context.Context, repo Repo, path, text string, spans []Span) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d lines (%d KB), too large to show whole. ", path, len(lines), len(text)>>10)
	if len(spans) > 0 {
		b.WriteString("Below are its outline and the regions this pull request changes, each with surrounding lines. ")
	} else {
		b.WriteString("Below is its outline. ")
	}
	b.WriteString("Read any other part with read_file(path, start, end).\n\nOutline:\n")
	if outline, err := repo.Outline(ctx, path); err == nil {
		b.WriteString(outline)
	} else {
		b.WriteString("(unavailable)\n")
	}
	if len(spans) == 0 {
		return b.String()
	}
	sorted := append([]Span{}, spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	regions := mergeSpans(sorted, regionMargin, len(lines))
	size := 0
	for _, r := range regions {
		for _, l := range lines[r.Start-1 : r.End] {
			size += len(l) + 1
		}
	}
	if size > maxRegionBytes {
		regions = mergeSpans(sorted, regionMargin/3, len(lines))
	}
	b.WriteString("\nChanged regions:\n")
	written := 0
	for i, r := range regions {
		block := strings.Join(lines[r.Start-1:r.End], "\n")
		if written+len(block) > maxRegionBytes {
			fmt.Fprintf(&b, "[%d more changed regions omitted: lines", len(regions)-i)
			for j, o := range regions[i:] {
				if j > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, " %d-%d", o.Start, o.End)
			}
			b.WriteString("; read them by range]\n")
			break
		}
		fmt.Fprintf(&b, "\nlines %d-%d of %d:\n%s\n", r.Start, r.End, len(lines), block)
		written += len(block)
	}
	return b.String()
}
