package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type State struct {
	Repos map[string]*Repo `json:"repos"`
}

type Repo struct {
	CommentsSince time.Time        `json:"comments_since"`
	LastCommentID int64            `json:"last_comment_id"`
	Pulls         map[string]*Pull `json:"pulls"`
}

type Pull struct {
	HeadSHA        string    `json:"head_sha"`
	ReviewedSHA    string    `json:"reviewed_sha,omitempty"`
	ReviewedAt     time.Time `json:"reviewed_at,omitempty"`
	LabelSHA       string    `json:"label_sha,omitempty"`
	FailedSHA      string    `json:"failed_sha,omitempty"`
	PendingComment int64     `json:"pending_comment,omitempty"`
	Baselined      bool      `json:"baselined,omitempty"`
}

func Load(path string) (*State, bool, error) {
	s := &State{Repos: map[string]*Repo{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, false, err
	}
	if s.Repos == nil {
		s.Repos = map[string]*Repo{}
	}
	return s, true, nil
}

func (s *State) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) Repo(name string) (*Repo, bool) {
	r, ok := s.Repos[name]
	if !ok {
		r = &Repo{Pulls: map[string]*Pull{}}
		s.Repos[name] = r
	}
	if r.Pulls == nil {
		r.Pulls = map[string]*Pull{}
	}
	return r, ok
}
