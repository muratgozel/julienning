// Package discover finds Claude Code config dirs on this machine (SPEC
// "Discovery"): the default dir, look-alike dirs at the top of $HOME, dirs
// assigned to CLAUDE_CONFIG_DIR in shell rc files, and the current
// $CLAUDE_CONFIG_DIR. It only reads; registering is the caller's job.
package discover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
)

// Candidate sources.
const (
	SourceDefault = "default" // ~/.claude
	SourceHome    = "home"    // a top-level entry of $HOME
	SourceEnv     = "env"     // the current $CLAUDE_CONFIG_DIR
	// rc-file candidates use the rc path, e.g. "~/.zshrc".
)

// RCFiles are the shell startup files scanned for CLAUDE_CONFIG_DIR=.
var RCFiles = []string{".zshrc", ".zprofile", ".zshenv", ".bashrc", ".bash_profile", ".profile"}

// Candidate is one discovered config dir. Email is read live from the
// account file: logins move between dirs, so it is never cached.
type Candidate struct {
	Dir        string // cleaned absolute path
	Name       string // registered name, else the name it would be registered under (AssignNames)
	Email      string // lowercased; empty unless LoggedIn
	LoggedIn   bool
	Err        error  // the account file exists but is unreadable or invalid
	Source     string // see Source* constants
	Registered bool
}

// Find returns candidates sorted with the default dir first, then by path.
// registered supplies names for already-registered dirs and the names new
// ones must not clash with; every other candidate gets the name it would be
// registered under (AssignNames with prefix, no priority).
func Find(registered []config.ConfigDir, prefix string) ([]Candidate, error) {
	if !config.ValidNamePrefix(prefix) {
		return nil, fmt.Errorf("invalid name prefix %q (%s)", prefix, config.NamePrefixRule)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory (set $HOME): %w", err)
	}
	defaultDir, err := claudecfg.DefaultDir()
	if err != nil {
		return nil, err
	}
	excluded := map[string]bool{filepath.Join(home, ".julienning"): true}
	if d, err := config.Dir(); err == nil {
		excluded[filepath.Clean(d)] = true
	}

	var found []Candidate
	seen := map[string]bool{}
	add := func(dir, source string) {
		dir = filepath.Clean(dir)
		if seen[dir] || excluded[dir] {
			return
		}
		seen[dir] = true
		found = append(found, Candidate{Dir: dir, Source: source})
	}

	if isDir(defaultDir) {
		add(defaultDir, SourceDefault)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", home, err)
	}
	for _, e := range entries {
		if skipHomeEntry(e.Name()) {
			continue
		}
		p := filepath.Join(home, e.Name())
		if isDir(p) && looksLikeConfigDir(p) {
			add(p, SourceHome)
		}
	}

	for _, rc := range RCFiles {
		dirs, err := rcAssignments(filepath.Join(home, rc), home)
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			if isDir(d) {
				add(d, "~/"+rc)
			}
		}
	}

	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		d, ok := expand(v, home)
		if !ok {
			if abs, err := filepath.Abs(v); err == nil {
				d, ok = abs, true
			}
		}
		if ok && isDir(d) && !IsUnsafeConfigDir(d, home) {
			add(d, SourceEnv)
		}
	}

	for i := range found {
		email, err := claudecfg.ReadEmail(found[i].Dir)
		switch {
		case err == nil:
			found[i].Email, found[i].LoggedIn = email, true
		case errors.Is(err, claudecfg.ErrNotLoggedIn):
		default:
			found[i].Err = err
		}
	}

	AssignNames(found, registered, prefix, nil)
	sort.SliceStable(found, func(i, j int) bool {
		di, dj := found[i].Dir == defaultDir, found[j].Dir == defaultDir
		if di != dj {
			return di
		}
		return found[i].Dir < found[j].Dir
	})
	return found, nil
}

// DefaultName is the name Claude's default dir (~/.claude) is registered
// under when julienning picks the name.
const DefaultName = "default"

