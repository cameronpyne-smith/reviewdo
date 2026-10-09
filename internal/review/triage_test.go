package review

import "testing"

func TestHollow(t *testing.T) {
	yes := []string{
		"The retry logic looks correct as written.",
		"No action needed here, the default covers it.",
		"This works as intended since the guard runs first.",
	}
	no := []string{
		"The link `docs/x.md` does not exist at this commit.",
		"`always` is missing so the header is dropped on 4xx responses.",
	}
	for _, s := range yes {
		if !Hollow(s) {
			t.Errorf("expected hollow: %q", s)
		}
	}
	for _, s := range no {
		if Hollow(s) {
			t.Errorf("unexpected hollow: %q", s)
		}
	}
}
