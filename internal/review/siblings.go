package review

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

const (
	maxSiblingDirs  = 12
	maxSiblingFiles = 12
	maxSiblingKeys  = 8
	maxSiblingBytes = 64 << 10
)

var keyLine = regexp.MustCompile(`^\s*-?\s*"?([A-Za-z_][\w.-]*)"?\s*[:=]`)

func keysOf(text string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(text, "\n") {
		if m := keyLine.FindStringSubmatch(l); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

func siblingDirs(ctx context.Context, repo Repo, parent, self string) []string {
	out, err := repo.ListDir(ctx, parent)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(e, "/") && e != self+"/" {
			dirs = append(dirs, strings.TrimSuffix(e, "/"))
		}
	}
	prefix := func(a, b string) int {
		n := 0
		for n < len(a) && n < len(b) && a[n] == b[n] {
			n++
		}
		return n
	}
	sort.SliceStable(dirs, func(i, j int) bool { return prefix(dirs[i], self) > prefix(dirs[j], self) })
	return dirs
}

type siblingSet struct {
	parent, self, rel string
	paths             []string
}

func findSiblings(ctx context.Context, repo Repo, path string) *siblingSet {
	segs := strings.Split(path, "/")
	for i := len(segs) - 2; i >= 0; i-- {
		parent := strings.Join(segs[:i], "/")
		self := segs[i]
		rel := strings.Join(segs[i+1:], "/")
		var paths []string
		for _, d := range siblingDirs(ctx, repo, parent, self) {
			if ctx.Err() != nil || len(paths) >= maxSiblingDirs {
				break
			}
			p := rel
			if parent != "" {
				p = parent + "/" + d + "/" + rel
			} else {
				p = d + "/" + rel
			}
			if _, err := repo.Outline(ctx, p); err == nil {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			return &siblingSet{parent: parent, self: self, rel: rel, paths: paths}
		}
	}
	return nil
}

func Siblings(ctx context.Context, repo Repo, files []*diff.File) string {
	var b strings.Builder
	n := 0
	for _, f := range files {
		if f.Binary || f.Deleted || !strings.Contains(f.Path, "/") || ctx.Err() != nil {
			continue
		}
		if n >= maxSiblingFiles {
			b.WriteString("[further changed files not compared]\n")
			break
		}
		set := findSiblings(ctx, repo, f.Path)
		if set == nil {
			continue
		}
		n++
		text, err := repo.ReadFile(ctx, f.Path)
		if err != nil || len(text) > maxSiblingBytes {
			continue
		}
		mine := keysOf(text)
		count := map[string]int{}
		checked := 0
		for _, p := range set.paths {
			t, err := repo.ReadFile(ctx, p)
			if err != nil || len(t) > maxSiblingBytes {
				continue
			}
			checked++
			for k := range keysOf(t) {
				count[k]++
			}
		}
		if checked == 0 {
			continue
		}
		type miss struct {
			key string
			n   int
		}
		var missing []miss
		for k, c := range count {
			if !mine[k] && c*3 >= checked*2 && c >= 2 {
				missing = append(missing, miss{k, c})
			}
		}
		sort.Slice(missing, func(i, j int) bool {
			if missing[i].n != missing[j].n {
				return missing[i].n > missing[j].n
			}
			return missing[i].key < missing[j].key
		})
		fmt.Fprintf(&b, "- %s: %d sibling files with the same path under %s/<other>/ (", f.Path, checked, set.parent)
		shown := set.paths
		if len(shown) > 3 {
			shown = shown[:3]
		}
		var names []string
		for _, p := range shown {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(p, set.parent+"/"), "/"+set.rel))
		}
		b.WriteString(strings.Join(names, ", "))
		if len(set.paths) > 3 {
			b.WriteString(", ...")
		}
		b.WriteString("). ")
		if len(missing) == 0 {
			b.WriteString("No key that most siblings set is missing here.\n")
			continue
		}
		b.WriteString("Keys most siblings set that this file does not: ")
		for i, m := range missing {
			if i >= maxSiblingKeys {
				fmt.Fprintf(&b, ", %d more", len(missing)-i)
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s (%d of %d)", m.key, m.n, checked)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "Sibling comparison for changed files that have a counterpart under a sibling directory (keys are YAML, JSON, HCL or properties names found anywhere in the file):\n" + b.String() +
		"A key every comparable sibling sets and this file lacks is a finding unless the difference is deliberate and explained; open one sibling to see what the key does there.\n"
}
