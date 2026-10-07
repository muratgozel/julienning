package sessions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxGitFile bounds how much of a `.git` or `commondir` file is read; real
// ones are a single short line.
const maxGitFile = 4 << 10

// gitRoot returns the directory Claude Code keys a project's auto-memory by:
// the top level of the main worktree of the repository containing dir (the
// parent of `git rev-parse --git-common-dir`), so every worktree and
// subdirectory of one repository shares one memory folder. ok is false when
// dir is not inside a repository.
//
// It reads the repository layout instead of running git (no subprocess, and
// it still works when git is missing):
//   - `.git` is a directory → its parent is the root;
//   - `.git` is a file ("gitdir: <path>", a linked worktree) → the gitdir's
//     `commondir` names the shared git dir, whose parent is the root;
//   - a `.git` file without `commondir` (submodule, --separate-git-dir) →
//     the directory holding the `.git` file is the root;
//   - a common dir not named `.git` (worktrees of a bare repository) → the
//     common dir itself, as Claude Code does, since there is no main
//     worktree.
//
// CRITICAL for future agents: this mirrors Claude Code's
// findCanonicalGitRoot as observed in 2.1.28x. If memory stops following
// sessions between config dirs, compare this with how Claude names
// projects/<enc>/memory for a worktree.
func gitRoot(dir string) (root string, ok bool) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", false
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		gitPath := filepath.Join(d, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			switch {
			case fi.IsDir():
				return d, true
			case fi.Mode().IsRegular():
				return linkedWorktreeRoot(d, gitPath), true
			}
		}
		if filepath.Dir(d) == d {
			return "", false
		}
	}
}

// linkedWorktreeRoot resolves a `.git` file in top to the main worktree's
// top level, falling back to top when the file is not a linked worktree's.
func linkedWorktreeRoot(top, gitFile string) string {
	line, err := readGitLine(gitFile)
	if err != nil || !strings.HasPrefix(line, "gitdir:") {
		return top
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if gitDir == "" {
		return top
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}
	common, err := readGitLine(filepath.Join(gitDir, "commondir"))
	if err != nil || common == "" {
		return top // submodule or separate git dir: no shared repository
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return common // bare repository
	}
	return filepath.Dir(common)
}

func readGitLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxGitFile))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(line), nil
}

// memoryProject returns the projects/ folder (in srcRoot, and by the same
// name in the target) whose memory/ belongs to s: the folder of s's git
// root, or s's own folder when s is not in a repository (Claude keys memory
// by the project root it started in, which is what s.ProjectDir encodes).
// An empty name with a nil error means there is no memory folder to merge.
func memoryProject(s Session, srcRoot string) (string, error) {
	root, ok := gitRoot(s.Cwd)
	if !ok {
		return s.ProjectDir, nil
	}
	enc := EncodeCwd(root)
	if len(enc) <= maxEncodedLen {
		return enc, nil
	}
	if filepath.Clean(s.Cwd) == root && strings.HasPrefix(s.ProjectDir, enc[:maxEncodedLen]) {
		return s.ProjectDir, nil // the session was started at the root itself
	}
	// Claude cuts long names at 200 characters and appends a hash we cannot
	// reproduce. The hash is the same in every config dir on this machine,
	// so finding the folder in the source names it in the target too.
	projects := filepath.Join(srcRoot, "projects")
	entries, err := os.ReadDir(projects)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmtErr("read", projects, err)
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || len(name) <= maxEncodedLen || !strings.HasPrefix(name, enc[:maxEncodedLen]) || !validProjectDir(name) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(projects, name, "memory")); err == nil && fi.IsDir() {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("several project folders in %s could hold the memory of %s (%s); merge the right memory/ folder by hand",
		srcRoot, root, strings.Join(found, ", "))
}