// AssignNames sets Name and Registered on every candidate (SPEC "Config
// names"): a registered dir keeps its stored name; Claude's default dir is
// DefaultName; every other dir gets <prefix><N>, reusing the number its
// basename ends in when that name is free (~/.claude-x2 → <prefix>2), else
// the smallest free N ≥ 1. Names never clash with registered ones or with
// each other.
//
// Candidates for which first returns true (nil: none) are named before the
// rest, so the dirs setup is about to register get the low numbers rather
// than personal dirs that are only listed. Within each group, number reuse
// runs before smallest-free and both go in path order: ~/.claude-x sorts
// before ~/.claude-x1 but must not take x1's number.
func AssignNames(found []Candidate, registered []config.ConfigDir, prefix string, first func(Candidate) bool) {
	byDir := map[string]string{}
	taken := map[string]bool{}
	for _, cd := range registered {
		byDir[filepath.Clean(cd.Dir)] = cd.Name
		taken[cd.Name] = true
	}
	var preferred, rest []int
	for i := range found {
		if name, ok := byDir[filepath.Clean(found[i].Dir)]; ok {
			found[i].Name, found[i].Registered = name, true
			continue
		}
		found[i].Name, found[i].Registered = "", false
		if first != nil && first(found[i]) {
			preferred = append(preferred, i)
		} else {
			rest = append(rest, i)
		}
	}
	isTaken := func(n string) bool { return taken[n] }
	assign := func(i int, name string) {
		found[i].Name = name
		taken[name] = true
	}
	for _, group := range [][]int{preferred, rest} {
		sort.SliceStable(group, func(a, b int) bool { return found[group[a]].Dir < found[group[b]].Dir })
		for _, i := range group {
			if claudecfg.IsDefaultDir(found[i].Dir) && !taken[DefaultName] {
				assign(i, DefaultName)
			} else if name, ok := reusedName(found[i].Dir, prefix); ok && !taken[name] {
				assign(i, name)
			}
		}
		for _, i := range group {
			if found[i].Name == "" {
				assign(i, NextFreeName(prefix, isTaken))
			}
		}
	}
}

// AutoName is the name julienning registers dir under when no --name is
// given, by the AssignNames rules for a single dir.
func AutoName(dir, prefix string, taken func(string) bool) string {
	if claudecfg.IsDefaultDir(dir) && !taken(DefaultName) {
		return DefaultName
	}
	if name, ok := reusedName(dir, prefix); ok && !taken(name) {
		return name
	}
	return NextFreeName(prefix, taken)
}

// NextFreeName returns <prefix><N> for the smallest N ≥ 1 that is not taken.
func NextFreeName(prefix string, taken func(string) bool) string {
	for n := 1; ; n++ {
		if name := prefix + strconv.Itoa(n); !taken(name) {
			return name
		}
	}
}

// reusedName returns <prefix><N> when dir's basename ends in the decimal
// number N ≥ 1 (leading zeros dropped: x007 → 7). Only the number is ever
// taken from the basename: the rest of it may name the team or the person
// and must not reach the alias.
func reusedName(dir, prefix string) (string, bool) {
	base := filepath.Base(filepath.Clean(dir))
	end := len(base)
	start := end
	for start > 0 && base[start-1] >= '0' && base[start-1] <= '9' {
		start--
	}
	if start == end {
		return "", false
	}
	n, err := strconv.Atoi(base[start:end])
	if err != nil || n < 1 {
		return "", false
	}
	return prefix + strconv.Itoa(n), true
}

// macOSPrivate are $HOME folders never scanned on macOS. Desktop, Documents
// and Downloads are privacy-protected (TCC): merely stat-ing inside them can
// pop a permission dialog for the terminal. The rest are large media/system
// folders that are never Claude config dirs.
var macOSPrivate = map[string]bool{
	"Desktop": true, "Documents": true, "Downloads": true, "Library": true,
	"Movies": true, "Music": true, "Pictures": true, "Applications": true,
}

func skipHomeEntry(name string) bool {
	return runtime.GOOS == "darwin" && macOSPrivate[name]
}

// looksLikeConfigDir: contains .claude.json, or projects/ together with one
// of settings.json, history.jsonl, sessions/.
func looksLikeConfigDir(dir string) bool {
	if exists(filepath.Join(dir, claudecfg.AccountFile)) {
		return true
	}
	if !isDir(filepath.Join(dir, "projects")) {
		return false
	}
	return exists(filepath.Join(dir, claudecfg.SettingsFile)) ||
		exists(filepath.Join(dir, "history.jsonl")) ||
		isDir(filepath.Join(dir, "sessions"))
}

const assignPrefix = "CLAUDE_CONFIG_DIR="

// rcAssignments returns the absolute paths assigned to CLAUDE_CONFIG_DIR in
// one rc file. A missing file yields nothing.
func rcAssignments(path, home string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		out = append(out, lineAssignments(stripComment(line), home, 0)...)
	}
	return out, nil
}

// prefixWords may precede an assignment that still sets the variable for
// the command (or the shell): `export X=…`, `env X=… claude`, `then X=…`.
var prefixWords = map[string]bool{
	"export": true, "declare": true, "typeset": true, "local": true, "readonly": true,
	"env": true, "command": true, "exec": true, "nohup": true, "sudo": true, "time": true,
	"if": true, "elif": true, "then": true, "else": true, "do": true, "while": true, "until": true,
	"{": true, "!": true,
}

