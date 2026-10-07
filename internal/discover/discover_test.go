package discover

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
)

type env struct {
	t    *testing.T
	home string
}

func setup(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("JULIENNING_HOME", filepath.Join(root, "jl"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return &env{t: t, home: home}
}

func (e *env) mkdir(rel string) string {
	e.t.Helper()
	p := filepath.Join(e.home, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) write(rel, body string) {
	e.t.Helper()
	p := filepath.Join(e.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) login(rel, email string) {
	e.t.Helper()
	e.write(rel, `{"oauthAccount":{"emailAddress":"`+email+`"}}`)
}

// summary renders "<dir relative to $HOME> <name> <source> <email|ERR|-> [R]".
func summary(cs []Candidate) []string {
	home, _ := os.UserHomeDir()
	var out []string
	for _, c := range cs {
		dir := c.Dir
		if rel, err := filepath.Rel(home, c.Dir); err == nil && !strings.HasPrefix(rel, "..") {
			dir = rel
		}
		s := dir + " " + c.Name + " " + c.Source
		switch {
		case c.LoggedIn:
			s += " " + c.Email
		case c.Err != nil:
			s += " ERR"
		default:
			s += " -"
		}
		if c.Registered {
			s += " R"
		}
		out = append(out, s)
	}
	return out
}

func TestFindAllSources(t *testing.T) {
	e := setup(t)
	e.mkdir(".claude")
	e.login(".claude.json", "Personal@Example.com") // default dir's account file
	e.mkdir(".claude-team1")
	e.login(".claude-team1/.claude.json", "claude1@team.io")
	e.mkdir("work/projects")
	e.write("work/history.jsonl", "")
	e.mkdir("halfway/projects") // projects/ alone is not enough
	e.mkdir(".julienning")
	e.login(".julienning/.claude.json", "never@x.io")
	e.mkdir(".claude-bad")
	e.write(".claude-bad/.claude.json", "{nope")
	e.mkdir("elsewhere/one")
	e.mkdir("elsewhere/two")
	e.mkdir("elsewhere/three")
	e.mkdir("elsewhere/four")
	e.write(".zshrc", strings.Join([]string{
		`export CLAUDE_CONFIG_DIR="$HOME/elsewhere/one"`,
		`alias c2='CLAUDE_CONFIG_DIR=~/elsewhere/two claude'`,
		`# export CLAUDE_CONFIG_DIR=~/elsewhere/three`,
		`echo hi # CLAUDE_CONFIG_DIR=~/elsewhere/three`,
		`export CLAUDE_CONFIG_DIR=~/elsewhere/missing`,
		`export MY_CLAUDE_CONFIG_DIR=~/elsewhere/three`,
		`export CLAUDE_CONFIG_DIR=$OTHER/x`,
	}, "\n"))
	e.write(".bashrc", `alias c4="CLAUDE_CONFIG_DIR=\"${HOME}/elsewhere/four\" claude"`+"\n")
	e.mkdir("envdir")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(e.home, "envdir"))

	got, err := Find(nil, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	// Default dir first, then by path. .claude-team1 reuses its number; the
	// rest take the smallest free numbers in path order.
	want := []string{
		".claude default default personal@example.com",
		".claude-bad julienning2 home ERR",
		".claude-team1 julienning1 home claude1@team.io",
		"elsewhere/four julienning3 ~/.bashrc -",
		"elsewhere/one julienning4 ~/.zshrc -",
		"elsewhere/two julienning5 ~/.zshrc -",
		"envdir julienning6 env -",
		"work julienning7 home -",
	}
	if !reflect.DeepEqual(summary(got), want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(summary(got), "\n"), strings.Join(want, "\n"))
	}
	for _, c := range got {
		if !filepath.IsAbs(c.Dir) {
			t.Errorf("%s: dir %q not absolute", c.Name, c.Dir)
		}
	}
}

func TestFindDedupesAndUsesRegisteredNames(t *testing.T) {
	e := setup(t)
	a := e.mkdir(".claude-a")
	e.login(".claude-a/.claude.json", "a@x.io")
	e.mkdir("a")
	e.write("a/.claude.json", "{}")
	e.mkdir(".a")
	e.write(".a/.claude.json", "{}")
	e.write(".zshrc", "export CLAUDE_CONFIG_DIR=~/.claude-a/\n") // same dir, trailing slash
	t.Setenv("CLAUDE_CONFIG_DIR", a)

	got, err := Find([]config.ConfigDir{{Name: "mine", Dir: a}, {Name: "julienning1", Dir: "/elsewhere/a"}}, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".a julienning2 home -", ".claude-a mine home a@x.io R", "a julienning3 home -"}
	if !reflect.DeepEqual(summary(got), want) {
		t.Fatalf("got %q, want %q", summary(got), want)
	}
}

func TestFindWithoutDefaultDir(t *testing.T) {
	e := setup(t)
	e.login(".claude.json", "p@x.io") // file only, no ~/.claude dir
	got, err := Find(nil, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v", summary(got))
	}
}

func TestFindFollowsSymlinkedDirs(t *testing.T) {
	e := setup(t)
	target := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(filepath.Join(target, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(e.home, ".claude-linked")); err != nil {
		t.Fatal(err)
	}
	got, err := Find(nil, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary(got), []string{".claude-linked julienning1 home -"}) {
		t.Fatalf("got %v", summary(got))
	}
}

func TestFindSkipsPrivateMacFolders(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	e := setup(t)
	e.mkdir("Documents")
	e.write("Documents/.claude.json", "{}")
	got, err := Find(nil, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("scanned a TCC-protected folder: %v", summary(got))
	}
}

func TestFindUnreadableRCIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	e := setup(t)
	e.write(".zshrc", "x")
	if err := os.Chmod(filepath.Join(e.home, ".zshrc"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Find(nil, config.DefaultNamePrefix); err == nil || !strings.Contains(err.Error(), ".zshrc") {
		t.Fatalf("got %v", err)
	}
}

// The team's dirs are often named after the company; only the trailing
// number may carry over into the name (and so the alias).
func TestFindReusesBasenameNumbers(t *testing.T) {
	e := setup(t)
	e.mkdir(".claude")
	for _, d := range []string{".claude-acme", ".claude-acme1", ".claude-acme2"} {
		e.mkdir(d)
		e.write(d+"/.claude.json", "{}")
	}

	got, err := Find(nil, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".claude default default -",
		".claude-acme julienning3 home -",
		".claude-acme1 julienning1 home -",
		".claude-acme2 julienning2 home -",
	}
	if !reflect.DeepEqual(summary(got), want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(summary(got), "\n"), strings.Join(want, "\n"))
	}

	// A number already registered elsewhere is not reused: the smallest free
	// one is taken instead, in path order after the dirs that keep theirs.
	got, err = Find([]config.ConfigDir{{Name: "julienning2", Dir: "/elsewhere/x"}}, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		".claude default default -",
		".claude-acme julienning3 home -",
		".claude-acme1 julienning1 home -",
		".claude-acme2 julienning4 home -",
	}
	if !reflect.DeepEqual(summary(got), want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(summary(got), "\n"), strings.Join(want, "\n"))
	}
}

func TestFindCustomPrefixAndTakenDefault(t *testing.T) {
	e := setup(t)
	e.mkdir(".claude")
	e.mkdir(".claude-acme7")
	e.write(".claude-acme7/.claude.json", "{}")

	// "default" belongs to another dir, so ~/.claude is numbered like any other.
	got, err := Find([]config.ConfigDir{{Name: "default", Dir: "/elsewhere/d"}}, "team")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".claude team1 default -", ".claude-acme7 team7 home -"}
	if !reflect.DeepEqual(summary(got), want) {
		t.Fatalf("got %q, want %q", summary(got), want)
	}
}

func TestFindRejectsInvalidPrefix(t *testing.T) {
	setup(t)
	for _, p := range []string{"", "abc1", "-x", "a b"} {
		if _, err := Find(nil, p); err == nil || !strings.Contains(err.Error(), "invalid name prefix") {
			t.Errorf("prefix %q: got %v", p, err)
		}
	}
}

// Setup names the dirs it is about to register first, so a personal dir that
// is only listed never pushes a team dir to a higher number.
func TestAssignNamesPreferred(t *testing.T) {
	setup(t)
	cands := []Candidate{
		{Dir: "/x/.claude-me"},
		{Dir: "/x/.claude-p1"},
		{Dir: "/x/.claude-team"},
		{Dir: "/x/.claude-reg"},
	}
	registered := []config.ConfigDir{{Name: "work", Dir: "/x/.claude-reg"}}
	AssignNames(cands, registered, "j", func(c Candidate) bool { return c.Dir == "/x/.claude-team" })
	got := map[string]string{}
	for _, c := range cands {
		got[filepath.Base(c.Dir)] = c.Name
		if c.Registered != (c.Dir == "/x/.claude-reg") {
			t.Errorf("%s: Registered = %v", c.Dir, c.Registered)
		}
	}
	want := map[string]string{".claude-me": "j2", ".claude-p1": "j3", ".claude-team": "j1", ".claude-reg": "work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Without a preference .claude-p1 keeps its number and the rest fill in.
	AssignNames(cands, registered, "j", nil)
	got = map[string]string{}
	for _, c := range cands {
		got[filepath.Base(c.Dir)] = c.Name
	}
	want = map[string]string{".claude-me": "j2", ".claude-p1": "j1", ".claude-team": "j3", ".claude-reg": "work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAutoName(t *testing.T) {
	e := setup(t)
	taken := map[string]bool{"julienning1": true, "julienning3": true}
	isTaken := func(n string) bool { return taken[n] }
	cases := map[string]string{
		filepath.Join(e.home, ".claude"): "default",
		"/x/.claude-acme2":               "julienning2",
		"/x/.claude-acme3":               "julienning2", // 3 is taken
		"/x/.claude-acme":                "julienning2",
		"/x/.claude-acme0":               "julienning2", // 0 is never reused
		"/x/backup-007":                  "julienning7",
		"/x/acme-12/":                    "julienning12",
		"/x/.claude":                     "julienning2", // not the default dir
		"/x/" + strings.Repeat("9", 30):  "julienning2", // overflows int
	}
	for dir, want := range cases {
		got := AutoName(dir, config.DefaultNamePrefix, isTaken)
		if got != want {
			t.Errorf("AutoName(%q) = %q, want %q", dir, got, want)
		}
		if !config.ValidConfigName(got) {
			t.Errorf("AutoName(%q) = %q is not a valid config name", dir, got)
		}
	}
	taken["default"] = true
	if got := AutoName(filepath.Join(e.home, ".claude"), "team", isTaken); got != "team1" {
		t.Errorf("default dir with \"default\" taken: got %q", got)
	}
}

func TestNextFreeName(t *testing.T) {
	taken := map[string]bool{"a1": true, "a2": true, "a4": true}
	if got := NextFreeName("a", func(s string) bool { return taken[s] }); got != "a3" {
		t.Fatalf("got %q", got)
	}
}

func TestExpand(t *testing.T) {
	cases := map[string]string{
		"~":               "/h",
		"~/x":             "/h/x",
		"$HOME/x":         "/h/x",
		"${HOME}/x/":      "/h/x",
		"/abs/./y":        "/abs/y",
		"relative/x":      "",
		"$OTHER/x":        "",
		"~other/x":        "",
		"/a/$(whoami)/x":  "",
		"/a/`whoami`/x":   "",
		"  /spaced/x   ":  "/spaced/x",
		"${HOME}${HOME}x": "/h/hx",
	}
	for in, want := range cases {
		got, ok := expand(in, "/h")
		if (want == "") != !ok || got != want {
			t.Errorf("expand(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestStripComment(t *testing.T) {
	cases := map[string]string{
		`a # b`:        `a `,
		`a#b`:          `a#b`,
		`"a # b" # c`:  `"a # b" `,
		`'a # b'`:      `'a # b'`,
		`# all`:        ``,
		`x=\# y # z`:   `x=\# y `,
		`"q\"# x" # y`: `"q\"# x" `,
	}
	for in, want := range cases {
		if got := stripComment(in); got != want {
			t.Errorf("stripComment(%q) = %q, want %q", in, got, want)
		}
	}
}

// rc values are whole shell words: quoted and unquoted parts join, only real
// assignments count, and /, $HOME and its parents are never candidates.
func TestRCAssignmentsParsesShellWords(t *testing.T) {
	e := setup(t)
	h := e.home
	parent := filepath.Dir(h)
	cases := []struct {
		line string
		want []string
	}{
		{`export CLAUDE_CONFIG_DIR="$HOME"/.claude-work`, []string{h + "/.claude-work"}},
		{`export CLAUDE_CONFIG_DIR=$HOME/"my dir"`, []string{h + "/my dir"}},
		{`export CLAUDE_CONFIG_DIR='~/x'`, []string{h + "/x"}},
		{`export CLAUDE_CONFIG_DIR=${HOME}/y`, []string{h + "/y"}},
		{`export CLAUDE_CONFIG_DIR=~/a\ b`, []string{h + "/a b"}},
		{`export CLAUDE_CONFIG_DIR="${HOME}/q"'/r'`, []string{h + "/q/r"}},
		{`  CLAUDE_CONFIG_DIR=/opt/c claude`, []string{"/opt/c"}},
		{`FOO=1 CLAUDE_CONFIG_DIR=~/d claude`, []string{h + "/d"}},
		{`true && CLAUDE_CONFIG_DIR=~/e claude`, []string{h + "/e"}},
		{`x=1;export CLAUDE_CONFIG_DIR=~/f`, []string{h + "/f"}},
		{`env -i CLAUDE_CONFIG_DIR=~/g claude`, []string{h + "/g"}},
		{`declare -x CLAUDE_CONFIG_DIR=~/g2`, []string{h + "/g2"}},
		{`if [ -d ~/h ]; then export CLAUDE_CONFIG_DIR=~/h; fi`, []string{h + "/h"}},
		{`work() { CLAUDE_CONFIG_DIR=~/i claude "$@"; }`, []string{h + "/i"}},
		{`alias w='CLAUDE_CONFIG_DIR="$HOME"/j claude'`, []string{h + "/j"}},
		{`alias w="CLAUDE_CONFIG_DIR=\"$HOME/k l\" claude"`, []string{h + "/k l"}},
		{`alias a='CLAUDE_CONFIG_DIR=~/m claude' b='CLAUDE_CONFIG_DIR=~/n claude'`, []string{h + "/m", h + "/n"}},
		{`export CLAUDE_CONFIG_DIR=~/o # CLAUDE_CONFIG_DIR=~/p`, []string{h + "/o"}},
		// not assignments
		{`echo "CLAUDE_CONFIG_DIR=/tmp"`, nil},
		{`echo CLAUDE_CONFIG_DIR=/tmp`, nil},
		{`echo 'export CLAUDE_CONFIG_DIR=/tmp'`, nil},
		{`printf '%s\n' "$(echo CLAUDE_CONFIG_DIR=/tmp)"`, nil},
		{`MY_CLAUDE_CONFIG_DIR=/opt/x`, nil},
		{`unset CLAUDE_CONFIG_DIR`, nil},
		// unknowable or dangerous values
		{`export CLAUDE_CONFIG_DIR="$HOME"`, nil},
		{`export CLAUDE_CONFIG_DIR=$HOME/`, nil},
		{`export CLAUDE_CONFIG_DIR=~`, nil},
		{`export CLAUDE_CONFIG_DIR=/`, nil},
		{`export CLAUDE_CONFIG_DIR=` + parent, nil},
		{`export CLAUDE_CONFIG_DIR="$HOME"/..`, nil},
		{`export CLAUDE_CONFIG_DIR=$OTHER/x`, nil},
		{`export CLAUDE_CONFIG_DIR=$HOMEDIR/x`, nil},
		{`export CLAUDE_CONFIG_DIR="$(dirname "$HOME")/x"`, nil},
		{"export CLAUDE_CONFIG_DIR=`pwd`/x", nil},
		{`export CLAUDE_CONFIG_DIR="$HOME/unterminated`, nil},
		{`export CLAUDE_CONFIG_DIR=relative/x`, nil},
		{`export CLAUDE_CONFIG_DIR=~other/x`, nil},
		{`export CLAUDE_CONFIG_DIR=`, nil},
	}
	rc := filepath.Join(h, ".zshrc")
	for _, tc := range cases {
		if err := os.WriteFile(rc, []byte(tc.line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := rcAssignments(rc, h)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s\n got %q\nwant %q", tc.line, got, tc.want)
		}
	}
}

// Find ignores an rc value that resolves to $HOME even though it exists.
func TestFindRejectsHomeFromRC(t *testing.T) {
	e := setup(t)
	e.write(".zshrc", "export CLAUDE_CONFIG_DIR=\"$HOME\"/.claude-work\nexport CLAUDE_CONFIG_DIR=\"$HOME\"\n")
	e.mkdir(".claude-work")
	got, err := Find(nil, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary(got), []string{".claude-work julienning1 ~/.zshrc -"}) {
		t.Fatalf("got %v", summary(got))
	}
}

func TestFindRejectsHomeFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@x.io"}}`), 0o600)
	got, err := Find(nil, config.DefaultNamePrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if filepath.Clean(c.Dir) == filepath.Clean(home) {
			t.Fatalf("home dir must not be a candidate: %+v", c)
		}
	}
}

func TestRCAliasesMatchesRegisteredDirsByPath(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, ".zshrc")
	os.WriteFile(rc, []byte(strings.Join([]string{
		"# comment alias claude-x='CLAUDE_CONFIG_DIR=~/.claude-a claude'",
		"alias claude-sixtynine1='CLAUDE_CONFIG_DIR=~/.claude-a claude'",
		"alias other=\"CLAUDE_CONFIG_DIR=$HOME/.claude-b claude\"",
		"alias unrelated='CLAUDE_CONFIG_DIR=~/.claude-zzz claude'",
		"export CLAUDE_CONFIG_DIR=~/.claude-a",
	}, "\n")), 0o644)
	got, err := RCAliases(rc, home, []string{filepath.Join(home, ".claude-a"), filepath.Join(home, ".claude-b")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Line != 2 || got[0].Alias != "claude-sixtynine1" || got[1].Line != 3 || got[1].Alias != "other" {
		t.Fatalf("got %+v", got)
	}
	if hits, err := RCAliases(filepath.Join(home, "missing"), home, nil); err != nil || hits != nil {
		t.Fatalf("missing file: %v %v", hits, err)
	}
}
