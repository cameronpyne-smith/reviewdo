package review

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
	"github.com/cameronpyne-smith/reviewdo/internal/gitrepo"
)

type outlineRepo struct{ treeRepo }

func (r outlineRepo) Outline(ctx context.Context, p string) (string, error) {
	text, err := r.ReadFile(ctx, p)
	if err != nil {
		return "", err
	}
	return gitrepo.Outline(text), nil
}

func (r outlineRepo) References(ctx context.Context, name, _ string) (string, error) {
	lines, _ := r.Grep(ctx, `\b`+name+`\b`, 100)
	if len(lines) == 0 {
		return "(no references)\n", nil
	}
	var b strings.Builder
	b.WriteString("1 references in 1 files\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String(), nil
}

const unitsFile = `package main

import "fmt"

func first(a int) int {
	if a > 0 {
		return a
	}
	return -a
}

func second(b string) string {
	return fmt.Sprintf("%s!", b)
}

func third() {
	fmt.Println(second("x"))
}
`

func TestUnitsSplitsAtFunctionsAndMarksChanges(t *testing.T) {
	repo := outlineRepo{treeRepo{"main.go": unitsFile, "other.go": "package main\n\nfunc other() { second(\"y\") }\n"}}
	f := &diff.File{Path: "main.go", Hunks: []diff.Hunk{{Lines: []diff.Line{
		{Kind: ' ', Text: "func second(b string) string {", NewNo: 12},
		{Kind: '-', Text: "\treturn b"},
		{Kind: '+', Text: "\treturn fmt.Sprintf(\"%s!\", b)", NewNo: 13},
		{Kind: ' ', Text: "}", NewNo: 14},
	}}}}
	units := Units(context.Background(), repo, []*diff.File{f}, nil, nil)
	if len(units) != 1 {
		t.Fatalf("want 1 unit, got %d: %+v", len(units), units)
	}
	u := units[0]
	if u.name != "second" || u.start != 12 || u.end != 15 || u.changed != 2 {
		t.Errorf("unexpected unit %+v", u)
	}
	for _, want := range []string{"   13   \treturn fmt.Sprintf", "   12   func second"} {
		if !strings.Contains(u.text, want) {
			t.Errorf("missing %q in:\n%s", want, u.text)
		}
	}
	if strings.Contains(u.text, "return b\n") {
		t.Errorf("removed line shown:\n%s", u.text)
	}
}

func TestUnitsSkipsTestsAndNonCode(t *testing.T) {
	repo := outlineRepo{treeRepo{"a_test.go": unitsFile, "Proj.Tests/X.cs": "x", "docs/a.md": "# a", "examples/e.py": "def e():\n    pass\n", "src/sync/shutdown_tests.rs": "fn a() {}\n", "src/sync/position/tests.rs": "fn b() {}\n"}}
	var files []*diff.File
	for p := range repo.treeRepo {
		files = append(files, &diff.File{Path: p, Hunks: []diff.Hunk{{Lines: []diff.Line{{Kind: '+', Text: "x", NewNo: 1}}}}})
	}
	if units := Units(context.Background(), repo, files, nil, nil); len(units) != 0 {
		t.Errorf("expected no units, got %d", len(units))
	}
}

func TestUnitTextWindowsLargeUnits(t *testing.T) {
	var lines []string
	for i := 1; i <= 400; i++ {
		lines = append(lines, "line")
	}
	c := fileChanges{added: map[int]bool{300: true}, removed: map[int][]string{}}
	text := unitText(lines, c, 1, 400)
	if !strings.Contains(text, "only the changed regions") || !strings.Contains(text, "  300   line") || strings.Contains(text, "  200   line") {
		t.Errorf("unexpected window:\n%s", text)
	}
	if !strings.HasPrefix(strings.SplitN(text, "\n", 3)[1], "    1   line") {
		t.Errorf("declaration line not shown first:\n%s", text)
	}
}

func TestCoalesceMergesTinyAdjacentUnits(t *testing.T) {
	lines := strings.Split(strings.Repeat("x\n", 30), "\n")
	c := fileChanges{added: map[int]bool{2: true, 5: true, 20: true}, removed: map[int][]string{}}
	units := []*unit{
		{path: "a", name: "p", start: 1, end: 3, changed: 1},
		{path: "a", name: "q", start: 4, end: 6, changed: 1},
		{path: "a", name: "r", start: 15, end: 30, changed: 1},
	}
	out := coalesce(units, lines, c)
	if len(out) != 2 || out[0].start != 1 || out[0].end != 6 || out[0].changed != 2 || out[0].name != "p" {
		t.Errorf("unexpected merge: %+v %+v", out[0], out[len(out)-1])
	}
}

func TestPackUnitsRespectsSize(t *testing.T) {
	units := []*unit{{text: strings.Repeat("a", 40)}, {text: strings.Repeat("b", 40)}, {text: strings.Repeat("c", 150)}, {text: "d"}}
	calls := PackUnits(units, 100)
	if len(calls) != 3 || len(calls[0]) != 2 || len(calls[1]) != 1 || len(calls[2]) != 1 {
		t.Errorf("unexpected packing: %d calls", len(calls))
	}
}

func TestAddCommentsDropsNearDuplicates(t *testing.T) {
	have := []Comment{{Path: "a", Line: 10}}
	out, added := AddComments(have, []Comment{{Path: "a", Line: 11}, {Path: "a", Line: 20}, {Path: "b", Line: 10}})
	if added != 2 || len(out) != 3 {
		t.Errorf("added %d, len %d", added, len(out))
	}
}

func TestRiskCountsLinesThatCanGoWrong(t *testing.T) {
	plain := "func a() int {\n\treturn 1\n}\n"
	risky := "func b() error {\n\tfor {\n\t\tif err := conn.Read(buf); err != nil {\n\t\t\tbreak\n\t\t}\n\t}\n\treturn nil\n}\n"
	if risk(plain) != 0 || risk(risky) < 3 {
		t.Errorf("risk plain=%d risky=%d", risk(plain), risk(risky))
	}
}

func TestUnitsStopAtRustTestModule(t *testing.T) {
	src := "#[cfg(test)]\nmod other_tests;\n\nfn live(a: i32) -> i32 {\n    a + 1\n}\n\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn t() {\n        assert_eq!(super::live(1), 2);\n    }\n}\n"
	repo := outlineRepo{treeRepo{"src/lib.rs": src}}
	var lines []diff.Line
	for i := 1; i <= 14; i++ {
		lines = append(lines, diff.Line{Kind: '+', Text: "x", NewNo: i})
	}
	files := []*diff.File{{Path: "src/lib.rs", Hunks: []diff.Hunk{{Lines: lines}}}}
	units := Units(context.Background(), repo, files, nil, nil)
	if len(units) != 1 || units[0].start != 1 || units[0].end != 7 {
		t.Fatalf("expected one unit before the test module, got %+v", units)
	}
}
