package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/ptytest"
)

const testDefault = "/Users/murat/.claude"

func TestInitShape(t *testing.T) {
	got, err := Init(Zsh, Options{Nicknames: []string{"beta", "alpha", "beta"}, DefaultDir: testDefault + "/"})
	if err != nil {
		t.Fatal(err)
	}
	fn := func(n string) string {
		return "function claude-" + n + " { local _jl_dir; _jl_dir=$(command julienning resolve-dir '" + n + "') || return 1; " +
			`if [ "$_jl_dir" = '/Users/murat/.claude' ]; then env -u CLAUDE_CONFIG_DIR claude "$@"; else CLAUDE_CONFIG_DIR="$_jl_dir" command claude "$@"; fi; }` + "\n"
	}
	want := wrapper(Options{DefaultDir: testDefault}) + fn("alpha") + fn("beta")
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(got, `[ "$_jl_dir" != '/Users/murat/.claude' ]`) {
		t.Fatalf("wrapper lacks the default-dir comparison:\n%s", got)
	}
	if strings.Contains(got, "alias ") {
		t.Fatalf("dir-name aliases are gone; got:\n%s", got)
	}
	if strings.Contains(got, "_julienning_claude") || strings.Contains(got, "tmux") {
		t.Fatalf("tmux code without tmux mode:\n%s", got)
	}
}