// lineAssignments finds real CLAUDE_CONFIG_DIR assignments in one line of
// shell: an unquoted assignment word in command position (start of a
// command, after `;`, `&&`, `|`, other assignments or a prefix word such as
// export/env), or the same inside an alias body. Text inside quotes, such as
// `echo "CLAUDE_CONFIG_DIR=/tmp"`, or a plain argument never counts.
func lineAssignments(line, home string, depth int) []string {
	var out []string
	cmdStart := true // the next word may still be an assignment
	afterPrefix := false
	aliasArgs := false // inside `alias name=body ...`
	for _, tk := range lexLine(line) {
		if tk.op {
			cmdStart, afterPrefix, aliasArgs = true, false, false
			continue
		}
		w := tk.text
		switch {
		case aliasArgs:
			def := unquote(w)
			if i := strings.IndexByte(def, '='); i > 0 && depth < 2 {
				out = append(out, lineAssignments(def[i+1:], home, depth+1)...)
			}
		case !cmdStart:
		case strings.HasPrefix(w, assignPrefix):
			if d, ok := expandWord(w[len(assignPrefix):], home); ok {
				out = append(out, d)
			}
			afterPrefix = false
		case assignWordRe.MatchString(w):
			afterPrefix = false
		case w == "alias":
			aliasArgs, cmdStart = true, false
		case prefixWords[w]:
			afterPrefix = true
		case afterPrefix && strings.HasPrefix(w, "-"): // export -x, env -i
		default:
			cmdStart = false
		}
	}
	return out
}

var assignWordRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)

type token struct {
	text string // raw word, quotes kept
	op   bool   // a control operator: ; & | ( )
}

