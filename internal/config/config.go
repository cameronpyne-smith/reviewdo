package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type Config struct {
	AppID            int64    `json:"app_id"`
	InstallationID   int64    `json:"installation_id"`
	PrivateKeyPath   string   `json:"private_key_path"`
	StatePath        string   `json:"state_path"`
	CloneDir         string   `json:"clone_dir"`
	PollInterval     Duration `json:"poll_interval"`
	Label            string   `json:"label"`
	Instructions     string   `json:"instructions"`
	InstructionsFile string   `json:"instructions_file"`
	Repos            []Repo   `json:"repos"`
	ExcludeRepos     []string `json:"exclude_repos"`
	Ollama           Ollama   `json:"ollama"`
	Review           Review   `json:"review"`
}

type Repo struct {
	Name             string `json:"name"`
	Instructions     string `json:"instructions"`
	InstructionsFile string `json:"instructions_file"`
}

func (r Repo) Wildcard() bool {
	return strings.HasSuffix(r.Name, "/*")
}

func (r Repo) owner() string {
	o, _, _ := strings.Cut(r.Name, "/")
	return o
}

func (c *Config) HasWildcard() bool {
	for _, r := range c.Repos {
		if r.Wildcard() {
			return true
		}
	}
	return false
}

func (c *Config) Excluded(name string) bool {
	for _, x := range c.ExcludeRepos {
		if strings.EqualFold(x, name) {
			return true
		}
	}
	return false
}

func (c *Config) Resolve(installed []string) []Repo {
	seen := map[string]bool{}
	for _, x := range c.ExcludeRepos {
		seen[strings.ToLower(x)] = true
	}
	var out []Repo
	for _, r := range c.Repos {
		if !r.Wildcard() && !seen[strings.ToLower(r.Name)] {
			seen[strings.ToLower(r.Name)] = true
			out = append(out, r)
		}
	}
	names := append([]string{}, installed...)
	sort.Strings(names)
	for _, r := range c.Repos {
		if !r.Wildcard() {
			continue
		}
		for _, n := range names {
			owner, _, _ := strings.Cut(n, "/")
			if strings.EqualFold(owner, r.owner()) && !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				out = append(out, Repo{Name: n, Instructions: r.Instructions})
			}
		}
	}
	return out
}

func (c *Config) RepoFor(name string) Repo {
	for _, r := range c.Repos {
		if strings.EqualFold(r.Name, name) {
			return r
		}
	}
	owner, _, _ := strings.Cut(name, "/")
	for _, r := range c.Repos {
		if r.Wildcard() && strings.EqualFold(r.owner(), owner) {
			return Repo{Name: name, Instructions: r.Instructions}
		}
	}
	return Repo{Name: name}
}

type Ollama struct {
	URL         string   `json:"url"`
	Model       string   `json:"model"`
	NumCtx      int      `json:"num_ctx"`
	NumPredict  int      `json:"num_predict"`
	Temperature float64  `json:"temperature"`
	Think       Think    `json:"think"`
	Timeout     Duration `json:"timeout"`
}

type Review struct {
	MaxDiffBytes int      `json:"max_diff_bytes"`
	MaxComments  int      `json:"max_comments"`
	MaxToolCalls int      `json:"max_tool_calls"`
	MaxOutput    int      `json:"max_output_tokens"`
	PartBytes    int      `json:"part_bytes"`
	PartTime     Duration `json:"part_time"`
	Triage       *bool    `json:"triage"`
	Verify       *bool    `json:"verify"`
	TimeBudget   Duration `json:"time_budget"`
	Timeout      Duration `json:"timeout"`
	Ignore       []string `json:"ignore"`
}

var DefaultIgnore = []string{
	"*.lock", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "Cargo.lock",
	"*.min.js", "*.min.css", "*.map", "*.svg", "*.pb.go", "*.generated.*", "*.snap",
	"vendor/", "node_modules/", "dist/", "build/",
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	return &c, c.validate()
}

