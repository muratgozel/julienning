// Package shell generates the code `julienning shell-init` prints. It is a
// pure function of its Options (team nicknames, the default dir, where
// julienning and tmux live) so it can be unit-tested and, where the shells
// are installed, syntax-checked and executed in tests.
package shell

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Supported shells. No fish: its function/alias syntax differs entirely.
const (
	Zsh  = "zsh"
	Bash = "bash"
)

// Supported reports whether name is a shell we can generate code for.
func Supported(name string) bool { return name == Zsh || name == Bash }

var nicknameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// NicknameRule describes a valid nickname for error messages.
const NicknameRule = "1-32 characters: lowercase letters, digits, dot, underscore or dash, starting with a letter or digit"

// ValidNickname reports whether s is a valid team nickname. Init depends on
// it as its injection guard: nicknames come from the Worker and become shell
// function names, which cannot be quoted.
func ValidNickname(s string) bool { return nicknameRe.MatchString(s) }

// tmuxSessionRe mirrors config.ValidTmuxSession (config imports this
// package, so it cannot be called from here).
var tmuxSessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// WindowScript is the script a tmux window runs claude under (the launch
// rule of tmux mode; launch.WindowCommand passes it the same way): when
// claude exits the window becomes the user's shell. `trap : INT` installs a
// handler rather than ignoring the signal, so claude still gets Ctrl-C
// (exec resets handlers, not ignores) while the script survives it.
const WindowScript = `trap : INT; "$@"; exec "${SHELL:-/bin/sh}"`

// Tmux is tmux mode as baked into the shell code.
type Tmux struct {
	Path    string // absolute path of a tmux 2.1 or newer
	Session string // config.ValidTmuxSession
}

// Options is everything the generated code depends on.
type Options struct {
	Nicknames []string
	// DefaultDir is Claude's default config dir (claudecfg.DefaultDir); ""
	// omits the default-dir comparisons.
	DefaultDir string
	// Julienning is the absolute path the functions call julienning by
	// (paths.StableCommand when stable); "" means `command julienning`,
	// looked up on PATH at call time.
	Julienning string
	// Tmux is set when tmux mode is on and tmux was found.
	Tmux *Tmux
}

func (o Options) julienning() string {
	if o.Julienning == "" {
		return "command julienning"
	}
	return singleQuote(o.Julienning)
}

// runs are the commands that start claude: on an explicit dir (an exported
// CLAUDE_CONFIG_DIR, the selection or a nickname's dir) and with the
// variable unset (no selection, or Claude's default dir). In tmux mode both
// go through _julienning_claude, which applies the launch rule.
func (o Options) runs(dirExpr string) (onDir, unset string) {
	if o.Tmux != nil {
		return `_julienning_claude ` + dirExpr + ` "$@"`, `_julienning_claude '' "$@"`
	}
	return `CLAUDE_CONFIG_DIR=` + dirExpr + ` command claude "$@"`, `env -u CLAUDE_CONFIG_DIR claude "$@"`
}

// wrapper returns the claude function, identical for zsh and bash: `local`
// inside a function and the POSIX test syntax are valid in both.
//
// CRITICAL: it is defined as `function claude {`, never `claude() {`. With
// the POSIX form an existing `alias claude=...` (Claude's own installer adds
// one) is expanded inside the definition, which breaks the whole eval in zsh
// and bash. After the `function` keyword the name is not alias-expanded; the
// alias still applies when the user types `claude`, and its flags reach the
// wrapper, which passes them through. A `claude()` function defined earlier
// in the rc file (provisioning scripts write one) is simply redefined.
//
// The default-dir rule: when `current` holds Claude's default dir the
// function runs claude with the variable unset, because
// CLAUDE_CONFIG_DIR=~/.claude set explicitly makes Claude read a different
// account file and credential. defaultDir is compared as an absolute path
// baked in at generation time; an empty defaultDir omits the comparison.
//
// Pins come before the selection, and only cost a julienning process while
// pins.json (config.PinsFile) exists. `resolve-dir --pin` prints nothing
// when no pin covers the working directory; when a pin does but its account
// cannot be opened it fails, and the wrapper returns 1 instead of falling
// back to the selection, so a pinned project never opens on another account.
func wrapper(o Options) string {
	notDefault := ""
	if o.DefaultDir != "" {
		notDefault = ` && [ "$_jl_dir" != ` + singleQuote(o.DefaultDir) + ` ]`
	}
	exported, _ := o.runs(`"$CLAUDE_CONFIG_DIR"`)
	onDir, unset := o.runs(`"$_jl_dir"`)
	if o.Tmux == nil {
		// Without tmux mode the variable is empty or unset in both places,
		// so plain `command claude` is enough (and was always generated).
		exported, unset = `command claude "$@"`, `command claude "$@"`
	}
	return `# julienning shell integration (generated; do not edit)
function claude {
  if [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then ` + exported + `; return; fi
  local _jl_dir _jl_home
  _jl_home="${JULIENNING_HOME:-$HOME/.julienning}"
  _jl_dir=
  if [ -f "$_jl_home/pins.json" ]; then
    _jl_dir=$(` + o.julienning() + ` resolve-dir --pin) || return 1
  fi
  if [ -z "$_jl_dir" ]; then
    _jl_dir=$(command cat "$_jl_home/current" 2>/dev/null)
  fi
  if [ -n "$_jl_dir" ]` + notDefault + ` && [ -d "$_jl_dir" ]; then
    ` + onDir + `
  else
    ` + unset + `
  fi
}
`
}

