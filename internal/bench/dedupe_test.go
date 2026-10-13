package bench

import "testing"

func TestGroupGoldenFoldsDuplicatesOntoFirst(t *testing.T) {
	golden := []Finding{
		{ID: "a", Verdict: "valid"},
		{ID: "b", Verdict: "valid", Dup: "a"},
		{ID: "c", Verdict: "invalid", Dup: "a"},
		{ID: "d", Verdict: "valid", Dup: "missing"},
		{ID: "e", Verdict: "invalid"},
	}
	groups := groupGolden(golden)
	if len(groups) != 4 {
		t.Fatalf("got %d groups, want 4", len(groups))
	}
	if len(groups[0]) != 2 || groups[0][1].ID != "b" {
		t.Errorf("group a = %v, want a with b folded in", groups[0])
	}
	for _, g := range groups[1:] {
		if len(g) != 1 {
			t.Errorf("group %s has %d members, want 1", g[0].ID, len(g))
		}
	}
}
