// Package shell generates the code `julienning shell-init` prints. It is a
// pure function of the team nicknames and the default dir path so it can be
// unit-tested and, where the shells are installed, syntax-checked and
// executed in tests.
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

// wrapper returns the claude function, identical for zsh and bash: `local`
// inside a function and the POSIX test syntax are valid in both.
//
// CRITICAL: it is defined as `function claude {`, never `claude() {`. With
// the POSIX form an existing `alias claude=...` (Claude's own installer adds
// one) is expanded inside the definition, which breaks the whole eval in zsh
// and bash. After the `function` keyword the name is not alias-expanded; the
// alias still applies when the user types `claude`, and its flags reach the
// wrapper, which passes them through.
//
// The default-dir rule: when `current` holds Claude's default dir the
// function runs plain `command claude`, because CLAUDE_CONFIG_DIR=~/.claude
// set explicitly makes Claude read a different account file and credential.
// defaultDir is compared as an absolute path baked in at generation time; an
// empty defaultDir omits the comparison.
func wrapper(defaultDir string) string {
	notDefault := ""
	if defaultDir != "" {
		notDefault = ` && [ "$_jl_dir" != ` + singleQuote(defaultDir) + ` ]`
	}
	return `# julienning shell integration (generated; do not edit)
function claude {
  if [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then command claude "$@"; return; fi
  local _jl_dir
  _jl_dir=$(command cat "${JULIENNING_HOME:-$HOME/.julienning}/current" 2>/dev/null)
  if [ -n "$_jl_dir" ]` + notDefault + ` && [ -d "$_jl_dir" ]; then
    CLAUDE_CONFIG_DIR="$_jl_dir" command claude "$@"
  else
    command claude "$@"
  fi
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
// exactly as claudecfg.DefaultDir, which is what defaultDir holds).
func nicknameFunction(nick, defaultDir string) string {
	run := `CLAUDE_CONFIG_DIR="$_jl_dir" command claude "$@"`
	if defaultDir != "" {
		run = `if [ "$_jl_dir" = ` + singleQuote(defaultDir) + ` ]; then env -u CLAUDE_CONFIG_DIR claude "$@"; else ` + run + `; fi`
	}
	return "function claude-" + nick + " { local _jl_dir; _jl_dir=$(command julienning resolve-dir " + singleQuote(nick) + ") || return 1; " + run + "; }\n"
}

// Init returns the shell code for the given shell: the claude wrapper plus
// one claude-<nick> function per team nickname, sorted and deduplicated so
// the output is stable across runs. defaultDir is Claude's default config
// dir (claudecfg.DefaultDir). An invalid nickname is an error; callers drop
// those first, since one bad record must not break every shell start.
func Init(shellName string, nicknames []string, defaultDir string) (string, error) {
	if !Supported(shellName) {
		return "", fmt.Errorf("unsupported shell %q (want zsh or bash)", shellName)
	}
	if defaultDir != "" {
		defaultDir = filepath.Clean(defaultDir)
	}
	seen := map[string]bool{}
	nicks := make([]string, 0, len(nicknames))
	for _, n := range nicknames {
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
	b.WriteString(wrapper(defaultDir))
	for _, n := range nicks {
		b.WriteString(nicknameFunction(n, defaultDir))
	}
	return b.String(), nil
}

// singleQuote wraps s in single quotes. A literal quote inside s closes the
// quote, emits an escaped one and reopens: the only way to quote it in POSIX
// shells.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
