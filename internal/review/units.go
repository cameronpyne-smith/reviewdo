package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
)

const UnitPrompt = `For each function, first state its contract in two sentences: what it promises its callers and the rest of the system. Then find one concrete sequence of events under which the function breaks that contract. Give the line where it goes wrong.

Severity: critical means it will certainly break, lose data or open a security hole; major means a likely bug; minor means worth fixing but not blocking; nit means optional.`

const ExitsPrompt = `For each function, first state its contract in two sentences: what it promises its callers and the rest of the system. Then list every way it can stop early or fail: an error return, an exception, a timeout, a cancellation, an empty or missing input, a lock or resource not obtained. For each, say what state, resource or caller promise it leaves behind. Report the exits that leave the contract broken, with the line where it goes wrong.

Severity: critical means it will certainly break, lose data or open a security hole; major means a likely bug; minor means worth fixing but not blocking; nit means optional.`

const InputsPrompt = `For each function, first state its contract in two sentences: what it promises its callers and the rest of the system. Then list the inputs and situations it does not handle: empty, missing, malformed, boundary or oversized values, repeated or out-of-order calls, concurrent callers, a clock or counter that has moved. For each, say what the function does with it. Report the ones where what it does breaks the contract, with the line where it goes wrong.

Severity: critical means it will certainly break, lose data or open a security hole; major means a likely bug; minor means worth fixing but not blocking; nit means optional.`

type DrawSpec struct {
	Question    string
	Temperature *float64
	Model       string
	Think       any
}

func (d DrawSpec) prompt() string {
	switch d.Question {
	case "inputs":
		return InputsPrompt
	case "exits":
		return ExitsPrompt
	}
	return UnitPrompt
}

func (d DrawSpec) client(llm *ollama.Client) *ollama.Client {
	if d.Model != "" {
		llm = llm.WithModel(d.Model, d.Think)
	}
	if d.Temperature != nil {
		llm = llm.WithTemperature(*d.Temperature)
	}
	return llm
}

var UnitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "contracts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string"},
          "name": {"type": "string"},
          "contract": {"type": "string"}
        },
        "required": ["path", "name", "contract"]
      }
    },
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string"},
          "line": {"type": "integer"},
          "quote": {"type": "string"},
          "title": {"type": "string"},
          "severity": {"type": "string", "enum": ["critical", "major", "minor", "nit"]},
          "body": {"type": "string"}
        },
        "required": ["path", "line", "quote", "title", "severity", "body"]
      }
    }
  },
  "required": ["contracts", "findings"]
}`)

const (
	maxUnitLines   = 220
	minUnitLines   = 8
	unitMargin     = 40
	unitCallBytes  = 10 << 10
	maxUnitCallers = 1500
	unitCallTime   = 3 * time.Minute
	unitPredict    = 32768
	drawLines      = 5
)

var codeExt = map[string]bool{
	".go": true, ".rs": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".svelte": true, ".vue": true, ".cs": true, ".fs": true, ".py": true, ".rb": true, ".java": true, ".kt": true,
	".kts": true, ".scala": true, ".swift": true, ".php": true, ".c": true, ".cc": true, ".cpp": true, ".h": true,
	".hpp": true, ".m": true, ".sh": true, ".bash": true, ".ps1": true, ".sql": true, ".ex": true, ".exs": true,
	".dart": true, ".lua": true, ".zig": true,
}

var (
	testDir      = regexp.MustCompile(`(?i)(^|/|\.)(tests?|__tests__|specs?|testing|fixtures?|mocks?|testdata|examples?|samples?|docs?)/`)
	testFile     = regexp.MustCompile(`(?i)(_tests?\.\w+|\.test\.\w+|\.spec\.\w+|tests?\.(cs|fs|py|rb|java|kt|scala|swift|rs)|test_[^/]*\.py)$`)
	rustTestAttr = regexp.MustCompile(`^\s*#\[cfg\(test\)\]`)
	rustTestMod  = regexp.MustCompile(`^\s*(pub(\([a-z]+\))?\s+)?mod\s+\w+\s*\{`)
	decoratorRe  = regexp.MustCompile(`^\s*(@[A-Za-z_][\w.]*|\[[A-Z][\w.]*(\(.*\))?\])\s*$`)
	outlineEntry = regexp.MustCompile(`^(\d+): (.*)$`)
)

