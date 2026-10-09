package review

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

const (
	maxSymbols     = 30
	maxSymbolFiles = 6
	commonUses     = 100
	commonFiles    = 20
)

var declarations = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:class|interface|struct|enum|record|trait|type|func|fn|def|function)\s+(?:\([^)]*\)\s+)?([A-Za-z_]\w*)`),
	regexp.MustCompile(`^\s*(?:(?:public|private|protected|internal|static|async|override|virtual|abstract|sealed|partial|unsafe|extern|new|readonly|export|default)\s+)+[\w<>\[\],.?*&]+\s+([A-Za-z_]\w*)\s*(?:<[^>]*>)?\s*[({=]`),
	regexp.MustCompile(`^(?:export\s+)?(?:const|let)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:\(|function\b|class\b|new\b|writable\b|derived\b|readable\b)`),
	regexp.MustCompile(`^\s+(?:(?:public|private|protected|static|async|readonly|get|set)\s+)*([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\([^)]*\)\s*(?::\s*[^{;]+)?\{\s*$`),
	regexp.MustCompile(`\b(?:resource|data|module|variable|output)\s+(?:"[^"]+"\s+)?"([A-Za-z_][\w-]*)"`),
}

var notSymbols = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true, "new": true, "else": true,
	"catch": true, "function": true, "class": true, "async": true, "await": true, "get": true, "set": true,
	"init": true, "main": true, "Main": true, "default": true, "constructor": true, "this": true, "self": true,
	"super": true, "string": true, "int": true, "var": true, "let": true, "const": true, "void": true,
	"bool": true, "true": true, "false": true, "null": true, "type": true, "func": true, "def": true,
	"ToString": true, "Equals": true, "GetHashCode": true, "Dispose": true, "toString": true, "using": true,
	"import": true, "export": true, "from": true, "with": true, "when": true, "where": true, "then": true,
}

func declaredName(line string) string {
	t := strings.TrimRight(line, " \t\r")
	lead := strings.TrimSpace(t)
	if lead == "" || strings.HasPrefix(lead, "//") || strings.HasPrefix(lead, "#") || strings.HasPrefix(lead, "*") || strings.HasPrefix(lead, "/*") {
		return ""
	}
	for _, p := range declarations {
		if m := p.FindStringSubmatch(t); m != nil {
			name := m[1]
			if len(name) < 3 || notSymbols[name] || len(name) > 30 && strings.Count(name, "_") >= 3 {
				return ""
			}
			return name
		}
	}
	return ""
}

type symbol struct {
	name    string
	path    string
	added   bool
	removed bool
}

func (s symbol) state() string {
	switch {
	case s.added && s.removed:
		return "modified"
	case s.removed:
		return "removed"
	}
	return "new"
}

func (s symbol) rank() int {
	switch s.state() {
	case "removed":
		return 0
	case "modified":
		return 1
	}
	return 2
}

func changedSymbols(files []*diff.File) []*symbol {
	var order []*symbol
	byName := map[string]*symbol{}
	for _, f := range files {
		if f.Binary {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != '+' && l.Kind != '-' {
					continue
				}
				name := declaredName(l.Text)
				if name == "" {
					continue
				}
				s := byName[name]
				if s == nil {
					s = &symbol{name: name, path: f.Path}
					byName[name] = s
					order = append(order, s)
				}
				if l.Kind == '+' {
					s.added = true
				} else {
					s.removed = true
				}
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].rank() < order[j].rank() })
	return order
}

type uses struct {
	count int
	files []string
}

func parseReferences(out string) uses {
	var u uses
	for i, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if i == 0 {
			fields := strings.Fields(l)
			if len(fields) > 0 {
				u.count, _ = strconv.Atoi(fields[0])
			}
			continue
		}
		if l == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "[") {
			continue
		}
		u.files = append(u.files, l)
	}
	return u
}

func Symbols(ctx context.Context, repo Repo, files []*diff.File) string {
	syms := changedSymbols(files)
	if len(syms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Declarations this change adds, alters or removes, with every use of the name in the repository at the head commit (git grep -w, so comments and strings count):\n")
	for i, s := range syms {
		if i >= maxSymbols {
			fmt.Fprintf(&b, "[%d more declarations not listed; use references]\n", len(syms)-i)
			break
		}
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(&b, "- %s (%s, %s): ", s.name, s.path, s.state())
		out, err := repo.References(ctx, s.name, "")
		if err != nil {
			b.WriteString("references unavailable\n")
			continue
		}
		u := parseReferences(out)
		switch {
		case u.count == 0 && s.removed && !s.added:
			b.WriteString("no remaining uses\n")
		case u.count == 0:
			b.WriteString("no uses found\n")
		case u.count >= commonUses || len(u.files) >= commonFiles:
			fmt.Fprintf(&b, "a common name, %d uses in %d+ files; narrow with references(name, path)\n", u.count, len(u.files))
		case s.added && len(u.files) == 1 && u.files[0] == fmt.Sprintf("%s (%d)", s.path, u.count):
			fmt.Fprintf(&b, "used only in its own file (%d)\n", u.count)
		default:
			if s.removed && !s.added {
				fmt.Fprintf(&b, "still used, %d uses in %d files: ", u.count, len(u.files))
			} else {
				fmt.Fprintf(&b, "%d uses in %d files: ", u.count, len(u.files))
			}
			shown := u.files
			more := 0
			if len(shown) > maxSymbolFiles {
				more = len(shown) - maxSymbolFiles
				shown = shown[:maxSymbolFiles]
			}
			b.WriteString(strings.Join(shown, ", "))
			if more > 0 {
				fmt.Fprintf(&b, ", %d more files", more)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("Read the uses before judging a change to a signature, behaviour or contract. A removed or renamed declaration that is still used is a finding unless every remaining use is a comment, a string or a different declaration of the same name.\n")
	return b.String()
}
