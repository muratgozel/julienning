package sessions

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/muratgozel/julienning/internal/config"
)

// memoryIndex is the project memory index Claude Code loads first; the other
// files in memory/ are topic files it points to.
const memoryIndex = "MEMORY.md"

// maxMemoryFile bounds how much of a MEMORY.md we are willing to merge.
const maxMemoryFile = 4 << 20

// MemoryReport says what mergeMemory did. Paths are relative to memory/.
type MemoryReport struct {
	Added      []string // files the target lacked, copied over
	IndexLines int      // MEMORY.md lines appended to the target's index
	Conflicts  []string // files present on both sides with different content, kept as-is
}

func (m MemoryReport) summary() string {
	var added, lines, conflicts string
	if n := len(m.Added); n > 0 {
		added = plural(n, "file", "files") + " added"
	}
	if m.IndexLines > 0 {
		lines = plural(m.IndexLines, "index line", "index lines") + " merged"
	}
	if n := len(m.Conflicts); n > 0 {
		conflicts = plural(n, "conflict", "conflicts") + " kept"
	}
	return joinNonEmpty(added, lines, conflicts)
}

// mergeMemory merges the source project's memory/ into the target's without
// ever overwriting: missing files are copied, MEMORY.md gains the index lines
// it lacks, and differing files are reported and left alone. It keeps going
// after a per-file failure and returns the problems joined.
//
// MEMORY.md is merged last, once every conflict is known, so that index
// lines pointing at a conflicted file are not appended (they describe the
// source's version, which the target does not have).
func mergeMemory(srcDir, dstDir string) (MemoryReport, error) {
	var rep MemoryReport
	fi, err := os.Stat(srcDir)
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	if !fi.IsDir() {
		return rep, fmt.Errorf("%s is not a directory", srcDir)
	}
	var problems []error
	var index string // the source's MEMORY.md, when the target has a different one
	walkErr := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			problems = append(problems, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return nil // created on demand for the files inside
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and specials in memory/ are not ours to replicate
		}
		if err := fail("memory", p); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", rel, err))
			return nil
		}
		ok, err := exists(dst)
		if err != nil {
			problems = append(problems, err)
			return nil
		}
		if !ok {
			if err := copyNew(p, dst); err != nil {
				problems = append(problems, fmt.Errorf("copy %s: %w", rel, err))
				return nil
			}
			rep.Added = append(rep.Added, rel)
			return nil
		}
		same, err := sameContent(p, dst)
		if err != nil {
			problems = append(problems, err)
			return nil
		}
		if same {
			return nil
		}
		if rel == memoryIndex {
			index = p
			return nil
		}
		rep.Conflicts = append(rep.Conflicts, rel)
		return nil
	})
	if walkErr != nil {
		problems = append(problems, walkErr)
	}
	if index != "" {
		conflicted := map[string]bool{}
		for _, c := range rep.Conflicts {
			conflicted[filepath.ToSlash(c)] = true
		}
		n, err := mergeIndex(index, filepath.Join(dstDir, memoryIndex), conflicted)
		if err != nil {
			problems = append(problems, fmt.Errorf("merge %s: %w", memoryIndex, err))
		}
		rep.IndexLines += n
	}
	return rep, errors.Join(problems...)
}

// copyNew copies one file to a path that does not exist yet, through a
// temporary sibling so a crash never leaves a half-written memory file.
func copyNew(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	perm := os.FileMode(0o700)
	if pfi, err := os.Stat(filepath.Dir(src)); err == nil {
		perm = pfi.Mode().Perm()
	}
	if _, err := mkdirAllTracked(filepath.Dir(dst), perm); err != nil {
		return err
	}
	tmp, err := tempName(dst)
	if err != nil {
		return err
	}
	if err := copyFile(src, tmp, fi, nil); err != nil {
		return errors.Join(err, removeTemp(tmp))
	}
	// Link instead of rename: it fails if dst appeared meanwhile, so a
	// concurrent writer's file is never replaced.
	if err := os.Link(tmp, dst); err != nil {
		return errors.Join(err, removeTemp(tmp))
	}
	return removeTemp(tmp)
}

// removeTemp removes a temporary file this package created, if present.
func removeTemp(tmp string) error {
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temporary %s: %w", tmp, err)
	}
	return nil
}

func sameContent(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if fa.Size() != fb.Size() {
		return false, nil
	}
	if fa.Size() > maxMemoryFile {
		return false, nil
	}
	ra, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	rb, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ra, rb), nil
}

// mergeIndex appends to dst every non-blank line of src that dst lacks
// (compared with surrounding whitespace trimmed), in src's order, except
// lines whose first markdown link points at a conflicted file or at a file
// some dst line (or an earlier appended line) already links to: one topic
// file gets one index entry. conflicted holds slash-separated paths
// relative to memory/. Returns how many lines were appended.
//
// When dst is a symlink (a MEMORY.md shared between config dirs), the link
// is kept and the file it points to gets the lines.
func mergeIndex(src, dst string, conflicted map[string]bool) (int, error) {
	real, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return 0, err
	}
	for _, p := range []string{src, real} {
		fi, err := os.Stat(p)
		if err != nil {
			return 0, err
		}
		if fi.Size() > maxMemoryFile {
			return 0, fmt.Errorf("%s is larger than %d MB; merge it by hand", p, maxMemoryFile>>20)
		}
	}
	srcRaw, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}
	dstRaw, err := os.ReadFile(real)
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	linked := map[string]bool{}
	for _, l := range strings.Split(string(dstRaw), "\n") {
		have[strings.TrimSpace(l)] = true
		if t := indexLink(l); t != "" {
			linked[t] = true
		}
	}
	var add []string
	for _, l := range strings.Split(string(srcRaw), "\n") {
		key := strings.TrimSpace(l)
		if key == "" || have[key] {
			continue
		}
		if t := indexLink(l); t != "" {
			if conflicted[t] || linked[t] {
				continue
			}
			linked[t] = true
		}
		have[key] = true
		add = append(add, strings.TrimRight(l, " \t\r"))
	}
	if len(add) == 0 {
		return 0, nil
	}
	out := dstRaw
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, strings.Join(add, "\n")+"\n"...)
	fi, err := os.Stat(real)
	if err != nil {
		return 0, err
	}
	if err := config.WriteFileAtomic(real, out, fi.Mode().Perm()); err != nil {
		return 0, err
	}
	return len(add), nil
}

// linkRe finds the target of the first inline markdown link on a line:
// "[text](target)", "[text](<target with spaces>)", "[text](target "title")".
var linkRe = regexp.MustCompile(`\[[^\]]*\]\(\s*(<[^>]*>|[^)\s]+)`)

// indexLink returns the memory file the first link on an index line points
// to, as a clean slash-separated path relative to memory/, or "" when the
// line has no link to a local file (URLs and in-page anchors do not count).
func indexLink(line string) string {
	m := linkRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	t := strings.TrimSuffix(strings.TrimPrefix(m[1], "<"), ">")
	if i := strings.IndexAny(t, "#?"); i >= 0 {
		t = t[:i]
	}
	if u, err := url.PathUnescape(t); err == nil {
		t = u
	}
	if t == "" || schemeRe.MatchString(t) {
		return ""
	}
	t = path.Clean(filepath.ToSlash(t))
	if t == "." {
		return ""
	}
	return t
}

// schemeRe matches a URL scheme ("https:", "mailto:").
var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
