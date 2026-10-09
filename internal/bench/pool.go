package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cameronpyne-smith/reviewdo/internal/github"
)

const copilotLogin = "copilot-pull-request-reviewer"

type PR struct {
	Repo         string                     `json:"repo"`
	Number       int                        `json:"number"`
	Title        string                     `json:"title"`
	Author       string                     `json:"author"`
	State        string                     `json:"state"`
	CreatedAt    time.Time                  `json:"created_at"`
	MergedAt     time.Time                  `json:"merged_at,omitempty"`
	BaseRef      string                     `json:"base_ref"`
	BaseSHA      string                     `json:"base_sha"`
	HeadSHA      string                     `json:"head_sha"`
	Additions    int                        `json:"additions"`
	Deletions    int                        `json:"deletions"`
	ChangedFiles int                        `json:"changed_files"`
	Body         string                     `json:"body"`
	Reviews      []github.PullReviewSummary `json:"reviews"`
	Threads      []github.ReviewThread      `json:"threads"`
	MinedAt      time.Time                  `json:"mined_at"`
}

func (p *PR) Ref() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

type Finding struct {
	ID       string    `json:"id"`
	Source   string    `json:"source"`
	Commit   string    `json:"commit"`
	Path     string    `json:"path"`
	Line     int       `json:"line"`
	Body     string    `json:"body"`
	Reply    string    `json:"reply,omitempty"`
	Resolved bool      `json:"resolved"`
	Verdict  string    `json:"verdict"`
	Reason   string    `json:"reason,omitempty"`
	Labelled string    `json:"labelled,omitempty"`
	Locked   bool      `json:"locked,omitempty"`
	At       time.Time `json:"at,omitempty"`
}

type Golden struct {
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Author     string    `json:"author"`
	Commit     string    `json:"commit"`
	BaseRef    string    `json:"base_ref"`
	BaseSHA    string    `json:"base_sha"`
	ReviewedAt time.Time `json:"reviewed_at"`
	Size       int       `json:"size"`
	Files      int       `json:"files"`
	Findings   []Finding `json:"findings"`
}

func (g *Golden) Ref() string { return fmt.Sprintf("%s#%d", g.Repo, g.Number) }

func (g *Golden) Scored() []Finding {
	var out []Finding
	for _, f := range g.Findings {
		if f.Commit == g.Commit || f.Source != "copilot" {
			out = append(out, f)
		}
	}
	return out
}

func (g *Golden) Count(verdict string) int {
	n := 0
	for _, f := range g.Scored() {
		if f.Source == "copilot" && f.Verdict == verdict {
			n++
		}
	}
	return n
}

type Dir struct{ Root string }

func (d Dir) path(kind, repo string, number int) string {
	return filepath.Join(d.Root, kind, filepath.FromSlash(repo), fmt.Sprintf("%d.json", number))
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (d Dir) SavePR(p *PR) error { return writeJSON(d.path("pool", p.Repo, p.Number), p) }

func (d Dir) HasPR(repo string, number int) bool {
	_, err := os.Stat(d.path("pool", repo, number))
	return err == nil
}

func (d Dir) SaveGolden(g *Golden) error { return writeJSON(d.path("golden", g.Repo, g.Number), g) }

func (d Dir) LoadGolden(repo string, number int) (*Golden, error) {
	var g Golden
	if err := readJSON(d.path("golden", repo, number), &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func (d Dir) walk(kind string, visit func(path string) error) error {
	root := filepath.Join(d.Root, kind)
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(path, ".json") || e.Name() == "seen.json" {
			return nil
		}
		return visit(path)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (d Dir) PRs() ([]*PR, error) {
	var out []*PR
	err := d.walk("pool", func(path string) error {
		var p PR
		if err := readJSON(path, &p); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, &p)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, err
}

func (d Dir) Goldens() ([]*Golden, error) {
	var out []*Golden
	err := d.walk("golden", func(path string) error {
		var g Golden
		if err := readJSON(path, &g); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, &g)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ReviewedAt.After(out[j].ReviewedAt) })
	return out, err
}

type seenSet struct {
	path string
	m    map[string]bool
}

func (d Dir) seen() (*seenSet, error) {
	s := &seenSet{path: filepath.Join(d.Root, "pool", "seen.json"), m: map[string]bool{}}
	if err := readJSON(s.path, &s.m); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func (s *seenSet) save() error { return writeJSON(s.path, s.m) }

func isCopilot(login string) bool {
	return strings.HasPrefix(strings.ToLower(login), copilotLogin)
}

type SetEntry struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (d Dir) LoadSet(name string) ([]SetEntry, error) {
	var out []SetEntry
	if err := readJSON(filepath.Join(d.Root, name), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (d Dir) SaveSet(name string, set []SetEntry) error {
	return writeJSON(filepath.Join(d.Root, name), set)
}
