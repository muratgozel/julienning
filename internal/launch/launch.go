// Package launch starts the claude CLI against a config dir.
package launch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/muratgozel/julienning/internal/claudecfg"
)

// EnvVar is Claude Code's config dir selector.
const EnvVar = "CLAUDE_CONFIG_DIR"

// ErrClaudeNotFound is returned when claude is not on PATH.
var ErrClaudeNotFound = errors.New("claude not found on PATH (install Claude Code first)")

// Env returns environ with CLAUDE_CONFIG_DIR pointing at dir. For Claude's
// default dir the variable is removed instead, because setting it explicitly
// changes where Claude looks for the login (see claudecfg.DefaultDir).
func Env(environ []string, dir string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, EnvVar+"=") {
			out = append(out, kv)
		}
	}
	if !claudecfg.IsDefaultDir(dir) {
		out = append(out, EnvVar+"="+dir)
	}
	return out
}

// ClaudePath finds the claude binary on PATH.
func ClaudePath() (string, error) {
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", ErrClaudeNotFound
	}
	return p, nil
}

// Exec replaces the current process with `claude args...` using dir as the
// config dir. chdir, when non-empty and existing, becomes the working
// directory first (resuming a session from another project). A var so
// commands can be tested without replacing the test process.
var Exec = func(dir string, args []string, chdir string) error {
	path, err := ClaudePath()
	if err != nil {
		return err
	}
	if chdir != "" {
		if fi, err := os.Stat(chdir); err == nil && fi.IsDir() {
			if err := os.Chdir(chdir); err != nil {
				return fmt.Errorf("change directory to %s: %w", chdir, err)
			}
		}
	}
	argv := append([]string{"claude"}, args...)
	if err := syscall.Exec(path, argv, Env(os.Environ(), dir)); err != nil {
		return fmt.Errorf("run claude: %w", err)
	}
	return nil
}