type unit struct {
	path    string
	name    string
	start   int
	end     int
	changed int
	risk    int
	text    string
}

func (u *unit) label() string {
	if u.name == "" {
		return fmt.Sprintf("%s lines %d-%d", u.path, u.start, u.end)
	}
	return fmt.Sprintf("%s in %s lines %d-%d", u.name, u.path, u.start, u.end)
}

func (u *unit) size() int {
	return len(u.text)
}

func codeFile(p string) bool {
	return codeExt[strings.ToLower(path.Ext(p))] && !testDir.MatchString(p) && !testFile.MatchString(p)
}

type fileChanges struct {
	added   map[int]bool
	removed map[int][]string
}

func changesOf(f *diff.File) fileChanges {
	c := fileChanges{added: map[int]bool{}, removed: map[int][]string{}}
	for _, h := range f.Hunks {
		var pending []string
		for _, l := range h.Lines {
			if l.Kind == '-' {
				pending = append(pending, l.Text)
				continue
			}
			if len(pending) > 0 {
				c.removed[l.NewNo] = append(c.removed[l.NewNo], pending...)
				pending = nil
			}
			if l.Kind == '+' {
				c.added[l.NewNo] = true
			}
		}
		if len(pending) > 0 {
			last := 0
			for _, l := range h.Lines {
				if l.NewNo > last {
					last = l.NewNo
				}
			}
			c.removed[last+1] = append(c.removed[last+1], pending...)
		}
	}
	return c
}

func (c fileChanges) count(start, end int) int {
	n := 0
	for i := start; i <= end; i++ {
		if c.added[i] {
			n++
		}
		n += len(c.removed[i])
	}
	return n
}

func boundaries(outline string, lines []string) []int {
	var starts []int
	pending := 0
	for _, l := range strings.Split(outline, "\n") {
		m := outlineEntry.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n < 1 || n > len(lines) {
			continue
		}
		text := strings.TrimSpace(lines[n-1])
		if decoratorRe.MatchString(text) {
			if pending == 0 || !contiguousDecorators(lines, pending, n) {
				pending = n
			}
			continue
		}
		if declaredName(lines[n-1]) == "" {
			pending = 0
			continue
		}
		start := n
		if pending > 0 && contiguousDecorators(lines, pending, n) {
			start = pending
		}
		pending = 0
		if len(starts) == 0 || start > starts[len(starts)-1] {
			starts = append(starts, start)
		}
	}
	return starts
}

func contiguousDecorators(lines []string, from, to int) bool {
	for i := from; i < to; i++ {
		t := strings.TrimSpace(lines[i-1])
		if t != "" && !decoratorRe.MatchString(t) {
			return false
		}
	}
	return true
}

func renderUnit(lines []string, start, end int) string {
	var b strings.Builder
	for n := start; n <= end; n++ {
		fmt.Fprintf(&b, "%5d   %s\n", n, lines[n-1])
	}
	return b.String()
}

func windows(c fileChanges, start, end int) []Span {
	var spans []Span
	for n := start; n <= end; n++ {
		if !c.added[n] && len(c.removed[n]) == 0 {
			continue
		}
		s := Span{max(start, n-unitMargin), min(end, n+unitMargin)}
		if k := len(spans); k > 0 && s.Start <= spans[k-1].End+1 {
			spans[k-1].End = s.End
			continue
		}
		spans = append(spans, s)
	}
	return spans
}

