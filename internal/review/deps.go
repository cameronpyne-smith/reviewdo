package review

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/diff"
)

const (
	maxManifests     = 6
	maxManifestDeps  = 40
	maxManifestBytes = 64 << 10
)

var (
	packageRef = regexp.MustCompile(`<PackageReference\s+[^>]*Include="([^"]+)"(?:[^>]*Version="([^"]+)")?`)
	packageVer = regexp.MustCompile(`<PackageVersion\s+[^>]*Include="([^"]+)"[^>]*Version="([^"]+)"`)
	goRequire  = regexp.MustCompile(`(?m)^\s*([\w./-]+)\s+(v[\w.+-]+)`)
	cargoDep   = regexp.MustCompile(`(?m)^([\w-]+)\s*=\s*(?:"([^"]+)"|\{[^}]*version\s*=\s*"([^"]+)")`)
	pipDep     = regexp.MustCompile(`(?m)^([A-Za-z][\w.-]*)\s*([=<>~!]=?\s*[\w.*]+)?`)
)

var manifestNames = map[string]bool{"package.json": true, "go.mod": true, "Cargo.toml": true, "requirements.txt": true, "pyproject.toml": true, "Directory.Packages.props": true}

func isManifest(name string) bool {
	return manifestNames[name] || strings.HasSuffix(name, ".csproj") || strings.HasSuffix(name, ".fsproj")
}

func parseManifest(name, text string) []string {
	var deps []string
	add := func(n, v string) {
		if n == "" {
			return
		}
		if v == "" {
			deps = append(deps, n)
		} else {
			deps = append(deps, n+" "+v)
		}
	}
	switch {
	case name == "package.json":
		var m struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal([]byte(text), &m) != nil {
			return nil
		}
		for _, set := range []map[string]string{m.Dependencies, m.DevDependencies} {
			var names []string
			for n := range set {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				add(n, set[n])
			}
		}
	case name == "go.mod":
		for _, m := range goRequire.FindAllStringSubmatch(text, -1) {
			if strings.Contains(m[1], ".") {
				add(m[1], m[2])
			}
		}
	case name == "Cargo.toml":
		in := false
		for _, l := range strings.Split(text, "\n") {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "[") {
				in = strings.Contains(t, "dependencies]")
				continue
			}
			if !in {
				continue
			}
			if m := cargoDep.FindStringSubmatch(t); m != nil {
				add(m[1], m[2]+m[3])
			}
		}
	case name == "requirements.txt":
		for _, l := range strings.Split(text, "\n") {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "-") {
				continue
			}
			if m := pipDep.FindStringSubmatch(t); m != nil {
				add(m[1], strings.ReplaceAll(m[2], " ", ""))
			}
		}
	case name == "pyproject.toml":
		in := false
		for _, l := range strings.Split(text, "\n") {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "[") {
				in = strings.Contains(t, "dependencies")
				continue
			}
			if !in {
				continue
			}
			t = strings.Trim(strings.TrimSuffix(t, ","), `"' `)
			if m := pipDep.FindStringSubmatch(t); m != nil && m[1] != "" {
				add(m[1], strings.ReplaceAll(m[2], " ", ""))
			}
		}
	default:
		for _, m := range packageRef.FindAllStringSubmatch(text, -1) {
			add(m[1], m[2])
		}
		for _, m := range packageVer.FindAllStringSubmatch(text, -1) {
			add(m[1], m[2])
		}
	}
	return deps
}

func Dependencies(ctx context.Context, repo Repo, files []*diff.File) string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		d := path.Dir(f.Path)
		for {
			if d == "." {
				d = ""
			}
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
			if d == "" {
				break
			}
			d = path.Dir(d)
		}
	}
	var b strings.Builder
	found := 0
	for _, d := range dirs {
		if found >= maxManifests || ctx.Err() != nil {
			break
		}
		out, err := repo.ListDir(ctx, d)
		if err != nil {
			continue
		}
		for _, e := range strings.Split(strings.TrimSpace(out), "\n") {
			if !isManifest(e) {
				continue
			}
			p := e
			if d != "" {
				p = d + "/" + e
			}
			text, err := repo.ReadFile(ctx, p)
			if err != nil || len(text) > maxManifestBytes {
				continue
			}
			deps := parseManifest(e, text)
			if len(deps) == 0 {
				continue
			}
			found++
			if len(deps) > maxManifestDeps {
				deps = append(deps[:maxManifestDeps], fmt.Sprintf("%d more", len(deps)-maxManifestDeps))
			}
			fmt.Fprintf(&b, "- %s: %s\n", p, strings.Join(deps, ", "))
			if found >= maxManifests {
				break
			}
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "Dependencies declared by the manifests nearest the changed files (name and version at the head commit):\n" + b.String() +
		"A claim about how a library behaves must match the version listed here, and a suggested fix must only use APIs that version offers and the repository already uses.\n"
}
