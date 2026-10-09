package review

import "testing"

func TestApplyBasis(t *testing.T) {
	cases := []struct {
		in           Verdict
		verdict, sev string
	}{
		{Verdict{Verdict: "confirmed", Severity: "major", Basis: "repository"}, "confirmed", "major"},
		{Verdict{Verdict: "rejected", Severity: "major", Basis: "platform"}, "rejected", "major"},
		{Verdict{Verdict: "confirmed", Severity: "major", Basis: "platform", Rule: ""}, "rejected", "major"},
		{Verdict{Verdict: "confirmed", Severity: "major", Basis: "platform", Rule: "yes"}, "rejected", "major"},
		{Verdict{Verdict: "confirmed", Severity: "major", Basis: "platform", Rule: "a number input shows an empty field for NaN"}, "downgraded", "minor"},
		{Verdict{Verdict: "confirmed", Severity: "nit", Basis: "platform", Rule: "a number input shows an empty field for NaN"}, "confirmed", "nit"},
		{Verdict{Verdict: "confirmed", Severity: "critical", Basis: "external", Rule: ""}, "downgraded", "minor"},
		{Verdict{Verdict: "downgraded", Severity: "minor", Basis: "external"}, "downgraded", "minor"},
	}
	for _, c := range cases {
		got := applyBasis(c.in)
		if got.Verdict != c.verdict || got.Severity != c.sev {
			t.Errorf("%+v: got %s/%s want %s/%s", c.in, got.Verdict, got.Severity, c.verdict, c.sev)
		}
	}
}