func (c *Config) applyDefaults() error {
	if c.PrivateKeyPath == "" {
		if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
			c.PrivateKeyPath = filepath.Join(dir, "private-key")
		}
	}
	if c.StatePath == "" {
		if dir := os.Getenv("STATE_DIRECTORY"); dir != "" {
			c.StatePath = filepath.Join(dir, "state.json")
		}
	}
	if c.PollInterval.Duration == 0 {
		c.PollInterval.Duration = time.Minute
	}
	if c.Label == "" {
		c.Label = "reviewdo"
	}
	if c.Ollama.URL == "" {
		c.Ollama.URL = "http://127.0.0.1:11434"
	}
	if c.Ollama.NumCtx == 0 {
		c.Ollama.NumCtx = 32768
	}
	if c.Ollama.NumPredict == 0 {
		c.Ollama.NumPredict = 12288
	}
	if c.Ollama.Timeout.Duration == 0 {
		c.Ollama.Timeout.Duration = 20 * time.Minute
	}
	if c.Review.MaxDiffBytes == 0 {
		c.Review.MaxDiffBytes = 3 * c.Ollama.NumCtx
	}
	if c.Review.MaxComments == 0 {
		c.Review.MaxComments = 15
	}
	if c.Review.MaxToolCalls == 0 {
		c.Review.MaxToolCalls = 200
	}
	if c.Review.MaxOutput == 0 {
		c.Review.MaxOutput = 48000
	}
	if c.Review.PartBytes == 0 {
		c.Review.PartBytes = 40000
	}
	if c.Review.PartTime.Duration == 0 {
		c.Review.PartTime.Duration = 2 * time.Minute
	}
	if c.Review.Triage == nil {
		v := true
		c.Review.Triage = &v
	}
	if c.Review.Verify == nil {
		v := true
		c.Review.Verify = &v
	}
	if c.Review.TimeBudget.Duration == 0 {
		c.Review.TimeBudget.Duration = 5 * time.Minute
	}
	if c.Review.Timeout.Duration == 0 {
		c.Review.Timeout.Duration = 3 * c.Review.TimeBudget.Duration
	}
	if strings.HasPrefix(c.CloneDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		c.CloneDir = filepath.Join(home, c.CloneDir[2:])
	}
	var err error
	if c.Instructions, err = withFile(c.Instructions, c.InstructionsFile); err != nil {
		return err
	}
	for i := range c.Repos {
		if c.Repos[i].Instructions, err = withFile(c.Repos[i].Instructions, c.Repos[i].InstructionsFile); err != nil {
			return err
		}
	}
	if c.Review.Ignore == nil {
		c.Review.Ignore = DefaultIgnore
	}
	return nil
}

func withFile(inline, file string) (string, error) {
	if file == "" {
		return inline, nil
	}
	if strings.HasPrefix(file, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		file = filepath.Join(home, file[2:])
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(inline + "\n\n" + string(b)), nil
}

func (c *Config) validate() error {
	var errs []error
	if c.AppID == 0 {
		errs = append(errs, errors.New("app_id is required"))
	}
	if c.InstallationID == 0 {
		errs = append(errs, errors.New("installation_id is required"))
	}
	if c.PrivateKeyPath == "" {
		errs = append(errs, errors.New("private_key_path is required (or run under systemd with LoadCredential=private-key:...)"))
	}
	if c.StatePath == "" {
		errs = append(errs, errors.New("state_path is required (or run under systemd with StateDirectory=)"))
	}
	if len(c.Repos) == 0 {
		errs = append(errs, errors.New("at least one repo is required"))
	}
	for _, r := range c.Repos {
		owner, name, ok := strings.Cut(r.Name, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			errs = append(errs, fmt.Errorf("repo %q must be owner/name or owner/*", r.Name))
		}
	}
	for _, x := range c.ExcludeRepos {
		if strings.Count(x, "/") != 1 || strings.HasSuffix(x, "/*") {
			errs = append(errs, fmt.Errorf("exclude_repos entry %q must be owner/name", x))
		}
	}
	if c.Ollama.Model == "" {
		errs = append(errs, errors.New("ollama.model is required"))
	}
	if c.PollInterval.Duration < 10*time.Second {
		errs = append(errs, errors.New("poll_interval must be at least 10s"))
	}
	return errors.Join(errs...)
}

type Think struct {
	v any
}

func (t *Think) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch v.(type) {
	case nil, bool, string:
		t.v = v
		return nil
	}
	return fmt.Errorf("think must be true, false or a level name such as \"low\"")
}

func (t Think) MarshalJSON() ([]byte, error) { return json.Marshal(t.v) }

func (t Think) Value() any { return t.v }

func ParseThink(s string) Think {
	switch s {
	case "":
		return Think{}
	case "true":
		return Think{v: true}
	case "false":
		return Think{v: false}
	}
	return Think{v: s}
}