// tmuxHelper returns _julienning_claude DIR ARGS..., the shell side of tmux
// mode's launch rule (launch.ExecTmux is the Go side; keep them alike). DIR
// "" means CLAUDE_CONFIG_DIR unset. From a terminal, outside tmux and VS
// Code, claude runs in a window of the session: a new window when it exists
// (then attached), else a new session. Otherwise it runs in place.
//
// CRITICAL: every target is quoted. zsh expands an unquoted word starting
// with `=` to a command's path (`=claude` → /usr/local/bin/claude), and the
// `=` is what makes tmux match the session name exactly. claude's path comes
// from a PATH search (bash `type -P`, zsh `whence -p`), never `command -v`,
// which names this very function. The account goes on the window's command
// line explicitly (the tmux server's environment is whichever shell started
// it), and a new server is started without CLAUDE_CONFIG_DIR.
func tmuxHelper(shellName string, t Tmux) string {
	find := `type -P claude`
	if shellName == Zsh {
		find = `whence -p claude`
	}
	tm := singleQuote(t.Path)
	exact := singleQuote("=" + t.Session)
	return `function _julienning_claude {
  local _jl_dir="$1" _jl_bin
  shift
  if [ -t 0 ] && [ -t 1 ] && [ -z "${TMUX:-}" ] && [ "${TERM_PROGRAM:-}" != vscode ] && [ -x ` + tm + ` ] && _jl_bin=$(` + find + `); then
    if [ -n "$_jl_dir" ]; then
      set -- "CLAUDE_CONFIG_DIR=$_jl_dir" "$_jl_bin" "$@"
    else
      set -- -u CLAUDE_CONFIG_DIR "$_jl_bin" "$@"
    fi
    set -- /bin/sh -c ` + singleQuote(WindowScript) + ` julienning env "$@"
    if ` + tm + ` has-session -t ` + exact + ` 2>/dev/null; then
      ` + tm + ` new-window -t ` + singleQuote("="+t.Session+":") + ` -c "$PWD" "$@" && ` + tm + ` attach-session -t ` + exact + `
    else
      env -u CLAUDE_CONFIG_DIR ` + tm + ` new-session -s ` + singleQuote(t.Session) + ` -c "$PWD" "$@"
    fi
    return
  fi
  if [ -n "$_jl_dir" ]; then CLAUDE_CONFIG_DIR="$_jl_dir" command claude "$@"; else env -u CLAUDE_CONFIG_DIR claude "$@"; fi
}
`
}

// nicknameFunction returns `claude-<nick>`, which asks `julienning
// resolve-dir` for the account's dir every time it runs: logins move between
// dirs, so baking a path in at shell start would go stale. resolve-dir
// prints its own error, so a failure only needs `return 1`. The split
// `local` / assignment keeps the command's exit status (`local x=$(…)`
// would mask it). Like the wrapper, the `function` keyword keeps a
// same-named alias from breaking the definition, and the default dir is
// launched with CLAUDE_CONFIG_DIR unset (resolve-dir prints the default dir
// exactly as claudecfg.DefaultDir, which is what DefaultDir holds).
func nicknameFunction(nick string, o Options) string {
	run, unset := o.runs(`"$_jl_dir"`)
	if o.DefaultDir != "" {
		run = `if [ "$_jl_dir" = ` + singleQuote(o.DefaultDir) + ` ]; then ` + unset + `; else ` + run + `; fi`
	}
	return "function claude-" + nick + " { local _jl_dir; _jl_dir=$(" + o.julienning() + " resolve-dir " + singleQuote(nick) + ") || return 1; " + run + "; }\n"
}

// Init returns the shell code for the given shell: the claude wrapper plus
// one claude-<nick> function per team nickname, sorted and deduplicated so
// the output is stable across runs, and in tmux mode the helper both use.
// An invalid nickname is an error; callers drop those first, since one bad
// record must not break every shell start.
func Init(shellName string, o Options) (string, error) {
	if !Supported(shellName) {
		return "", fmt.Errorf("unsupported shell %q (want zsh or bash)", shellName)
	}
	if o.DefaultDir != "" {
		o.DefaultDir = filepath.Clean(o.DefaultDir)
	}
	if o.Julienning != "" && !filepath.IsAbs(o.Julienning) {
		return "", fmt.Errorf("julienning path %q is not absolute", o.Julienning)
	}
	if t := o.Tmux; t != nil {
		switch {
		case !filepath.IsAbs(t.Path):
			return "", fmt.Errorf("tmux path %q is not absolute", t.Path)
		case !tmuxSessionRe.MatchString(t.Session):
			return "", fmt.Errorf("invalid tmux session name %q", t.Session)
		}
	}
	seen := map[string]bool{}
	nicks := make([]string, 0, len(o.Nicknames))
	for _, n := range o.Nicknames {
		if !ValidNickname(n) {
			return "", fmt.Errorf("invalid nickname %q (%s)", n, NicknameRule)
		}
		if !seen[n] {
			seen[n] = true
			nicks = append(nicks, n)
		}
	}
	sort.Strings(nicks)

	var b strings.Builder
	b.WriteString(wrapper(o))
	if o.Tmux != nil {
		b.WriteString(tmuxHelper(shellName, *o.Tmux))
	}
	for _, n := range nicks {
		b.WriteString(nicknameFunction(n, o))
	}
	return b.String(), nil
}

// singleQuote wraps s in single quotes. A literal quote inside s closes the
// quote, emits an escaped one and reopens: the only way to quote it in POSIX
// shells.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SingleQuote is singleQuote for other packages (setup's rc line).
func SingleQuote(s string) string { return singleQuote(s) }
