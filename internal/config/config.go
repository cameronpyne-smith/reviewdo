package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	AppID          int64    `json:"app_id"`
	InstallationID int64    `json:"installation_id"`
	PrivateKeyPath string   `json:"private_key_path"`
	StatePath      string   `json:"state_path"`
	CloneDir       string   `json:"clone_dir"`
	PollInterval   Duration `json:"poll_interval"`
	Label          string   `json:"label"`
	Repos          []Repo   `json:"repos"`
	Ollama         Ollama   `json:"ollama"`
	Review         Review   `json:"review"`
}

type Repo struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}

type Ollama struct {
	URL         string   `json:"url"`
	Model       string   `json:"model"`
	NumCtx      int      `json:"num_ctx"`
	Temperature float64  `json:"temperature"`
	Think       *bool    `json:"think"`
	Timeout     Duration `json:"timeout"`
}

type Review struct {
	MaxDiffBytes int      `json:"max_diff_bytes"`
	MaxComments  int      `json:"max_comments"`
	MaxToolCalls int      `json:"max_tool_calls"`
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
		c.Review.MaxToolCalls = 60
	}
	if strings.HasPrefix(c.CloneDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		c.CloneDir = filepath.Join(home, c.CloneDir[2:])
	}
	if c.Review.Ignore == nil {
		c.Review.Ignore = DefaultIgnore
	}
	return nil
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
		if strings.Count(r.Name, "/") != 1 {
			errs = append(errs, fmt.Errorf("repo %q must be owner/name", r.Name))
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
