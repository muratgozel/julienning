package sessions

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GitRoot is the directory Claude Code keys a project's auto-memory by (see
// gitRoot): the main worktree's top level of the repository containing dir.
func GitRoot(dir string) (string, bool) { return gitRoot(dir) }

// RecentCwds returns the working directory of the newest session in every
// project folder of the given config dirs (projects/<enc>/<id>.jsonl, newest
// by mtime, agent-* files left out), read from the first chunk of the file
// only: where Claude was used on this machine, for locating a project
// (hosts.Locate). Deduplicated and sorted; unreadable folders and files are
// skipped, since a missing candidate only narrows the search.
func RecentCwds(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		projects := filepath.Join(d, "projects")
		entries, err := os.ReadDir(projects)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !validProjectDir(e.Name()) {
				continue
			}
			newest := newestSession(filepath.Join(projects, e.Name()))
			if newest == "" {
				continue
			}
			if cwd := headCwd(newest); cwd != "" && filepath.IsAbs(cwd) && !seen[cwd] {
				seen[cwd] = true
				out = append(out, filepath.Clean(cwd))
			}
		}
	}
	sort.Strings(out)
	return out
}

func newestSession(folder string) string {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return ""
	}
	var best string
	var bestMod int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if m := fi.ModTime().UnixNano(); best == "" || m > bestMod {
			best, bestMod = filepath.Join(folder, name), m
		}
	}
	return best
}

// headCwd is the first cwd recorded in the first chunk of a session file.
func headCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, chunkSize)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return ""
	}
	for _, line := range bytes.Split(head[:n], []byte{'\n'}) {
		if e, ok := decode(line); ok && e.Cwd != "" {
			return e.Cwd
		}
	}
	return ""
}