func unitText(lines []string, c fileChanges, start, end int) string {
	if end-start+1 <= maxUnitLines {
		return renderUnit(lines, start, end)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "(%d lines; only the changed regions are shown)\n", end-start+1)
	ws := windows(c, start, end)
	if len(ws) == 0 || ws[0].Start > start {
		b.WriteString(renderUnit(lines, start, start))
		b.WriteString("      ...\n")
	}
	for i, w := range ws {
		if i > 0 {
			b.WriteString("      ...\n")
		}
		b.WriteString(renderUnit(lines, w.Start, w.End))
	}
	return b.String()
}

func fileUnits(ctx context.Context, repo Repo, f *diff.File) []*unit {
	text, err := repo.ReadFile(ctx, f.Path)
	if err != nil {
		return nil
	}
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return nil
	}
	lines = beforeTests(f.Path, lines)
	c := changesOf(f)
	var starts []int
	if outline, err := repo.Outline(ctx, f.Path); err == nil {
		for _, s := range boundaries(outline, lines) {
			if s <= len(lines) {
				starts = append(starts, s)
			}
		}
	}
	var units []*unit
	add := func(start, end int) {
		if end < start {
			return
		}
		n := c.count(start, end)
		if n == 0 {
			return
		}
		units = append(units, &unit{path: f.Path, name: declaredName(lines[start-1]), start: start, end: end, changed: n, text: unitText(lines, c, start, end)})
	}
	if len(starts) == 0 {
		add(1, len(lines))
		return units
	}
	if starts[0] > 1 {
		add(1, starts[0]-1)
	}
	for i, s := range starts {
		end := len(lines)
		if i+1 < len(starts) {
			end = starts[i+1] - 1
		}
		add(s, end)
	}
	return coalesce(units, lines, c)
}

func beforeTests(p string, lines []string) []string {
	if strings.ToLower(path.Ext(p)) != ".rs" {
		return lines
	}
	for i, l := range lines {
		if rustTestAttr.MatchString(l) && i+1 < len(lines) && rustTestMod.MatchString(lines[i+1]) {
			return lines[:i]
		}
	}
	return lines
}

