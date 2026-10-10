package review

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

import "github.com/cameronpyne-smith/reviewdo/internal/diff"

const (
	maxRemovedCandidates = 60
	maxNewCandidates     = 150
	maxCheckLines      = 12
	maxCheckFiles      = 4
	commonCheckFiles   = 20
	checksTimeout      = 90 * time.Second
)

var (
	tokenRe     = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.-]*[A-Za-z0-9_]`)
	plainIdent  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	versionRe   = regexp.MustCompile(`^v?\d+(\.\d+)+$`)
	proseWord   = regexp.MustCompile(`^[A-Z][a-z]+$`)
	hexRe       = regexp.MustCompile(`^[0-9a-f]{7,}$`)
	urlRe       = regexp.MustCompile(`[a-z][a-z0-9+.-]*://\S+|mailto:\S+`)
	mdLinkRe    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	pathRe      = regexp.MustCompile(`(?:\.{1,2}/)?(?:[\w.-]+/)+[\w.-]+`)
	extRe       = regexp.MustCompile(`\.[a-z][a-z0-9]{0,4}$`)
	domainRe    = regexp.MustCompile(`^[\w-]+\.[a-z]{2,4}$`)
	quotedRe    = regexp.MustCompile("\"[^\"]*\"|'[^']*'|`[^`]*`")
	keyValueRe  = regexp.MustCompile(`^\s*-?\s*"?[A-Za-z_][\w.-]*"?\s*[:=]\s*(.*)$`)
	camelSplit  = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	valueFiles  = map[string]bool{".yaml": true, ".yml": true, ".json": true, ".toml": true, ".properties": true, ".env": true, ".tf": true, ".hcl": true, ".ini": true, ".conf": true}
	dataLineKey = regexp.MustCompile(`^\s*-?\s*"?[A-Za-z_][\w.-]*"?\s*[:=]`)
)

func identifierLike(tok string) bool {
	if len(tok) < 3 || len(tok) > 60 || versionRe.MatchString(tok) || hexRe.MatchString(tok) || proseWord.MatchString(tok) || strings.Contains(tok, "..") {
		return false
	}
	if len(tok) > 32 && !strings.ContainsAny(tok, "_.-") {
		return false
	}
	lower, upper, other := false, false, false
	for _, c := range tok {
		switch {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		default:
			other = true
		}
	}
	return other || lower && upper
}

func tokens(line string) []string {
	return tokenRe.FindAllString(urlRe.ReplaceAllString(line, " "), -1)
}

func (u uses) common() bool { return u.count >= commonUses || len(u.files) >= commonFiles }

func (u uses) shown() string {
	s := u.files
	more := 0
	if len(s) > maxCheckFiles {
		more = len(s) - maxCheckFiles
		s = s[:maxCheckFiles]
	}
	out := strings.Join(s, ", ")
	if more > 0 {
		out += fmt.Sprintf(", %d more files", more)
	}
	return out
}

const grepBatch = 20

func wordPattern(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = regexp.QuoteMeta(n)
	}
	return `\b(` + strings.Join(parts, "|") + `)\b`
}

func countsOf(lines []string, match func(text string) []string) map[string]uses {
	counts := map[string]map[string]int{}
	order := map[string][]string{}
	for _, l := range lines {
		parts := strings.SplitN(l, ":", 3)
		if len(parts) < 3 {
			continue
		}
		for _, name := range match(parts[2]) {
			if counts[name] == nil {
				counts[name] = map[string]int{}
			}
			if counts[name][parts[0]] == 0 {
				order[name] = append(order[name], parts[0])
			}
			counts[name][parts[0]]++
		}
	}
	out := map[string]uses{}
	for name, files := range counts {
		u := uses{}
		fs := order[name]
		sort.SliceStable(fs, func(i, j int) bool { return files[fs[i]] > files[fs[j]] })
		for _, f := range fs {
			u.count += files[f]
			u.files = append(u.files, fmt.Sprintf("%s (%d)", f, files[f]))
		}
		out[name] = u
	}
	return out
}

