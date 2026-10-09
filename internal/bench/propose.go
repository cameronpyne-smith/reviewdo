package bench

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

type ProposeOptions struct {
	Since   time.Time
	Count   int
	Include []SetEntry
}

type candidate struct {
	g       *Golden
	valid   int
	invalid int
	ext     string
	large   bool
}

func Propose(goldens []*Golden, opt ProposeOptions, w io.Writer) []SetEntry {
	var cands []candidate
	byRef := map[string]candidate{}
	for _, g := range goldens {
		if g.ReviewedAt.Before(opt.Since) || g.Commit == "" {
			continue
		}
		c := candidate{g: g, valid: g.Count("valid"), invalid: g.Count("invalid"), ext: dominantExt(g), large: g.Files >= 15 || g.Size >= 800}
		cands = append(cands, c)
		byRef[g.Ref()] = c
	}
	var chosen []candidate
	taken := map[string]bool{}
	repos := map[string]int{}
	exts := map[string]int{}
	pick := func(c candidate, why string) {
		if taken[c.g.Ref()] {
			return
		}
		taken[c.g.Ref()] = true
		repos[c.g.Repo]++
		exts[c.ext]++
		chosen = append(chosen, c)
		fmt.Fprintf(w, "%-45s %-6s valid=%d invalid=%d files=%d size=%d %s  [%s]\n", c.g.Ref(), c.ext, c.valid, c.invalid, c.g.Files, c.g.Size, c.g.ReviewedAt.Format("2006-01-02"), why)
	}
	for _, inc := range opt.Include {
		if c, ok := byRef[fmt.Sprintf("%s#%d", inc.Repo, inc.Number)]; ok {
			pick(c, "included")
		}
	}
	best := func(filter func(candidate) bool, score func(candidate) float64) (candidate, bool) {
		var out candidate
		found := false
		bestScore := -1e9
		for _, c := range cands {
			if taken[c.g.Ref()] || !filter(c) {
				continue
			}
			s := score(c) - 2*float64(repos[c.g.Repo]) - float64(exts[c.ext])
			if s > bestScore {
				bestScore, out, found = s, c, true
			}
		}
		return out, found
	}
	recency := func(c candidate) float64 { return float64(c.g.ReviewedAt.Unix()) / 1e9 }
	if c, ok := best(func(c candidate) bool { return c.valid+c.invalid == 0 && len(c.g.Scored()) == 0 }, recency); ok {
		pick(c, "copilot posted nothing")
	}
	for i := 0; i < 2; i++ {
		if c, ok := best(func(c candidate) bool { return c.invalid >= 1 }, func(c candidate) float64 { return float64(c.invalid) + 0.5*float64(c.valid) + recency(c) }); ok {
			pick(c, "rejected findings")
		}
	}
	largeNeeded := 2
	for len(chosen) < opt.Count {
		needLarge := largeNeeded > 0
		c, ok := best(func(c candidate) bool { return c.valid >= 2 && (!needLarge || c.large) }, func(c candidate) float64 { return float64(c.valid) + recency(c) })
		if !ok {
			c, ok = best(func(c candidate) bool { return c.valid >= 1 }, func(c candidate) float64 { return float64(c.valid) + recency(c) })
		}
		if !ok {
			break
		}
		if c.large {
			largeNeeded--
		}
		pick(c, "valid findings")
	}
	var set []SetEntry
	for _, c := range chosen {
		set = append(set, SetEntry{Repo: c.g.Repo, Number: c.g.Number})
	}
	var rs []string
	for r, n := range repos {
		rs = append(rs, fmt.Sprintf("%s=%d", r, n))
	}
	sort.Strings(rs)
	fmt.Fprintf(w, "\n%d PRs from %d repos: %s\n", len(set), len(repos), strings.Join(rs, ", "))
	return set
}

func dominantExt(g *Golden) string {
	counts := map[string]int{}
	for _, f := range g.Findings {
		e := strings.TrimPrefix(path.Ext(f.Path), ".")
		if e == "" {
			e = path.Base(f.Path)
		}
		counts[e]++
	}
	best, n := "", 0
	for e, c := range counts {
		if c > n || (c == n && e < best) {
			best, n = e, c
		}
	}
	if best == "" {
		return "?"
	}
	return best
}