func coalesce(units []*unit, lines []string, c fileChanges) []*unit {
	var out []*unit
	for _, u := range units {
		if n := len(out); n > 0 {
			prev := out[n-1]
			small := prev.end-prev.start+1 < minUnitLines || u.end-u.start+1 < minUnitLines
			if prev.end+1 == u.start && small && u.end-prev.start+1 <= maxUnitLines {
				prev.end = u.end
				prev.changed += u.changed
				if prev.name == "" {
					prev.name = u.name
				}
				prev.text = unitText(lines, c, prev.start, prev.end)
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

func Units(ctx context.Context, repo Repo, files []*diff.File, ignore, only []string) []*unit {
	var all []*unit
	for _, f := range files {
		if f.Binary || f.Deleted || diff.Ignored(f.Path, ignore) || !codeFile(f.Path) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		all = append(all, fileUnits(ctx, repo, f)...)
	}
	if len(only) > 0 {
		all = selectUnits(all, only)
	}
	for _, u := range all {
		u.risk = risk(u.text)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if (all[i].name == "") != (all[j].name == "") {
			return all[i].name != ""
		}
		if all[i].risk != all[j].risk {
			return all[i].risk > all[j].risk
		}
		return all[i].changed > all[j].changed
	})
	return all
}

var riskRes = []*regexp.Regexp{
	regexp.MustCompile(`\b(await|async|spawn|Task|Thread|lock|Lock|Mutex|mutex|Semaphore|select!|chan|channel|atomic|Interlocked)\b`),
	regexp.MustCompile(`\b(retry|Retry|timeout|Timeout|deadline|Deadline|sleep|Sleep|Duration|TimeSpan|setTimeout|Instant|now\(\)|Now)\b`),
	regexp.MustCompile(`\b(catch|except|rescue|Err|err|error|Error|unwrap|expect|panic|throw|raise|fail|Fail)\b`),
	regexp.MustCompile(`\b(parse|Parse|decode|Decode|deserialize|Deserialize|split|Split|slice|Slice|substring|Substring|indexOf|IndexOf|trim|Trim|match|Match|regex|Regex|RegExp)\b`),
	regexp.MustCompile(`\b(for|foreach|while|loop|continue|break|yield|return null|nil|None|undefined|default)\b`),
	regexp.MustCompile(`\b(read|Read|write|Write|send|Send|recv|receive|Receive|fetch|Fetch|request|Request|exec|Exec|spawn|open|Open|close|Close|dispose|Dispose|commit|Commit|rollback|Rollback)\b`),
}

func risk(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		for _, re := range riskRes {
			if re.MatchString(line) {
				n++
			}
		}
	}
	return n
}

func selectUnits(units []*unit, only []string) []*unit {
	var out []*unit
	for _, u := range units {
		for _, spec := range only {
			path, line, _ := strings.Cut(spec, ":")
			n, _ := strconv.Atoi(line)
			if u.name == spec || (strings.HasSuffix(u.path, path) && (line == "" || (u.start <= n && n <= u.end))) {
				out = append(out, u)
				break
			}
		}
	}
	return out
}

func PackUnits(units []*unit, maxBytes int) [][]*unit {
	var calls [][]*unit
	var cur []*unit
	size := 0
	for _, u := range units {
		if len(cur) > 0 && size+u.size() > maxBytes {
			calls = append(calls, cur)
			cur, size = nil, 0
		}
		cur = append(cur, u)
		size += u.size()
	}
	if len(cur) > 0 {
		calls = append(calls, cur)
	}
	return calls
}

func UnitInput(call []*unit) string {
	var b strings.Builder
	for _, u := range call {
		fmt.Fprintf(&b, "\n### %s\n", u.label())
		b.WriteString(u.text)
	}
	return b.String()
}

func DescribeUnits(units []*unit, calls [][]*unit) string {
	if len(units) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Units for the deep pass, %d in %d calls:\n", len(units), len(calls))
	for i, call := range calls {
		size := 0
		for _, u := range call {
			size += u.size()
		}
		fmt.Fprintf(&b, "call %d (%d KB):\n", i+1, (size+512)>>10)
		for _, u := range call {
			fmt.Fprintf(&b, "  - %s: risk %d, %d changed lines, %d bytes\n", u.label(), u.risk, u.changed, len(u.text))
		}
	}
	return b.String()
}

type unitResult struct {
	Contracts []struct {
		Path     string `json:"path"`
		Name     string `json:"name"`
		Contract string `json:"contract"`
	} `json:"contracts"`
	Findings []Comment `json:"findings"`
}

func reviewUnitCall(ctx context.Context, llm *ollama.Client, prompt string, call []*unit, log *slog.Logger) ([]Comment, Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, unitCallTime)
	defer cancel()
	raw, st, err := RunSingle(ctx, llm.WithMinPredict(unitPredict), prompt, UnitInput(call), UnitSchema)
	if err != nil {
		return nil, st, err
	}
	var r unitResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, st, fmt.Errorf("unit output invalid: %w", err)
	}
	for _, c := range r.Contracts {
		log.Debug("contract", "path", c.Path, "name", c.Name, "contract", truncate(c.Contract, 200))
	}
	inCall := map[string]bool{}
	for _, u := range call {
		inCall[u.path] = true
	}
	var out []Comment
	for _, c := range r.Findings {
		c.Path = strings.TrimSpace(c.Path)
		if !inCall[c.Path] || c.Line <= 0 || strings.TrimSpace(c.Body) == "" {
			continue
		}
		if _, ok := severityRank[c.Severity]; !ok {
			c.Severity = "minor"
		}
		log.Debug("unit finding", "path", c.Path, "line", c.Line, "severity", c.Severity, "title", c.Title)
		out = append(out, c)
	}
	return out, st, nil
}

