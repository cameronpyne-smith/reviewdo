package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	maxDefinitions = 30
	maxRefFiles    = 40
	maxRefLines    = 3
	maxOutline     = 300
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func declarationPatterns(name string) []string {
	n := regexp.QuoteMeta(name)
	return []string{
		`\b(class|interface|struct|enum|record|trait|type|func|fn|def|function|namespace|module|macro_rules!|impl(<[^>]*>)?( [^ ]+ for)?)\s+` + n + `\b`,
		`\b(public|private|protected|internal|static|async|override|virtual|abstract|export|pub|readonly|const|let|var|val)\b[^=;(]*\b` + n + `\s*[(<{:=]`,
		`\bfunc\s+\([^)]*\)\s+` + n + `\s*\(`,
		`^\s*` + n + `\s*[:=]\s*(async\s*)?(\(|function\b|new\b|class\b)`,
		`\b(resource|data|module|variable|output|locals)\s+("[^"]+"\s+)?"` + n + `"`,
		`^\s*` + n + `\s*:\s*$`,
	}
}

func (r *Repo) Definition(ctx context.Context, name, p string) (string, error) {
	name = strings.TrimSpace(name)
	if !identifier.MatchString(name) {
		return "", errors.New("name must be a single identifier")
	}
	cp, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	args := []string{"grep", "-n", "-I", "-E", "-A", "3", "--max-count=10"}
	for _, pat := range declarationPatterns(name) {
		args = append(args, "-e", pat)
	}
	args = append(args, r.head)
	if cp != "" {
		args = append(args, "--", cp)
	}
	out, err := r.run(ctx, args...)
	if err != nil && len(out) == 0 {
		return fmt.Sprintf("(no declaration of %s at this commit; it may come from a package listed in the project's dependency manifest, in which case the repository cannot tell you how it behaves)\n", name), nil
	}
	prefix := r.head + ":"
	var b strings.Builder
	hits := 0
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l == "--" {
			b.WriteString("\n")
			continue
		}
		l = strings.TrimPrefix(l, prefix)
		if i := strings.Index(l, ":"); i > 0 && len(l) > i+1 {
			if j := strings.IndexAny(l[i+1:], ":-"); j > 0 && l[i+1+j] == ':' {
				hits++
			}
		}
		if hits > maxDefinitions {
			b.WriteString("[more declarations omitted, limit by path]\n")
			break
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	if hits == 0 {
		return fmt.Sprintf("(no declaration of %s at this commit; it may come from a package listed in the project's dependency manifest, in which case the repository cannot tell you how it behaves)\n", name), nil
	}
	return b.String(), nil
}

func (r *Repo) References(ctx context.Context, name, p string) (string, error) {
	name = strings.TrimSpace(name)
	if !identifier.MatchString(name) {
		return "", errors.New("name must be a single identifier")
	}
	cp, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	args := []string{"grep", "-n", "-I", "-w", "-F", "-e", name, r.head}
	if cp != "" {
		args = append(args, "--", cp)
	}
	out, err := r.run(ctx, args...)
	if err != nil && len(out) == 0 {
		return "(no references)\n", nil
	}
	prefix := r.head + ":"
	type hit struct {
		line string
		text string
	}
	files := map[string][]hit{}
	var order []string
	total := 0
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		l = strings.TrimPrefix(l, prefix)
		parts := strings.SplitN(l, ":", 3)
		if len(parts) < 3 {
			continue
		}
		if _, ok := files[parts[0]]; !ok {
			order = append(order, parts[0])
		}
		files[parts[0]] = append(files[parts[0]], hit{parts[1], strings.TrimSpace(parts[2])})
		total++
	}
	if total == 0 {
		return "(no references)\n", nil
	}
	sort.SliceStable(order, func(i, j int) bool { return len(files[order[i]]) > len(files[order[j]]) })
	var b strings.Builder
	fmt.Fprintf(&b, "%d references in %d files\n", total, len(order))
	for i, f := range order {
		if i >= maxRefFiles {
			fmt.Fprintf(&b, "[%d more files omitted, limit by path]\n", len(order)-i)
			break
		}
		hs := files[f]
		fmt.Fprintf(&b, "%s (%d)\n", f, len(hs))
		for j, h := range hs {
			if j >= maxRefLines {
				fmt.Fprintf(&b, "  ... %d more\n", len(hs)-j)
				break
			}
			fmt.Fprintf(&b, "  %s: %s\n", h.line, truncateLine(h.text, 160))
		}
	}
	return b.String(), nil
}

var outlinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*(export\s+(default\s+)?|pub(\([^)]*\))?\s+)?(public|private|protected|internal|static|abstract|sealed|partial|async|override|virtual|readonly|final|unsafe|extern|const|declare)?[\w\s<>\[\],.?*&]*\b(class|interface|struct|enum|record|trait|impl|type|func|fn|def|function|namespace|module|macro_rules!)\b`),
	regexp.MustCompile(`^\s*(public|private|protected|internal)\b[^;]*\)\s*(\{|=>|where\b|$)`),
	regexp.MustCompile(`^\s*(public|private|protected|internal)\b[^=;(]*\{\s*(get|set|init)\b`),
	regexp.MustCompile(`^\s+(public|private|protected|static|async|get|set|readonly)?\s*[A-Za-z_$][\w$]*\s*(<[^>]*>)?\([^)]*\)\s*(:\s*[^{;]+)?\{\s*$`),
	regexp.MustCompile(`^\s*(export\s+)?(const|let|var)\s+[A-Za-z_$][\w$]*\s*(:[^=]+)?=\s*(async\s*)?(\(|function\b|class\b)`),
	regexp.MustCompile(`^\s*(resource|data|module|variable|output|locals|provider|terraform)\b`),
	regexp.MustCompile(`^#{1,4}\s+\S`),
	regexp.MustCompile(`^[A-Za-z_][\w.-]*:(\s|$)`),
	regexp.MustCompile(`^\s*(@[A-Za-z_][\w.]*|\[[A-Z][\w.]*(\(.*\))?\])\s*$`),
}

func Outline(text string) string {
	var b strings.Builder
	n := 0
	for i, l := range strings.Split(text, "\n") {
		t := strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(t) == "" {
			continue
		}
		for _, p := range outlinePatterns {
			if p.MatchString(t) {
				if n >= maxOutline {
					b.WriteString("[more omitted]\n")
					return b.String()
				}
				fmt.Fprintf(&b, "%d: %s\n", i+1, truncateLine(t, 140))
				n++
				break
			}
		}
	}
	if n == 0 {
		return "(no declarations recognised; use read_file)\n"
	}
	return b.String()
}

func (r *Repo) Outline(ctx context.Context, p string) (string, error) {
	text, err := r.ReadFile(ctx, p)
	if err != nil {
		return "", err
	}
	return Outline(text), nil
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