func usesOfAll(ctx context.Context, repo Repo, names []string) map[string]uses {
	out := map[string]uses{}
	for i := 0; i < len(names); i += grepBatch {
		if ctx.Err() != nil {
			break
		}
		batch := names[i:min(i+grepBatch, len(names))]
		re := regexp.MustCompile(wordPattern(batch))
		lines, err := repo.Grep(ctx, wordPattern(batch), 5000)
		if err != nil {
			continue
		}
		for name, u := range countsOf(lines, func(text string) []string {
			seen := map[string]bool{}
			var found []string
			for _, m := range re.FindAllString(text, -1) {
				if !seen[m] {
					seen[m] = true
					found = append(found, m)
				}
			}
			return found
		}) {
			out[name] = u
		}
		if len(lines) >= 5000 {
			for _, n := range batch {
				if _, ok := out[n]; !ok {
					out[n] = uses{}
				}
			}
		}
	}
	return out
}

func fileOf(entry string) string {
	i := strings.LastIndex(entry, " (")
	if i < 0 {
		return entry
	}
	return entry[:i]
}

type exists struct {
	ctx  context.Context
	repo Repo
	dirs map[string]map[string]bool
}

func (e *exists) has(p string) bool {
	p = strings.TrimPrefix(path.Clean(p), "./")
	if p == "" || p == "." || strings.HasPrefix(p, "../") {
		return false
	}
	dir, name := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	entries, ok := e.dirs[dir]
	if !ok {
		entries = map[string]bool{}
		if out, err := e.repo.ListDir(e.ctx, dir); err == nil {
			for _, l := range strings.Split(out, "\n") {
				entries[strings.TrimSuffix(strings.TrimSpace(l), "/")] = true
			}
		}
		e.dirs[dir] = entries
	}
	return entries[name]
}

func pathCandidates(line string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimRight(strings.TrimSpace(p), ".,;:)")
		if i := strings.IndexAny(p, "#?"); i >= 0 {
			p = p[:i]
		}
		if p == "" || seen[p] || strings.ContainsAny(p, "*{}$%\\<>|") || strings.Contains(p, "://") || strings.HasPrefix(p, "/") {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, m := range mdLinkRe.FindAllStringSubmatch(line, -1) {
		if strings.Contains(m[1], "/") || strings.Contains(m[1], ".") {
			add(m[1])
		}
	}
	rest := mdLinkRe.ReplaceAllString(urlRe.ReplaceAllString(line, " "), " ")
	for _, idx := range pathRe.FindAllStringIndex(rest, -1) {
		m := rest[idx[0]:idx[1]]
		if idx[0] > 0 && strings.ContainsRune("/:@", rune(rest[idx[0]-1])) {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimLeft(m, "./"), "/")
		if domainRe.MatchString(first) {
			continue
		}
		if !strings.HasPrefix(m, "./") && !strings.HasPrefix(m, "../") && !extRe.MatchString(m) {
			continue
		}
		add(m)
	}
	return out
}

func missingPaths(ctx context.Context, repo Repo, files []*diff.File) string {
	e := &exists{ctx: ctx, repo: repo, dirs: map[string]map[string]bool{}}
	var b strings.Builder
	n, checked := 0, 0
	seen := map[string]bool{}
	for _, f := range files {
		if f.Binary || f.Deleted {
			continue
		}
		dir := path.Dir(f.Path)
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != '+' || ctx.Err() != nil {
					continue
				}
				for _, p := range pathCandidates(l.Text) {
					if seen[p] {
						continue
					}
					seen[p] = true
					checked++
					if e.has(p) || e.has(path.Join(dir, p)) {
						continue
					}
					if n >= maxCheckLines {
						b.WriteString("[more missing paths not listed]\n")
						return b.String()
					}
					n++
					where := "from the repository root"
					if dir != "." {
						where += " nor relative to " + dir + "/"
					}
					fmt.Fprintf(&b, "- %s:%d: %s does not exist at the head commit, neither %s\n", f.Path, l.NewNo, p, where)
				}
			}
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

func diffTokens(files []*diff.File, kinds string) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		if f.Binary {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if !strings.ContainsRune(kinds, rune(l.Kind)) {
					continue
				}
				for _, t := range tokens(l.Text) {
					out[t] = true
				}
			}
		}
	}
	return out
}

type candidate struct {
	name string
	path string
	line int
}

