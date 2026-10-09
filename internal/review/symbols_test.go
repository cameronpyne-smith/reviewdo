package review

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

func TestDeclaredName(t *testing.T) {
	cases := map[string]string{
		"func (r *Repo) Definition(ctx context.Context) {":                          "Definition",
		"func Symbols(ctx context.Context) string {":                                "Symbols",
		"    public void SendToGroup(string hub, string group)":                     "SendToGroup",
		"    public static string Body(string hub) =>":                              "Body",
		"    public IReadOnlyList<Channel> ResubmissionChannels { get; init; }":     "ResubmissionChannels",
		"public sealed class RealtimePublishException(string message)":              "RealtimePublishException",
		"    private readonly Regex NonAlphanumeric = new(\"x\");":                  "NonAlphanumeric",
		"export const isCandidateView = writable(false)":                            "isCandidateView",
		"export const currentPage = (): ConsolePage => {":                           "currentPage",
		"const text = (value: FactValue): string | null => {":                       "text",
		"  subscribe(params: Params): void {":                                       "subscribe",
		"pub fn aggregate_id(&self) -> Option<String> {":                            "aggregate_id",
		"def validate_window(self, window):":                                        "validate_window",
		"resource \"kubernetes_secret\" \"rabbitmq\" {":                             "rabbitmq",
		"export interface ConsolePage {":                                            "ConsolePage",
		"    return Foo(bar)":                                                       "",
		"    if (x) {":                                                              "",
		"  foo(bar)":                                                                "",
		"// func Ignored() {":                                                       "",
		"    const n = 3":                                                           "",
		"    public override string ToString() =>":                                  "",
		"/** Registers a page; the returned function takes it back. */":             "",
		"namespace Remundo.Api.Realtime;":                                           "",
		"    var page = new AssistantPage()":                                        "",
		" * the ids the console's tools take. A page never fetches":                 "",
		"    public void Given_the_publisher_throws_When_published_Then_it_fails()": "",
	}
	for line, want := range cases {
		if got := declaredName(line); got != want {
			t.Errorf("%q: got %q want %q", line, got, want)
		}
	}
}

type refRepo struct {
	fakeRepo
	refs map[string]string
}

func (r refRepo) References(_ context.Context, name, _ string) (string, error) {
	if s, ok := r.refs[name]; ok {
		return s, nil
	}
	return "(no references)\n", nil
}

func TestSymbols(t *testing.T) {
	files := []*diff.File{{Path: "a.ts", Hunks: []diff.Hunk{{Lines: []diff.Line{
		{Kind: '-', Text: "export const oldName = (): void => {"},
		{Kind: '+', Text: "export const newName = (): void => {"},
		{Kind: '-', Text: "export const changed = (a: number): void => {"},
		{Kind: '+', Text: "export const changed = (a: number, b: number): void => {"},
		{Kind: ' ', Text: "export const untouched = (): void => {"},
	}}}}}
	repo := refRepo{refs: map[string]string{
		"oldName": "3 references in 2 files\nb.ts (2)\n  10: oldName()\n  12: oldName()\nc.ts (1)\n  4: oldName()\n",
		"changed": "1 references in 1 files\na.ts (1)\n  1: changed\n",
	}}
	out := Symbols(context.Background(), repo, files)
	want := []string{
		"- oldName (a.ts, removed): still used, 3 uses in 2 files: b.ts (2), c.ts (1)",
		"- changed (a.ts, modified): used only in its own file (1)",
		"- newName (a.ts, new): no uses found",
	}
	at := -1
	for _, w := range want {
		i := strings.Index(out, w)
		if i < 0 {
			t.Errorf("missing %q in:\n%s", w, out)
		}
		if i < at {
			t.Errorf("%q out of order in:\n%s", w, out)
		}
		at = i
	}
	if strings.Contains(out, "untouched") {
		t.Errorf("context line listed as a declaration:\n%s", out)
	}
	if Symbols(context.Background(), repo, nil) != "" {
		t.Error("expected empty output for no files")
	}
}
