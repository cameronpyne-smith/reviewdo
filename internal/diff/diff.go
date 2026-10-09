package diff

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

type File struct {
	Path    string
	OldPath string
	Added   bool
	Deleted bool
	Renamed bool
	Binary  bool
	Hunks   []Hunk
}

type Hunk struct {
	Header string
	Lines  []Line
}

type Line struct {
	Kind  byte
	Text  string
	OldNo int
	NewNo int
}

func (f *File) Changes() (added, removed int) {
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case '+':
				added++
			case '-':
				removed++
			}
		}
	}
	return
}

func (f *File) RightText() map[int]string {
	m := map[int]string{}
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.NewNo > 0 {
				m[l.NewNo] = l.Text
			}
		}
	}
	return m
}

func (f *File) RightLines() map[int]bool {
	m := map[int]bool{}
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.NewNo > 0 {
				m[l.NewNo] = true
			}
		}
	}
	return m
}

func Parse(data []byte) []*File {
	var files []*File
	var cur *File
	var hunk *Hunk
	oldNo, newNo := 0, 0
	for _, raw := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			cur = &File{}
			hunk = nil
			files = append(files, cur)
			parts := strings.SplitN(strings.TrimPrefix(raw, "diff --git "), " b/", 2)
			if len(parts) == 2 {
				cur.OldPath = strings.TrimPrefix(parts[0], "a/")
				cur.Path = parts[1]
			}
		case cur == nil:
		case hunk != nil && len(raw) > 0 && strings.IndexByte(" +-\\", raw[0]) >= 0:
			switch raw[0] {
			case '\\':
			case ' ':
				hunk.Lines = append(hunk.Lines, Line{Kind: ' ', Text: raw[1:], OldNo: oldNo, NewNo: newNo})
				oldNo++
				newNo++
			case '+':
				hunk.Lines = append(hunk.Lines, Line{Kind: '+', Text: raw[1:], NewNo: newNo})
				newNo++
			case '-':
				hunk.Lines = append(hunk.Lines, Line{Kind: '-', Text: raw[1:], OldNo: oldNo})
				oldNo++
			}
		case hunk != nil && raw == "":
			hunk.Lines = append(hunk.Lines, Line{Kind: ' ', OldNo: oldNo, NewNo: newNo})
			oldNo++
			newNo++
		case strings.HasPrefix(raw, "@@ "):
			oldNo, newNo = hunkStart(raw)
			cur.Hunks = append(cur.Hunks, Hunk{Header: raw})
			hunk = &cur.Hunks[len(cur.Hunks)-1]
		case strings.HasPrefix(raw, "--- "):
			if raw == "--- /dev/null" {
				cur.Added = true
			}
		case strings.HasPrefix(raw, "+++ "):
			if raw == "+++ /dev/null" {
				cur.Deleted = true
				cur.Path = cur.OldPath
			}
		case strings.HasPrefix(raw, "rename from "):
			cur.Renamed = true
		case strings.HasPrefix(raw, "Binary files "), strings.HasPrefix(raw, "GIT binary patch"):
			cur.Binary = true
		}
	}
	return files
}

func hunkStart(header string) (oldNo, newNo int) {
	fields := strings.Fields(header)
	if len(fields) < 3 {
		return 1, 1
	}
	return rangeStart(fields[1]), rangeStart(fields[2])
}

func rangeStart(r string) int {
	r = strings.TrimLeft(r, "+-")
	if i := strings.IndexByte(r, ','); i >= 0 {
		r = r[:i]
	}
	n, err := strconv.Atoi(r)
	if err != nil {
		return 1
	}
	return n
}

func Ignored(p string, patterns []string) bool {
	base := path.Base(p)
	for _, pat := range patterns {
		if strings.HasSuffix(pat, "/") {
			if strings.HasPrefix(p, pat) || strings.Contains(p, "/"+pat) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
	}
	return false
}

func (f *File) Render() string {
	var b strings.Builder
	b.WriteString("### ")
	b.WriteString(f.Path)
	switch {
	case f.Added:
		b.WriteString(" (new file)")
	case f.Deleted:
		b.WriteString(" (deleted)")
	case f.Renamed:
		fmt.Fprintf(&b, " (renamed from %s)", f.OldPath)
	}
	b.WriteString("\n")
	if f.Binary {
		b.WriteString("(binary)\n")
		return b.String()
	}
	for _, h := range f.Hunks {
		b.WriteString(h.Header)
		b.WriteString("\n")
		for _, l := range h.Lines {
			if l.NewNo > 0 {
				fmt.Fprintf(&b, "%5d %c %s\n", l.NewNo, l.Kind, l.Text)
			} else {
				fmt.Fprintf(&b, "      %c %s\n", l.Kind, l.Text)
			}
		}
	}
	return b.String()
}