func removedNames(ctx context.Context, repo Repo, files []*diff.File) string {
	kept := diffTokens(files, "+ ")
	seen := map[string]bool{}
	var cands []candidate
	for _, f := range files {
		if f.Binary {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != '-' {
					continue
				}
				decl := declaredName(l.Text)
				for _, t := range removedTokens(f.Path, l.Text) {
					if seen[t] || kept[t] || t == decl || !identifierLike(t) {
						continue
					}
					seen[t] = true
					cands = append(cands, candidate{t, f.Path, 0})
				}
			}
		}
	}
	var b strings.Builder
	more := 0
	if len(cands) > maxRemovedCandidates {
		more = len(cands) - maxRemovedCandidates
		cands = cands[:maxRemovedCandidates]
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.name
	}
	all := usesOfAll(ctx, repo, names)
	for _, c := range cands {
		u := all[c.name]
		if u.count == 0 || u.common() {
			continue
		}
		if len(u.files) == 1 && fileOf(u.files[0]) == c.path && u.count > 3 {
			continue
		}
		fmt.Fprintf(&b, "- %s (removed from %s): still used, %d uses in %d files: %s\n", c.name, c.path, u.count, len(u.files), u.shown())
	}
	if more > 0 {
		fmt.Fprintf(&b, "[%d more removed names not checked; use references]\n", more)
	}
	return b.String()
}

func words(name string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(camelSplit.ReplaceAllString(name, "$1 $2"), func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == ' ' }) {
		out = append(out, strings.ToLower(part))
	}
	return out
}

