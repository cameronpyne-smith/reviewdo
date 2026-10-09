package gitrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	cmdTimeout   = 2 * time.Minute
	cloneTimeout = 15 * time.Minute
	maxFileBytes = 64 << 10
	maxListLines = 500
	maxGrepLines = 200
	maxGrepBytes = 24 << 10
)

type TokenFunc func(ctx context.Context) (string, error)

type Store struct {
	Dir   string
	Token TokenFunc
}

type Repo struct {
	dir  string
	head string
	base string
}

func (s *Store) Ensure(ctx context.Context, fullName string, number int, head, base string) (*Repo, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil, fmt.Errorf("bad repo name %q", fullName)
	}
	root, err := filepath.Abs(s.Dir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, err
		}
		url := fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
		if _, err := s.git(ctx, root, cloneTimeout, true, "clone", "--quiet", "--no-recurse-submodules", url, dir); err != nil {
			return nil, fmt.Errorf("clone %s: %w", fullName, err)
		}
	} else if err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("refs/pull/%d/head", number)
	baseRef := "origin/" + base
	if isSHA(base) {
		baseRef = base
	}
	if _, err := s.git(ctx, dir, cloneTimeout, true, "fetch", "--quiet", "--no-recurse-submodules", "origin", base, ref); err != nil {
		return nil, fmt.Errorf("fetch %s and %s: %w", base, ref, err)
	}
	if _, err := s.git(ctx, dir, cmdTimeout, false, "cat-file", "-e", head+"^{commit}"); err != nil {
		if _, err := s.git(ctx, dir, cloneTimeout, true, "fetch", "--quiet", "--no-recurse-submodules", "origin", head); err != nil {
			return nil, fmt.Errorf("commit %s not available: %w", head, err)
		}
	}
	return &Repo{dir: dir, head: head, base: baseRef}, nil
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Store) git(ctx context.Context, dir string, timeout time.Duration, auth bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	if auth {
		tok, err := s.Token(ctx)
		if err != nil {
			return nil, err
		}
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
		env = append(env,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
			"GIT_CONFIG_KEY_1=credential.helper",
			"GIT_CONFIG_VALUE_1=",
		)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), errors.New(msg)
	}
	return stdout.Bytes(), nil
}

func (r *Repo) Head() string { return r.head }

func (r *Repo) Base() string { return r.base }

func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	s := &Store{}
	return s.git(ctx, r.dir, cmdTimeout, false, args...)
}

func cleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	c := path.Clean(p)
	if strings.HasPrefix(c, "../") || c == ".." {
		return "", errors.New("path escapes the repository")
	}
	return c, nil
}

func (r *Repo) ReadFile(ctx context.Context, p string) (string, error) {
	return r.ReadFileAt(ctx, r.head, p)
}

func (r *Repo) ReadFileAt(ctx context.Context, ref, p string) (string, error) {
	cp, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	if cp == "" {
		return "", errors.New("path is required")
	}
	typ, err := r.run(ctx, "cat-file", "-t", ref+":"+cp)
	if err != nil {
		return "", fmt.Errorf("%s: not found at this commit", cp)
	}
	if strings.TrimSpace(string(typ)) == "tree" {
		return "", fmt.Errorf("%s is a directory, use list_dir", cp)
	}
	out, err := r.run(ctx, "show", ref+":"+cp)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(out[:min(len(out), 8000)], 0) >= 0 {
		return "", fmt.Errorf("%s is a binary file", cp)
	}
	if len(out) > maxFileBytes {
		return string(out[:maxFileBytes]) + fmt.Sprintf("\n\n[truncated: %d of %d bytes shown]\n", maxFileBytes, len(out)), nil
	}
	return string(out), nil
}

func (r *Repo) ListDir(ctx context.Context, p string) (string, error) {
	return r.ListDirAt(ctx, r.head, p)
}

func (r *Repo) ListDirAt(ctx context.Context, ref, p string) (string, error) {
	cp, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	out, err := r.run(ctx, "ls-tree", ref+":"+cp)
	if err != nil {
		return "", fmt.Errorf("%s: not found at this commit", cp)
	}
	var b strings.Builder
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, l := range lines {
		if i >= maxListLines {
			fmt.Fprintf(&b, "[%d more entries]\n", len(lines)-i)
			break
		}
		meta, name, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		if strings.Contains(meta, " tree ") {
			name += "/"
		}
		b.WriteString(name)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "(empty)\n", nil
	}
	return b.String(), nil
}

func (r *Repo) Search(ctx context.Context, pattern, p string) (string, error) {
	return r.SearchAt(ctx, r.head, pattern, p)
}

func (r *Repo) SearchBase(ctx context.Context, pattern, p string) (string, error) {
	return r.SearchAt(ctx, r.base, pattern, p)
}

func (r *Repo) SearchAt(ctx context.Context, ref, pattern, p string) (string, error) {
	cp, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(pattern) == "" {
		return "", errors.New("pattern is required")
	}
	args := []string{"grep", "-n", "-I", "-E", "--max-count=50", "-e", pattern, ref}
	if cp != "" {
		args = append(args, "--", cp)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		if len(out) == 0 {
			return "(no matches)\n", nil
		}
		return "", err
	}
	prefix := ref + ":"
	var b strings.Builder
	n := 0
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l == "" {
			continue
		}
		if n >= maxGrepLines || b.Len() > maxGrepBytes {
			b.WriteString("[more matches omitted, narrow the pattern or path]\n")
			break
		}
		b.WriteString(strings.TrimPrefix(l, prefix))
		b.WriteString("\n")
		n++
	}
	if b.Len() == 0 {
		return "(no matches)\n", nil
	}
	return b.String(), nil
}
