// Package paths resolves where julienning is installed. The layout mirrors
// Claude Code's native installer: versioned binaries under
// ${XDG_DATA_HOME:-~/.local/share}/julienning/versions/<version> and a stable
// symlink at ~/.local/bin/julienning that updates repoint.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Env overrides (tests, unusual layouts).
const (
	EnvBinDir      = "JULIENNING_BIN_DIR"
	EnvVersionsDir = "JULIENNING_VERSIONS_DIR"
)

// BinDir is where the stable symlink lives.
func BinDir() (string, error) {
	if d := os.Getenv(EnvBinDir); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// BinLink is the stable path of the julienning command.
func BinLink() (string, error) {
	d, err := BinDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "julienning"), nil
}

// VersionsDir holds one binary file per installed version.
func VersionsDir() (string, error) {
	if d := os.Getenv(EnvVersionsDir); d != "" {
		return d, nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "julienning", "versions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "julienning", "versions"), nil
}

// Executable is the running binary with symlinks resolved; a var for tests.
var Executable = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running executable: %w", err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	return p, nil
}

// FindLink returns a `julienning` symlink that resolves to the running
// binary: $JULIENNING_BIN_DIR/julienning, then ~/.local/bin/julienning, then
// the first `julienning` on PATH. The PATH scan matters because the installer
// honors JULIENNING_BIN_DIR but later runs usually do not have it set.
func FindLink() (string, bool) {
	exe, err := Executable()
	if err != nil {
		return "", false
	}
	var candidates []string
	if l, err := BinLink(); err == nil {
		candidates = append(candidates, l)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "julienning"))
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d != "" {
			candidates = append(candidates, filepath.Join(d, "julienning"))
		}
	}
	for _, c := range candidates {
		fi, err := os.Lstat(c)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if target, err := filepath.EvalSymlinks(c); err == nil && sameFile(target, exe) {
			return c, true
		}
	}
	return "", false
}

// StableCommand is the path to write into Claude's settings.json (statusLine,
// hooks). It must survive updates, so it is a `julienning` symlink to the
// running binary (FindLink); otherwise (a dev build run from the repo) it is
// the running binary itself, and stable=false tells callers to warn.
//
// CRITICAL: settings.json ownership checks recognise julienning's entries by
// the command's basename (`julienning`) or by a versions-dir path
// (.../julienning/versions/<ver>); see IsVersionFile.
func StableCommand() (path string, stable bool, err error) {
	exe, err := Executable()
	if err != nil {
		return "", false, err
	}
	if link, ok := FindLink(); ok {
		return link, true, nil
	}
	return exe, false, nil
}

// IsVersionFile reports whether p looks like an installed version binary
// (.../julienning/versions/<version>), independent of env overrides.
func IsVersionFile(p string) bool {
	dir := filepath.Dir(filepath.Clean(p))
	return filepath.Base(dir) == "versions" && filepath.Base(filepath.Dir(dir)) == "julienning"
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}