func variants(name string) []string {
	w := words(name)
	if len(w) == 0 {
		return nil
	}
	title := func(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
	var camel, pascal strings.Builder
	for i, p := range w {
		if i == 0 {
			camel.WriteString(p)
		} else {
			camel.WriteString(title(p))
		}
		pascal.WriteString(title(p))
	}
	all := []string{
		strings.ToUpper(name), strings.ToLower(name),
		strings.Join(w, "_"), strings.Join(w, "-"), strings.Join(w, "."), strings.ToUpper(strings.Join(w, "_")),
		camel.String(), pascal.String(), strings.Join(w, ""),
	}
	var out []string
	seen := map[string]bool{name: true}
	for _, v := range all {
		if !seen[v] && len(v) >= 3 {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func ci(word string) string {
	var b strings.Builder
	for _, c := range word {
		switch {
		case c >= 'a' && c <= 'z':
			fmt.Fprintf(&b, "[%c%c]", c, c-'a'+'A')
		case c >= 'A' && c <= 'Z':
			fmt.Fprintf(&b, "[%c%c]", c-'A'+'a', c)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

func familyPattern(name string) string {
	w := words(name)
	if len(w) < 2 {
		return ""
	}
	sep := `[_.-]?`
	var middle []string
	for _, p := range w[:len(w)-1] {
		middle = append(middle, ci(p))
	}
	return ci(w[0]) + sep + `[A-Za-z0-9_.-]*` + sep + ci(w[len(w)-1]) + `|` + strings.Join(middle, sep) + sep + `[A-Za-z0-9]+`
}

const familyBatch = 10

func families(ctx context.Context, repo Repo, names []string) map[string][]string {
	out := map[string][]string{}
	type entry struct {
		name string
		re   *regexp.Regexp
	}
	var batch []entry
	var patterns []string
	flush := func() {
		if len(batch) == 0 {
			return
		}
		lines, err := repo.Grep(ctx, `\b(`+strings.Join(patterns, "|")+`)\b`, 2000)
		if err == nil {
			for _, e := range batch {
				seen := map[string]bool{strings.ToLower(e.name): true}
				n := len(words(e.name))
				for _, l := range lines {
					parts := strings.SplitN(l, ":", 3)
					if len(parts) < 3 {
						continue
					}
					for _, m := range e.re.FindAllString(parts[2], -1) {
						if !seen[strings.ToLower(m)] && len(words(m)) == n {
							seen[strings.ToLower(m)] = true
							out[e.name] = append(out[e.name], m)
						}
					}
				}
			}
		}
		batch, patterns = nil, nil
	}
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		p := familyPattern(name)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(`\b(` + p + `)\b`)
		if err != nil {
			continue
		}
		batch = append(batch, entry{name, re})
		patterns = append(patterns, p)
		if len(batch) >= familyBatch {
			flush()
		}
	}
	flush()
	return out
}

func referencedTokens(p, line string) []string {
	if valueFiles[path.Ext(p)] {
		if m := keyValueRe.FindStringSubmatch(strings.TrimLeft(line, " \t{[,")); m != nil {
			return tokens(strings.Trim(m[1], "\"'`}], "))
		}
		return nil
	}
	var out []string
	for _, q := range quotedRe.FindAllString(line, -1) {
		out = append(out, tokens(q[1:len(q)-1])...)
	}
	return out
}

func removedTokens(p, line string) []string {
	if !valueFiles[path.Ext(p)] {
		return nil
	}
	if m := dataLineKey.FindStringSubmatch(line); m != nil {
		return tokens(m[0])
	}
	return nil
}

func similar(a, b string) bool {
	wa, wb := words(a), words(b)
	if len(wa) != len(wb) {
		return false
	}
	diffs := 0
	for i := range wa {
		if wa[i] == wb[i] {
			continue
		}
		diffs++
		n := 0
		for n < len(wa[i]) && n < len(wb[i]) && wa[i][n] == wb[i][n] {
			n++
		}
		if n < 3 || abs(len(wa[i])-len(wb[i])) > 4 {
			return false
		}
	}
	return diffs <= 1
}

func nearMisses(ctx context.Context, repo Repo, files []*diff.File) string {
	old := diffTokens(files, "- ")
	added := diffTokens(files, "+")
	changed := map[string]bool{}
	for _, f := range files {
		changed[f.Path] = true
	}
	seen := map[string]bool{}
	var cands []candidate
	for _, f := range files {
		if f.Binary {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != '+' || declaredName(l.Text) != "" {
					continue
				}
				for _, t := range referencedTokens(f.Path, l.Text) {
					if seen[t] || old[t] || !identifierLike(t) {
						continue
					}
					seen[t] = true
					cands = append(cands, candidate{t, f.Path, l.NewNo})
				}
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return valueFiles[path.Ext(cands[i].path)] && !valueFiles[path.Ext(cands[j].path)] })
	var b strings.Builder
	more := 0
	if len(cands) > maxNewCandidates {
		more = len(cands) - maxNewCandidates
		cands = cands[:maxNewCandidates]
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.name
	}
	all := usesOfAll(ctx, repo, names)
	var orphans []candidate
	variantsOf := map[string][]string{}
	var allVariants []string
	for _, c := range cands {
		u := all[c.name]
		if u.common() {
			continue
		}
		elsewhere := false
		for _, f := range u.files {
			if !changed[fileOf(f)] {
				elsewhere = true
				break
			}
		}
		if elsewhere {
			continue
		}
		orphans = append(orphans, c)
		for _, v := range variants(c.name) {
			if added[v] && !strings.EqualFold(v, c.name) || firstCaseOnly(v, c.name) {
				continue
			}
			variantsOf[c.name] = append(variantsOf[c.name], v)
			allVariants = append(allVariants, v)
		}
	}
	variantUses := usesOfAll(ctx, repo, allVariants)
	var needFamily []string
	nearOf := map[string][]string{}
	for _, c := range orphans {
		for _, v := range variantsOf[c.name] {
			if vu, ok := variantUses[v]; ok && vu.count > 0 && (!vu.common() || strings.EqualFold(v, c.name)) && (len(c.name) >= 5 || strings.EqualFold(v, c.name)) {
				nearOf[c.name] = append(nearOf[c.name], fmt.Sprintf("%s (%d files)", v, len(vu.files)))
			}
		}
		if len(nearOf[c.name]) == 0 {
			needFamily = append(needFamily, c.name)
		}
	}
	for name, ms := range families(ctx, repo, needFamily) {
		for _, m := range ms {
			if added[m] || len(name) < 5 || !similar(name, m) {
				continue
			}
			nearOf[name] = append(nearOf[name], m)
		}
	}
	n := 0
	for _, c := range orphans {
		near := nearOf[c.name]
		if len(near) == 0 {
			continue
		}
		if n >= maxCheckLines {
			b.WriteString("[more not listed]\n")
			break
		}
		n++
		if len(near) > 3 {
			near = near[:3]
		}
		fmt.Fprintf(&b, "- %s (%s:%d): used nowhere else in the repository; it already uses %s\n", c.name, c.path, c.line, strings.Join(near, ", "))
	}
	if more > 0 {
		fmt.Fprintf(&b, "[%d more new names not checked]\n", more)
	}
	return b.String()
}

func firstCaseOnly(a, b string) bool {
	return len(a) == len(b) && len(a) > 1 && a[1:] == b[1:] && strings.EqualFold(a[:1], b[:1]) && a != b
}

var wordChar = regexp.MustCompile(`[A-Za-z0-9]`)

func environments(files []*diff.File, sets map[string]*siblingSet) string {
	var b strings.Builder
	n := 0
	for _, f := range files {
		set := sets[f.Path]
		if set == nil || f.Binary {
			continue
		}
		var names []string
		for _, p := range set.paths {
			name := strings.TrimSuffix(strings.TrimPrefix(p, set.parent+"/"), "/"+set.rel)
			if len(name) >= 3 && !strings.Contains(name, set.self) && !strings.Contains(set.self, name) {
				names = append(names, name)
			}
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != '+' {
					continue
				}
				for _, name := range names {
					if !mentions(l.Text, name) {
						continue
					}
					if n >= maxCheckLines {
						b.WriteString("[more not listed]\n")
						return b.String()
					}
					n++
					fmt.Fprintf(&b, "- %s:%d names %q, a sibling of %s/ under %s/: %s\n", f.Path, l.NewNo, name, set.self, set.parent, strings.TrimSpace(l.Text))
					break
				}
			}
		}
	}
	return b.String()
}

func mentions(line, name string) bool {
	lower := strings.ToLower(line)
	name = strings.ToLower(name)
	for i := 0; ; {
		j := strings.Index(lower[i:], name)
		if j < 0 {
			return false
		}
		s, e := i+j, i+j+len(name)
		before := s == 0 || !wordChar.MatchString(lower[s-1:s])
		after := e == len(lower) || !wordChar.MatchString(lower[e:e+1])
		if before && after {
			return true
		}
		i = s + 1
	}
}

func SiblingSets(ctx context.Context, repo Repo, files []*diff.File) map[string]*siblingSet {
	sets := map[string]*siblingSet{}
	n := 0
	for _, f := range files {
		if f.Binary || f.Deleted || !strings.Contains(f.Path, "/") || ctx.Err() != nil || n >= maxSiblingFiles {
			continue
		}
		if set := findSiblings(ctx, repo, f.Path); set != nil {
			sets[f.Path] = set
			n++
		}
	}
	return sets
}

func Checks(ctx context.Context, repo Repo, files []*diff.File, sets map[string]*siblingSet) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, checksTimeout)
	defer cancel()
	type section struct {
		title, intro, body string
		run                func(context.Context) string
		took               time.Duration
	}
	sections := []*section{
		{title: "Paths that do not exist", intro: "File paths and links written in added lines whose target is not in the repository at the head commit (links to other sites are not checked):", run: func(ctx context.Context) string { return missingPaths(ctx, repo, files) }},
		{title: "Removed keys still used", intro: "Keys this change removes from configuration, workflow, translation or data files (inputs, settings, variables, message keys) that the repository still uses at the head commit (git grep, so comments and strings count):", run: func(ctx context.Context) string { return removedNames(ctx, repo, files) }},
		{title: "New names with a near match", intro: "Names in added strings and configuration values that occur nowhere else in the repository, where the repository already uses a differently spelled form of the same name:", run: func(ctx context.Context) string { return nearMisses(ctx, repo, files) }},
		{title: "Sibling names in added lines", intro: "Added lines in a file under one of several sibling directories that name a different sibling:", run: func(context.Context) string { return environments(files, sets) }},
	}
	var wg sync.WaitGroup
	for _, s := range sections {
		wg.Add(1)
		go func(s *section) {
			defer wg.Done()
			start := time.Now()
			s.body = s.run(ctx)
			s.took = time.Since(start)
		}(s)
	}
	wg.Wait()
	var b, timing strings.Builder
	for i, s := range sections {
		if i > 0 {
			timing.WriteString(", ")
		}
		fmt.Fprintf(&timing, "%s %s", strings.ToLower(strings.Fields(s.title)[0]), s.took.Round(time.Millisecond))
		if s.body == "" {
			continue
		}
		fmt.Fprintf(&b, "%s. %s\n%s", s.title, s.intro, s.body)
	}
	if b.Len() == 0 {
		return "", timing.String()
	}
	return "Checks run in code before this review, at the head commit. Each line is a fact to confirm and report, not a finding yet; a path can legitimately point outside this repository, a remaining use can be a comment, and a new spelling can be deliberate.\n" + b.String(), timing.String()
}