type UnitOutcome struct {
	Units    int
	Calls    int
	Draws    int
	Reviewed int
	Agreed   int
	Comments []Comment
	Stats    Stats
}

func ReviewUnits(ctx context.Context, llm *ollama.Client, units []*unit, draws []DrawSpec, deadline time.Time, log *slog.Logger) UnitOutcome {
	if len(draws) == 0 {
		draws = []DrawSpec{{}}
	}
	oc := UnitOutcome{Units: len(units), Draws: len(draws)}
	index := map[*unit]int{}
	for i, u := range units {
		index[u] = i
	}
	seen := make([]int, len(units))
	var found []Comment
	var owner []int
draws:
	for d := 1; d <= oc.Draws; d++ {
		calls := PackUnits(drawOrder(units, d, oc.Draws), unitCallBytes)
		if d == 1 {
			oc.Calls = len(calls)
		}
		for i, call := range calls {
			if ctx.Err() != nil || time.Now().After(deadline) {
				log.Warn("no time left for the deep pass, skipping the remaining calls", "draw", d, "reviewed", i, "skipped", len(calls)-i)
				break draws
			}
			var names []string
			for _, u := range call {
				names = append(names, u.label())
			}
			started := time.Now()
			spec := draws[d-1]
			comments, st, err := reviewUnitCall(ctx, spec.client(llm), spec.prompt(), call, log.With("units", i+1, "draw", d))
			oc.Stats.Merge(st)
			if d == 1 {
				oc.Reviewed++
			}
			if err != nil {
				log.Warn("deep pass call failed", "draw", d, "units", names, "err", err)
				continue
			}
			for _, u := range call {
				seen[index[u]]++
			}
			log.Info("units reviewed", "call", i+1, "of", len(calls), "draw", d, "model", spec.client(llm).Model(), "units", names, "findings", len(comments), "rounds", st.Rounds, "output_tokens", st.OutputTokens, "truncated", st.Truncated, "took", time.Since(started).Round(time.Second))
			found, owner = mergeDraw(found, owner, comments, call, index, d)
		}
	}
	for i, c := range found {
		c.OfDraws = seen[owner[i]]
		if c.Draws > 1 {
			oc.Agreed++
		}
		oc.Comments = append(oc.Comments, c)
	}
	return oc
}

func drawOrder(units []*unit, draw, draws int) []*unit {
	off := (draw - 1) * len(units) / draws
	if off == 0 {
		return units
	}
	return append(append([]*unit{}, units[off:]...), units[:off]...)
}

func mergeDraw(have []Comment, owner []int, more []Comment, call []*unit, index map[*unit]int, draw int) ([]Comment, []int) {
	for _, m := range more {
		merged := false
		for i := range have {
			c := &have[i]
			if c.Path != m.Path || abs(c.Line-m.Line) > drawLines {
				continue
			}
			c.Draws++
			if severityRank[m.Severity] < severityRank[c.Severity] {
				c.Severity = m.Severity
			}
			if len(m.Body) > len(c.Body) {
				c.Line, c.Quote, c.Title, c.Body = m.Line, m.Quote, m.Title, m.Body
			}
			merged = true
			break
		}
		if merged {
			continue
		}
		m.Draws, m.FirstDraw = 1, draw
		have = append(have, m)
		owner = append(owner, index[unitOf(call, m)])
	}
	return have, owner
}

func unitOf(call []*unit, c Comment) *unit {
	for _, u := range call {
		if u.path == c.Path && u.start <= c.Line && c.Line <= u.end {
			return u
		}
	}
	for _, u := range call {
		if u.path == c.Path {
			return u
		}
	}
	return call[0]
}

func AddComments(comments, more []Comment) ([]Comment, int) {
	added := 0
	for _, m := range more {
		dup := false
		for _, c := range comments {
			if c.Path == m.Path && abs(c.Line-m.Line) <= 2 {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		comments = append(comments, m)
		added++
	}
	return comments, added
}