// lexLine splits one line into raw shell words and control operators.
// Quotes, backslash escapes, $(…) and backticks stay inside their word.
func lexLine(s string) []token {
	var toks []token
	var b strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			toks = append(toks, token{text: b.String()})
			b.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			flush()
		case c == ';' || c == '&' || c == '|' || c == '(' || c == ')':
			flush()
			toks = append(toks, token{text: string(c), op: true})
		case c == '\\':
			end := min(i+2, len(s))
			b.WriteString(s[i:end])
			i = end - 1
			inWord = true
		case c == '\'' || c == '`':
			end := len(s)
			if j := strings.IndexByte(s[i+1:], c); j >= 0 {
				end = i + 1 + j + 1
			}
			b.WriteString(s[i:end])
			i = end - 1
			inWord = true
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			end := min(j+1, len(s))
			b.WriteString(s[i:end])
			i = end - 1
			inWord = true
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			depth, j := 0, i+1
			for ; j < len(s); j++ {
				if s[j] == '(' {
					depth++
				} else if s[j] == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
			end := min(j+1, len(s))
			b.WriteString(s[i:end])
			i = end - 1
			inWord = true
		default:
			b.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return toks
}

// unquote removes one level of sh quoting without expanding anything; it
// turns an alias definition word into the text the alias runs.
func unquote(w string) string {
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		switch c := w[i]; c {
		case '\\':
			if i+1 < len(w) {
				i++
				b.WriteByte(w[i])
			}
		case '\'':
			j := strings.IndexByte(w[i+1:], '\'')
			if j < 0 {
				b.WriteString(w[i+1:])
				return b.String()
			}
			b.WriteString(w[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; i < len(w) && w[i] != '"'; i++ {
				if w[i] == '\\' && i+1 < len(w) && strings.IndexByte("$`\"\\", w[i+1]) >= 0 {
					i++
				}
				b.WriteByte(w[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// expandWord evaluates the value part of an assignment word the way the
// shell would, joining adjacent quoted and unquoted parts
// ("$HOME"/.claude-x, $HOME/"my dir"). Only $HOME and ${HOME} are expanded
// (outside single quotes); any other expansion makes the value unknowable
// and it is rejected. A leading ~ is expanded even when quoted: the shell
// would keep it literal, but the user clearly means the home dir and only
// existing dirs become candidates. The result must be absolute and must not
// be /, $HOME or a parent of $HOME: registering those would let julienning
// write settings.json into, and uninstall --purge reason about, the wrong
// place.
func expandWord(raw, home string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case '\\':
			if i+1 < len(raw) {
				i++
				b.WriteByte(raw[i])
			}
		case '\'':
			j := strings.IndexByte(raw[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			b.WriteString(raw[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; i < len(raw) && raw[i] != '"'; i++ {
				switch raw[i] {
				case '\\':
					if i+1 < len(raw) && strings.IndexByte("$`\"\\", raw[i+1]) >= 0 {
						i++
					}
					b.WriteByte(raw[i])
				case '$':
					n, ok := homeRef(raw[i:])
					if !ok {
						return "", false
					}
					b.WriteString(home)
					i += n - 1
				case '`':
					return "", false
				default:
					b.WriteByte(raw[i])
				}
			}
			if i >= len(raw) {
				return "", false // unterminated
			}
		case '$':
			n, ok := homeRef(raw[i:])
			if !ok {
				return "", false
			}
			b.WriteString(home)
			i += n - 1
		case '`':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	v := b.String()
	if v == "~" || strings.HasPrefix(v, "~/") {
		v = home + v[1:]
	}
	if v == "" || !filepath.IsAbs(v) {
		return "", false
	}
	v = filepath.Clean(v)
	if isHomeOrAncestor(v, home) {
		return "", false
	}
	return v, true
}

// homeRef returns the length of a $HOME or ${HOME} reference at the start
// of s (which starts with '$').
func homeRef(s string) (int, bool) {
	if strings.HasPrefix(s, "${HOME}") {
		return len("${HOME}"), true
	}
	if strings.HasPrefix(s, "$HOME") && (len(s) == 5 || !isNameChar(s[5])) {
		return len("$HOME"), true
	}
	return 0, false
}

func isNameChar(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// IsUnsafeConfigDir reports whether dir must never be treated as a Claude
// config dir: /, $HOME, or any parent of $HOME. Registering one would put a
// settings.json in the home dir and point CLAUDE_CONFIG_DIR at it.
func IsUnsafeConfigDir(dir, home string) bool { return isHomeOrAncestor(dir, home) }

// isHomeOrAncestor reports whether p is /, home, or a directory containing
// home, comparing both the paths as written and with symlinks resolved.
func isHomeOrAncestor(p, home string) bool {
	check := func(p, home string) bool {
		p, home = filepath.Clean(p), filepath.Clean(home)
		sep := string(filepath.Separator)
		return p == sep || p == home || strings.HasPrefix(home, p+sep)
	}
	if check(p, home) {
		return true
	}
	rp, err1 := filepath.EvalSymlinks(p)
	rh, err2 := filepath.EvalSymlinks(home)
	return err1 == nil && err2 == nil && check(rp, rh)
}

// stripComment drops a trailing sh comment: an unquoted # at the start of a
// word.
func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0:
			if ch == '\\' && quote == '"' {
				i++
			} else if ch == quote {
				quote = 0
			}
		case ch == '\\':
			i++
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// expand resolves ~, $HOME and ${HOME}; values that still reference other
// variables, or are not absolute, are rejected.
func expand(v, home string) (string, bool) {
	v = strings.TrimSpace(v)
	switch {
	case v == "~":
		v = home
	case strings.HasPrefix(v, "~/"):
		v = home + v[1:]
	}
	v = strings.ReplaceAll(v, "${HOME}", home)
	v = strings.ReplaceAll(v, "$HOME", home)
	if v == "" || strings.ContainsAny(v, "$`") || !filepath.IsAbs(v) {
		return "", false
	}
	return filepath.Clean(v), true
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// AliasHit is a hand-written rc alias that points CLAUDE_CONFIG_DIR at a dir.
type AliasHit struct {
	Line  int    // 1-based
	Alias string // alias name as written
	Dir   string // cleaned absolute config dir it selects
}

var aliasLineRe = regexp.MustCompile(`^\s*alias\s+(?:-[a-zA-Z]+\s+)*([A-Za-z0-9_.-]+)=`)

// RCAliases returns the `alias NAME=…CLAUDE_CONFIG_DIR=<dir>…` lines of an rc
// file whose dir matches one of dirs (cleaned, symlinks not resolved). Setup
// uses it to tell users which hand-written aliases julienning now replaces,
// whatever those aliases are called.
func RCAliases(path, home string, dirs []string) ([]AliasHit, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	want := map[string]bool{}
	for _, d := range dirs {
		want[filepath.Clean(d)] = true
	}
	var out []AliasHit
	for i, line := range strings.Split(string(raw), "\n") {
		m := aliasLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, d := range lineAssignments(stripComment(line), home, 0) {
			if want[filepath.Clean(d)] {
				out = append(out, AliasHit{Line: i + 1, Alias: m[1], Dir: filepath.Clean(d)})
				break
			}
		}
	}
	return out, nil
}
