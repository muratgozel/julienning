package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testDefault = "/Users/murat/.claude"

func TestInitShape(t *testing.T) {
	got, err := Init(Zsh, []string{"beta", "alpha", "beta"}, testDefault+"/")
	if err != nil {
		t.Fatal(err)
	}
	fn := func(n string) string {
		return "function claude-" + n + " { local _jl_dir; _jl_dir=$(command julienning resolve-dir '" + n + "') || return 1; " +
			`if [ "$_jl_dir" = '/Users/murat/.claude' ]; then env -u CLAUDE_CONFIG_DIR claude "$@"; else CLAUDE_CONFIG_DIR="$_jl_dir" command claude "$@"; fi; }` + "\n"
	}
	want := wrapper(testDefault) + fn("alpha") + fn("beta")
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(got, `[ "$_jl_dir" != '/Users/murat/.claude' ]`) {
		t.Fatalf("wrapper lacks the default-dir comparison:\n%s", got)
	}
	if strings.Contains(got, "alias ") {
		t.Fatalf("dir-name aliases are gone; got:\n%s", got)
	}
}

func TestInitNoNicknames(t *testing.T) {
	got, err := Init(Bash, nil, testDefault)
	if err != nil {
		t.Fatal(err)
	}
	if got != wrapper(testDefault) {
		t.Fatalf("got %q", got)
	}
}

func TestInitWithoutDefaultDir(t *testing.T) {
	got, err := Init(Zsh, []string{"alpha"}, "")
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
	got, err := Init(Zsh, []string{"a"}, `/tmp/it's $HOME/.claude`)
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
	if _, err := Init("fish", nil, testDefault); err == nil {
		t.Error("want error for fish")
	}
	for _, n := range []string{"a; rm -rf /", "A", "", "$(x)"} {
		if _, err := Init(Zsh, []string{"ok", n}, testDefault); err == nil {
			t.Errorf("%q: want error", n)
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
			code, err := Init(sh, []string{"a", "b.c", "d_e-f", "9"}, `/tmp/it's home/.claude`)
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
	f.write(filepath.Join(f.binDir, "julienning"), `#!/bin/sh
[ "$1" = resolve-dir ] || { echo "fake julienning: unexpected $*" >&2; exit 2; }
if [ -f "`+f.mapDir+`/$2" ]; then cat "`+f.mapDir+`/$2"; exit 0; fi
echo "julienning: unknown target \"$2\"" >&2
exit 1
`, 0o755)
	if defaultDir == "" {
		defaultDir = filepath.Join(root, ".claude")
	}
	code, err := Init(sh, nicks, defaultDir)
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
