package review

import "testing"

func TestVerdictFollowsFindings(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want string
	}{
		{"caution without findings is ready", Result{Verdict: "caution"}, "ready"},
		{"blocked without findings is ready", Result{Verdict: "blocked"}, "ready"},
		{"empty bodies do not count", Result{Verdict: "caution", Comments: []Comment{{Severity: "major", Body: " "}}}, "ready"},
		{"minor only is ready", Result{Verdict: "caution", Comments: []Comment{{Severity: "minor", Body: "x"}, {Severity: "nit", Body: "y"}}}, "ready"},
		{"major forces caution", Result{Verdict: "ready", Comments: []Comment{{Severity: "major", Body: "x"}}}, "caution"},
		{"critical forces blocked", Result{Verdict: "ready", Comments: []Comment{{Severity: "critical", Body: "x"}}}, "blocked"},
		{"blocked without critical is caution", Result{Verdict: "blocked", Comments: []Comment{{Severity: "major", Body: "x"}}}, "caution"},
		{"skipped parts force caution", Result{Verdict: "ready", SkippedParts: 2}, "caution"},
		{"unverified forces caution", Result{Verdict: "ready", Unverified: 1}, "caution"},
	}
	for _, c := range cases {
		if got := verdict(&c.res); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