// With a stable install the functions call julienning by its path, so they
// work before a startup file has put ~/.local/bin on PATH.
func TestInitCallsJulienningByPath(t *testing.T) {
	got, err := Init(Bash, Options{Nicknames: []string{"alpha"}, DefaultDir: testDefault, Julienning: "/home/dev/.local/bin/julienning"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`_jl_dir=$('/home/dev/.local/bin/julienning' resolve-dir --pin) || return 1`,
		`_jl_dir=$('/home/dev/.local/bin/julienning' resolve-dir 'alpha') || return 1;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "command julienning") {
		t.Errorf("still looks julienning up on PATH:\n%s", got)
	}
	got, err = Init(Bash, Options{Julienning: `/opt/it's/julienning`})
	if err != nil {
		t.Fatal(err)
	}
	if want := `_jl_dir=$('/opt/it'\''s/julienning' resolve-dir --pin)`; !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

func TestInitTmuxShape(t *testing.T) {
	o := Options{Nicknames: []string{"alpha"}, DefaultDir: testDefault, Tmux: &Tmux{Path: "/usr/bin/tmux", Session: "claude"}}
	for _, sh := range []string{Bash, Zsh} {
		got, err := Init(sh, o)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			`if [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then _julienning_claude "$CLAUDE_CONFIG_DIR" "$@"; return; fi`,
			`    _julienning_claude "$_jl_dir" "$@"` + "\n  else\n    _julienning_claude '' \"$@\"\n",
			`if [ "$_jl_dir" = '/Users/murat/.claude' ]; then _julienning_claude '' "$@"; else _julienning_claude "$_jl_dir" "$@"; fi; }`,
			`[ -z "${TMUX:-}" ] && [ "${TERM_PROGRAM:-}" != vscode ] && [ -x '/usr/bin/tmux' ]`,
			`'/usr/bin/tmux' has-session -t '=claude' 2>/dev/null`,
			`'/usr/bin/tmux' new-window -t '=claude:' -c "$PWD" "$@" && '/usr/bin/tmux' attach-session -t '=claude'`,
			`env -u CLAUDE_CONFIG_DIR '/usr/bin/tmux' new-session -s 'claude' -c "$PWD" "$@"`,
			`set -- /bin/sh -c 'trap : INT; "$@"; exec "${SHELL:-/bin/sh}"' julienning env "$@"`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in:\n%s", sh, want, got)
			}
		}
		find := map[string]string{Bash: "_jl_bin=$(type -P claude)", Zsh: "_jl_bin=$(whence -p claude)"}[sh]
		if !strings.Contains(got, find) {
			t.Errorf("%s: claude's path must come from a PATH search (%s):\n%s", sh, find, got)
		}
		if strings.Contains(got, "command -v claude") {
			t.Errorf("%s: command -v names the claude function itself:\n%s", sh, got)
		}
	}
}

func TestInitNoNicknames(t *testing.T) {
	got, err := Init(Bash, Options{DefaultDir: testDefault})
	if err != nil {
		t.Fatal(err)
	}
	if got != wrapper(Options{DefaultDir: testDefault}) {
		t.Fatalf("got %q", got)
	}
}

func TestInitWithoutDefaultDir(t *testing.T) {
	got, err := Init(Zsh, Options{Nicknames: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "!=") || strings.Contains(got, "env -u") {
		t.Fatalf("default-dir handling emitted without a default dir:\n%s", got)
	}
	contains := `_jl_dir=$(command julienning resolve-dir 'alpha') || return 1; CLAUDE_CONFIG_DIR="$_jl_dir" command claude "$@"; }`
	if !strings.Contains(got, contains) {
		t.Fatalf("got:\n%s", got)
	}
}

func TestInitQuotesDefaultDir(t *testing.T) {
	got, err := Init(Zsh, Options{Nicknames: []string{"a"}, DefaultDir: `/tmp/it's $HOME/.claude`})
	if err != nil {
		t.Fatal(err)
	}
	if want := `if [ "$_jl_dir" = '/tmp/it'\''s $HOME/.claude' ]; then`; !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

func TestValidNickname(t *testing.T) {
	for _, s := range []string{"a", "claude1", "a.b_c-d", "0x", strings.Repeat("a", 32)} {
		if !ValidNickname(s) {
			t.Errorf("%q: want valid", s)
		}
	}
	for _, s := range []string{"", "A", "-a", ".a", "_a", "a b", "a;b", "a$b", "a/b", "a'b", strings.Repeat("a", 33), "ä"} {
		if ValidNickname(s) {
			t.Errorf("%q: want invalid", s)
		}
	}
}

func TestInitRejectsBadInput(t *testing.T) {
	if _, err := Init("fish", Options{DefaultDir: testDefault}); err == nil {
		t.Error("want error for fish")
	}
	for _, n := range []string{"a; rm -rf /", "A", "", "$(x)"} {
		if _, err := Init(Zsh, Options{Nicknames: []string{"ok", n}, DefaultDir: testDefault}); err == nil {
			t.Errorf("%q: want error", n)
		}
	}
	for _, o := range []Options{
		{Julienning: "bin/julienning"},
		{Tmux: &Tmux{Path: "tmux", Session: "claude"}},
		{Tmux: &Tmux{Path: "/usr/bin/tmux", Session: "a:b"}},
		{Tmux: &Tmux{Path: "/usr/bin/tmux", Session: "it's"}},
		{Tmux: &Tmux{Path: "/usr/bin/tmux", Session: ""}},
	} {
		if _, err := Init(Bash, o); err == nil {
			t.Errorf("%+v: want error", o)
		}
	}
}

// --- shell-level tests; skipped when the shell is not installed ---

func TestGeneratedCodeIsValid(t *testing.T) {
	for _, sh := range []string{Zsh, Bash} {
		t.Run(sh, func(t *testing.T) {
			bin, err := exec.LookPath(sh)
			if err != nil {
				t.Skipf("%s not installed", sh)
			}
			for _, o := range []Options{
				{Nicknames: []string{"a", "b.c", "d_e-f", "9"}, DefaultDir: `/tmp/it's home/.claude`},
				{Nicknames: []string{"a"}, DefaultDir: `/tmp/it's home/.claude`, Julienning: `/opt/it's/julienning`,
					Tmux: &Tmux{Path: `/opt/it's/tmux`, Session: "claude-x_1"}},
			} {
				code, err := Init(sh, o)
				if err != nil {
					t.Fatal(err)
				}
				script := filepath.Join(t.TempDir(), "init.sh")
				if err := os.WriteFile(script, []byte(code), 0o644); err != nil {
					t.Fatal(err)
				}
				if out, err := exec.Command(bin, "-n", script).CombinedOutput(); err != nil {
					t.Fatalf("%s -n failed: %v\n%s\ncode:\n%s", sh, err, out, code)
				}
			}
		})
	}
}

// shellFixture is a temp HOME with a fake `claude` (prints the config dir it
// got, or "unset", then its args) and a fake `julienning resolve-dir` that
// answers from map/<nick> files, so tests can move an account between calls.
type shellFixture struct {
	t                                    *testing.T
	sh, bin                              string
	root, binDir, jlHome, mapDir, initSh string
}

func newShellFixture(t *testing.T, sh string, nicks []string, defaultDir string) *shellFixture {
	return newShellFixtureWith(t, sh, Options{Nicknames: nicks, DefaultDir: defaultDir})
}

// newShellFixtureWith generates the code from o. A Julienning path of
// "fake" is replaced by the fake julienning's absolute path.
func newShellFixtureWith(t *testing.T, sh string, o Options) *shellFixture {
	t.Helper()
	bin, err := exec.LookPath(sh)
	if err != nil {
		t.Skipf("%s not installed", sh)
	}
	root := t.TempDir()
	f := &shellFixture{t: t, sh: sh, bin: bin, root: root,
		binDir: filepath.Join(root, "bin"), jlHome: filepath.Join(root, "jl"), mapDir: filepath.Join(root, "map")}
	for _, d := range []string{f.binDir, f.jlHome, f.mapDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.write(filepath.Join(f.binDir, "claude"), "#!/bin/sh\necho \"${CLAUDE_CONFIG_DIR-unset}${*:+ $*}\"\n", 0o755)
	// Every call is logged to map/calls; map/<arg>.fail makes resolve-dir
	// fail with that file's text, map/<arg> makes it print the file.
	f.write(filepath.Join(f.binDir, "julienning"), `#!/bin/sh
echo "$*" >> "`+f.mapDir+`/calls"
[ "$1" = resolve-dir ] || { echo "fake julienning: unexpected $*" >&2; exit 2; }
if [ -f "`+f.mapDir+`/$2.fail" ]; then cat "`+f.mapDir+`/$2.fail" >&2; exit 1; fi
if [ -f "`+f.mapDir+`/$2" ]; then cat "`+f.mapDir+`/$2"; exit 0; fi
echo "julienning: unknown target \"$2\"" >&2
exit 1
`, 0o755)
	if o.DefaultDir == "" {
		o.DefaultDir = filepath.Join(root, ".claude")
	}
	if o.Julienning == "fake" {
		o.Julienning = filepath.Join(f.binDir, "julienning")
	}
	code, err := Init(sh, o)
	if err != nil {
		t.Fatal(err)
	}
	f.initSh = filepath.Join(root, "init.sh")
	f.write(f.initSh, code, 0o644)
	return f
}

func (f *shellFixture) write(path, body string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		f.t.Fatal(err)
	}
}

// point makes `julienning resolve-dir nick` print dir.
func (f *shellFixture) point(nick, dir string) {
	f.t.Helper()
	f.write(filepath.Join(f.mapDir, nick), dir+"\n", 0o644)
}

// run evals the init code in a fresh shell, then runs body. Aliases are
// expanded only on lines parsed after their definition, and non-interactive
// bash needs expand_aliases, hence a script file. A non-zero exit of the
// script fails the test.
func (f *shellFixture) run(prelude, body string, extraEnv ...string) string {
	f.t.Helper()
	script := filepath.Join(f.root, "run.sh")
	f.write(script, "shopt -s expand_aliases 2>/dev/null\n"+prelude+"eval \"$(cat '"+f.initSh+"')\" || exit 3\n"+body+"\n", 0o644)
	cmd := exec.Command(f.bin, script)
	cmd.Env = append([]string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.root,
		"JULIENNING_HOME=" + f.jlHome,
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("%s: %v\n%s", f.sh, err, out)
	}
	return string(out)
}

func TestWrapperBehavior(t *testing.T) {
	for _, sh := range []string{Zsh, Bash} {
		t.Run(sh, func(t *testing.T) {
			f := newShellFixture(t, sh, nil, "")
			target := filepath.Join(f.root, ".claude-a")
			other := filepath.Join(f.root, ".claude-b")
			defaultDir := filepath.Join(f.root, ".claude")
			for _, d := range []string{target, other, defaultDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			run := func(env ...string) string { return strings.TrimSpace(f.run("", "claude", env...)) }
			currentFile := filepath.Join(f.jlHome, "current")

			if got := run(); got != "unset" {
				t.Errorf("no current: got %q, want unset", got)
			}
			// current selects the dir (trailing newline must be stripped).
			f.write(currentFile, target+"\n", 0o600)
			if got := run(); got != target {
				t.Errorf("current: got %q, want %q", got, target)
			}
			// An exported CLAUDE_CONFIG_DIR wins over current.
			if got := run("CLAUDE_CONFIG_DIR=" + other); got != other {
				t.Errorf("explicit env: got %q, want %q", got, other)
			}
			// The default dir as current runs claude with the variable unset.
			f.write(currentFile, defaultDir+"\n", 0o600)
			if got := run(); got != "unset" {
				t.Errorf("default current: got %q, want unset", got)
			}
			// A stale current pointing at a deleted dir falls back.
			f.write(currentFile, filepath.Join(f.root, "gone")+"\n", 0o600)
			if got := run(); got != "unset" {
				t.Errorf("stale current: got %q, want unset", got)
			}
		})
	}
}

// calls returns what the fake julienning was asked since the last call.
func (f *shellFixture) calls() string {
	f.t.Helper()
	p := filepath.Join(f.mapDir, "calls")
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	_ = os.Remove(p)
	return string(raw)
}

// Pins come before the selection; julienning is consulted only while
// pins.json exists; an exported CLAUDE_CONFIG_DIR still wins; and a pin that
// cannot be resolved stops claude instead of falling back to the selection.
func TestWrapperFollowsPins(t *testing.T) {
	for _, sh := range []string{Zsh, Bash} {
		t.Run(sh, func(t *testing.T) {
			f := newShellFixture(t, sh, nil, "")
			selected := filepath.Join(f.root, ".claude-a")
			pinned := filepath.Join(f.root, ".claude-p")
			other := filepath.Join(f.root, ".claude-b")
			defaultDir := filepath.Join(f.root, ".claude")
			for _, d := range []string{selected, pinned, other, defaultDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			f.write(filepath.Join(f.jlHome, "current"), selected+"\n", 0o600)
			run := func(env ...string) string { return strings.TrimSpace(f.run("", "claude x 'two words'", env...)) }

			if got := run(); got != selected+" x two words" || f.calls() != "" {
				t.Errorf("without pins.json: got %q (julienning must not run)", got)
			}
			f.write(filepath.Join(f.jlHome, "pins.json"), "{}", 0o600)
			f.point("--pin", "") // no pin covers the working directory
			if got := run(); got != selected+" x two words" || f.calls() != "resolve-dir --pin\n" {
				t.Errorf("no pin here: got %q", got)
			}
			f.point("--pin", pinned)
			if got := run(); got != pinned+" x two words" {
				t.Errorf("pinned: got %q", got)
			}
			f.point("--pin", defaultDir)
			if got := run(); got != "unset x two words" {
				t.Errorf("pinned to the default dir: got %q", got)
			}
			f.calls()
			if got := run("CLAUDE_CONFIG_DIR=" + other); got != other+" x two words" || f.calls() != "" {
				t.Errorf("exported variable: got %q", got)
			}
			if err := os.Remove(filepath.Join(f.jlHome, "current")); err != nil {
				t.Fatal(err)
			}
			f.point("--pin", pinned)
			if got := run(); got != pinned+" x two words" {
				t.Errorf("pinned without a selection: got %q", got)
			}
			f.write(filepath.Join(f.mapDir, "--pin.fail"), "julienning: ~/Code/side is pinned to me@x.io, which cannot be opened\n", 0o644)
			out := f.run("", `claude x; echo "rc=$?"`)
			if want := "julienning: ~/Code/side is pinned to me@x.io, which cannot be opened\nrc=1\n"; out != want {
				t.Errorf("broken pin:\n got %q\nwant %q", out, want)
			}
		})
	}
}

func TestNicknameFunctions(t *testing.T) {
	for _, sh := range []string{Zsh, Bash} {
		t.Run(sh, func(t *testing.T) {
			f := newShellFixture(t, sh, []string{"alpha", "b.eta", "ghost"}, "")
			dirA := filepath.Join(f.root, ".claude-julienning1")
			dirB := filepath.Join(f.root, ".claude-julienning2")
			defaultDir := filepath.Join(f.root, ".claude")

			// Resolved at call time: the account moves to another dir between
			// two calls in the same shell, and the second call follows it.
			// Arguments pass through untouched.
			f.point("alpha", dirA)
			out := f.run("", `claude-alpha bar 'two words'
echo "`+dirB+`" > '`+filepath.Join(f.mapDir, "alpha")+`'
claude-alpha`)
			if want := dirA + " bar two words\n" + dirB + "\n"; out != want {
				t.Errorf("call-time resolution:\n got %q\nwant %q", out, want)
			}

			// The default dir is launched with the variable unset, even when
			// one is inherited; other dirs override an inherited value.
			f.point("b.eta", defaultDir)
			if got := f.run("", "claude-b.eta x", "CLAUDE_CONFIG_DIR="+dirA); got != "unset x\n" {
				t.Errorf("default dir: got %q, want %q", got, "unset x\n")
			}
			f.point("alpha", dirB)
			if got := f.run("", "claude-alpha", "CLAUDE_CONFIG_DIR="+dirA); got != dirB+"\n" {
				t.Errorf("inherited variable: got %q, want %q", got, dirB+"\n")
			}

			// An unresolvable nickname returns 1 with resolve-dir's message
			// and never starts claude.
			out = f.run("", `claude-ghost --resume x; echo "rc=$?"`)
			if want := "julienning: unknown target \"ghost\"\nrc=1\n"; out != want {
				t.Errorf("unresolvable:\n got %q\nwant %q", out, want)
			}
		})
	}
}

// An `alias claude=...` defined before the eval (Claude's installer adds
// one) must neither break the eval nor bypass the wrapper, and the alias's
// flags must reach claude. Same-named hand-written aliases must not break
// the nickname functions' definitions either.
func TestWrapperSurvivesExistingAliases(t *testing.T) {
	for _, sh := range []string{Zsh, Bash} {
		t.Run(sh, func(t *testing.T) {
			f := newShellFixture(t, sh, []string{"alpha"}, "")
			target := filepath.Join(f.root, ".claude-a")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			f.write(filepath.Join(f.jlHome, "current"), target+"\n", 0o600)
			f.point("alpha", target)
			out := f.run("alias claude='claude --foo'\nalias claude-alpha='echo hand-written'\n",
				"claude bar 'two words'\nunalias claude-alpha\nclaude-alpha baz")
			want := target + " --foo bar two words\n" + target + " baz\n"
			if out != want {
				t.Fatalf("got:\n%s\nwant:\n%s", out, want)
			}
		})
	}
}

// tmuxFixture adds a fake tmux to a shell fixture: every call is logged to
// map/tmux one argument per bracket, with the CLAUDE_CONFIG_DIR tmux itself
// saw; has-session succeeds while map/session exists; new-session and
// new-window run the window command in the -c directory, with SHELL set to a
// fake shell that says it took over the window.
func tmuxFixture(t *testing.T, sh string, o Options) *shellFixture {
	t.Helper()
	f := newShellFixtureWith(t, sh, o)
	tmux := filepath.Join(f.binDir, "tmux")
	f.write(tmux, `#!/bin/sh
{ printf '[%s]' "$@"; echo " env=${CLAUDE_CONFIG_DIR-unset}"; } >> "`+f.mapDir+`/tmux"
case "$1" in
has-session) [ -f "`+f.mapDir+`/session" ]; exit ;;
new-session|new-window) dir=$5; shift 5; cd "$dir" && SHELL="`+f.binDir+`/winshell" exec "$@" ;;
attach-session) echo attached ;;
esac
`, 0o755)
	f.write(filepath.Join(f.binDir, "winshell"), "#!/bin/sh\necho 'window shell'\n", 0o755)
	// Regenerate with the fake tmux's path baked in.
	o.Tmux = &Tmux{Path: tmux, Session: "claude"}
	if o.DefaultDir == "" {
		o.DefaultDir = filepath.Join(f.root, ".claude")
	}
	if o.Julienning == "fake" {
		o.Julienning = filepath.Join(f.binDir, "julienning")
	}
	code, err := Init(sh, o)
	if err != nil {
		t.Fatal(err)
	}
	f.write(f.initSh, code, 0o644)
	return f
}

// runTTY is run with stdin and stdout on a pseudo-terminal, from dir.
func (f *shellFixture) runTTY(dir, body string, extraEnv ...string) string {
	f.t.Helper()
	script := filepath.Join(f.root, "run.sh")
	f.write(script, "eval \"$(cat '"+f.initSh+"')\" || exit 3\n"+body+"\n", 0o644)
	cmd := exec.Command(f.bin, script)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.root,
		"JULIENNING_HOME=" + f.jlHome,
		"TERM=dumb",
	}, extraEnv...)
	out, ok, err := ptytest.Run(cmd, 20*time.Second)
	if !ok {
		f.t.Skip("no pseudo-terminal available")
	}
	if err != nil {
		f.t.Fatalf("%s: %v\n%s", f.sh, err, out)
	}
	return out
}

func (f *shellFixture) tmuxCalls() string {
	f.t.Helper()
	p := filepath.Join(f.mapDir, "tmux")
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	_ = os.Remove(p)
	return string(raw)
}

func TestTmuxModeLaunchRule(t *testing.T) {
	for _, sh := range []string{Zsh, Bash} {
		t.Run(sh, func(t *testing.T) {
			f := tmuxFixture(t, sh, Options{Nicknames: []string{"alpha"}, Julienning: "fake"})
			target := filepath.Join(f.root, ".claude-a")
			other := filepath.Join(f.root, ".claude-b")
			defaultDir := filepath.Join(f.root, ".claude")
			work := filepath.Join(f.root, "work dir")
			for _, d := range []string{target, other, defaultDir, work} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			work, _ = filepath.EvalSymlinks(work)
			f.write(filepath.Join(f.jlHome, "current"), target+"\n", 0o600)
			claude := filepath.Join(f.binDir, "claude")
			window := func(acct ...string) string {
				w := "[/bin/sh][-c][" + WindowScript + "][julienning][env]"
				for _, a := range acct {
					w += "[" + a + "]"
				}
				return w + "[" + claude + "][x][two words]"
			}

			// No session yet: a new one, with claude as its first window;
			// the window's shell takes over when claude exits.
			out := f.runTTY(work, "claude x 'two words'")
			want := "[new-session][-s][claude][-c][" + work + "]" + window("CLAUDE_CONFIG_DIR="+target) + " env=unset\n"
			if got := f.tmuxCalls(); got != "[has-session][-t][=claude] env=unset\n"+want {
				t.Errorf("new session:\n got %q\nwant %q", got, want)
			}
			if !strings.Contains(out, target+" x two words\nwindow shell\n") {
				t.Errorf("new session output:\n%s", out)
			}

			// A session exists: a new window in it, then attach.
			f.write(filepath.Join(f.mapDir, "session"), "", 0o644)
			out = f.runTTY(work, "claude x 'two words'")
			want = "[has-session][-t][=claude] env=unset\n" +
				"[new-window][-t][=claude:][-c][" + work + "]" + window("CLAUDE_CONFIG_DIR="+target) + " env=unset\n" +
				"[attach-session][-t][=claude] env=unset\n"
			if got := f.tmuxCalls(); got != want {
				t.Errorf("existing session:\n got %q\nwant %q", got, want)
			}
			if !strings.Contains(out, "attached") {
				t.Errorf("not attached:\n%s", out)
			}
			_ = os.Remove(filepath.Join(f.mapDir, "session"))

			// An exported CLAUDE_CONFIG_DIR is the window's account, and a
			// new tmux server never inherits it.
			f.runTTY(work, "claude x 'two words'", "CLAUDE_CONFIG_DIR="+other)
			if got := f.tmuxCalls(); !strings.Contains(got, window("CLAUDE_CONFIG_DIR="+other)+" env=unset\n") {
				t.Errorf("exported variable:\n%s", got)
			}

			// The default dir is passed as an unset variable, explicitly.
			f.write(filepath.Join(f.jlHome, "current"), defaultDir+"\n", 0o600)
			f.runTTY(work, "claude x 'two words'")
			if got := f.tmuxCalls(); !strings.Contains(got, window("-u", "CLAUDE_CONFIG_DIR")) {
				t.Errorf("default dir:\n%s", got)
			}

			// claude-<nick> takes the same way, with its own dir.
			f.point("alpha", other)
			f.runTTY(work, "claude-alpha x 'two words'")
			if got := f.tmuxCalls(); !strings.Contains(got, window("CLAUDE_CONFIG_DIR="+other)) {
				t.Errorf("nickname function:\n%s", got)
			}

			// In place: inside tmux, from VS Code, or without a terminal.
			f.write(filepath.Join(f.jlHome, "current"), target+"\n", 0o600)
			for _, env := range []string{"TMUX=/tmp/tmux-1/default,1,0", "TERM_PROGRAM=vscode"} {
				out := f.runTTY(work, "claude x 'two words'", env)
				if got := f.tmuxCalls(); got != "" || !strings.Contains(out, target+" x two words") {
					t.Errorf("%s: tmux calls %q, output %q", env, got, out)
				}
			}
			if out := f.run("", "claude x 'two words'"); out != target+" x two words\n" || f.tmuxCalls() != "" {
				t.Errorf("no terminal: %q", out)
			}
		})
	}
}
