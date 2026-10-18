package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/shell"
)

// Tmux is a tmux binary new enough for tmux mode.
type Tmux struct {
	Path    string // absolute
	Version string // as `tmux -V` prints it, without the leading "tmux "
}

// ErrTmuxNotFound is returned by FindTmux when no tmux binary exists.
var ErrTmuxNotFound = errors.New("tmux is not installed")

// TmuxTooOldError is a tmux older than tmux mode needs.
type TmuxTooOldError struct{ Path, Version string }

func (e *TmuxTooOldError) Error() string {
	return fmt.Sprintf("%s is tmux %s; tmux mode needs tmux 2.1 or newer", e.Path, e.Version)
}

// tmuxMin is the oldest tmux tmux mode works with. Checked against tmux's
// CHANGES: `=name` exact targets arrived in 2.1 (without them `claude`
// matches a session named claude2 by prefix), new-window and new-session -c
// in 1.9, and since 2.0 a window command given as several arguments is
// executed directly, without a shell, so nothing needs quoting for one.
var tmuxMin = [2]int{2, 1}

// tmuxDirs are searched after PATH: tmux is often outside the PATH a
// non-interactive shell or an early startup file sees.
var tmuxDirs = func() []string {
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin", "/run/current-system/sw/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append([]string{filepath.Join(home, ".local", "bin")}, dirs...)
		dirs = append(dirs, filepath.Join(home, ".nix-profile", "bin"))
	}
	return dirs
}

// FindTmux finds tmux on PATH, then in tmuxDirs, and checks its version.
func FindTmux() (Tmux, error) {
	path := lookTmux()
	if path == "" {
		return Tmux{}, ErrTmuxNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-V").Output()
	if err != nil {
		return Tmux{}, fmt.Errorf("run %s -V: %w", path, err)
	}
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "tmux"))
	if !tmuxNewEnough(v) {
		return Tmux{}, &TmuxTooOldError{Path: path, Version: v}
	}
	return Tmux{Path: path, Version: v}, nil
}

func lookTmux() string {
	if p, err := exec.LookPath("tmux"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	for _, d := range tmuxDirs() {
		p := filepath.Join(d, "tmux")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

var tmuxVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// tmuxNewEnough reads `tmux -V` without its prefix: "3.5a", "next-3.6",
// "2.1". A build that prints no number ("master") is a development build,
// newer than any release.
func tmuxNewEnough(v string) bool {
	m := tmuxVersionRe.FindStringSubmatch(v)
	if m == nil {
		return true
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > tmuxMin[0] || (major == tmuxMin[0] && minor >= tmuxMin[1])
}

// TmuxApplies is the launch rule's test of the surroundings: claude goes into
// tmux only from a terminal (stdin and stdout), never from inside tmux
// ($TMUX), and never from VS Code's terminal, which manages its own.
func TmuxApplies(getenv func(string) string, terminals bool) bool {
	return terminals && getenv("TMUX") == "" && getenv("TERM_PROGRAM") != "vscode"
}

// WindowCommand is the command a tmux window runs for claude on dir (""
// or Claude's default dir: CLAUDE_CONFIG_DIR unset, the default-dir rule).
//
// CRITICAL: the account is always explicit. A tmux window takes its
// environment from the tmux server, which keeps the environment of whichever
// shell started it, not from the shell that asked for the window; an account
// left implicit would be whatever that first shell had.
func WindowCommand(dir, claudePath string, args []string) []string {
	cmd := []string{"/bin/sh", "-c", shell.WindowScript, "julienning", "env"}
	if dir == "" || claudecfg.IsDefaultDir(dir) {
		cmd = append(cmd, "-u", EnvVar)
	} else {
		cmd = append(cmd, EnvVar+"="+dir)
	}
	cmd = append(cmd, claudePath)
	return append(cmd, args...)
}

// The tmux invocations of the launch rule, without the binary. Every target
// is exact (`=name`): a plain `claude` would also match `claude2`.
func HasSessionArgs(session string) []string { return []string{"has-session", "-t", "=" + session} }
func AttachArgs(session string) []string     { return []string{"attach-session", "-t", "=" + session} }

func NewWindowArgs(session, cwd string, window []string) []string {
	return append([]string{"new-window", "-t", "=" + session + ":", "-c", cwd}, window...)
}

func NewSessionArgs(session, cwd string, window []string) []string {
	return append([]string{"new-session", "-s", session, "-c", cwd}, window...)
}

// tmuxEnv is the environment tmux runs with: a new server keeps it for every
// later window, so no account may ride along in it.
func tmuxEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, EnvVar+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// tmuxFor decides whether Exec runs claude in tmux: tmux mode on in
// config.json, the surroundings (TmuxApplies) and tmux found. A var so tests
// can decide without a terminal.
var tmuxFor = func(stderr io.Writer) (Tmux, string, bool) {
	cfg, err := config.Load()
	if err != nil {
		return Tmux{}, "", false
	}
	session, on := cfg.TmuxSession()
	if !on || !TmuxApplies(os.Getenv, isTerminal(os.Stdin) && isTerminal(os.Stdout)) {
		return Tmux{}, "", false
	}
	t, err := FindTmux()
	if err != nil {
		fmt.Fprintf(stderr, "julienning: warning: tmux mode is on, but %v; running claude here (turn it off with: julienning setup --no-tmux)\n", err)
		return Tmux{}, "", false
	}
	return t, session, true
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// ExecTmux replaces the process with tmux showing claude on dir in a window
// of session, started in the working directory: a new window in the session
// when it exists, then attached; otherwise a new session with claude as its
// first window. A new window becomes the current window of every client
// attached to the session (tmux's behaviour).
func ExecTmux(t Tmux, session, dir, claudePath string, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determine the current directory: %w", err)
	}
	env := tmuxEnv(os.Environ())
	window := WindowCommand(dir, claudePath, args)
	has := exec.Command(t.Path, HasSessionArgs(session)...)
	has.Env = env
	if has.Run() == nil {
		nw := exec.Command(t.Path, NewWindowArgs(session, cwd, window)...)
		nw.Env = env
		if out, err := nw.CombinedOutput(); err != nil {
			return fmt.Errorf("open a window in tmux session %q: %v%s", session, err, outputDetail(out))
		}
		if err := syscall.Exec(t.Path, append([]string{"tmux"}, AttachArgs(session)...), env); err != nil {
			return fmt.Errorf("run tmux: %w", err)
		}
		return nil
	}
	if err := syscall.Exec(t.Path, append([]string{"tmux"}, NewSessionArgs(session, cwd, window)...), env); err != nil {
		return fmt.Errorf("run tmux: %w", err)
	}
	return nil
}

func outputDetail(out []byte) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		return " (" + s + ")"
	}
	return ""
}
