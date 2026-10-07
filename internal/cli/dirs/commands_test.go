package dirs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/remote"
)

// --- new-config ---

func TestNewConfigAutoNumbering(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false, config.ConfigDir{Name: "julienning1", Dir: h.mkdir(".claude-julienning1")})
	h.mkdir(".claude-julienning2") // exists on disk but is not registered

	out := h.mustRun(runNewConfig, "--no-login")
	contains(t, out, `Created ~/.claude-julienning3 (config "julienning3").`)
	contains(t, out, "Sign in: julienning login julienning3")
	contains(t, out, "claude-julienning3")

	dir := filepath.Join(h.home, ".claude-julienning3")
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", fi.Mode().Perm())
	}
	assertPatched(t, dir)
	if got := strings.Join(h.names(), ","); got != "julienning1,julienning3" {
		t.Fatalf("registered = %q", got)
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("unexpected warning: %s", h.stderr.String())
	}
}

func TestNewConfigWarnsWhenCommandIsNotInstalled(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.stable = false
	h.mustRun(runNewConfig)
	if n := strings.Count(h.stderr.String(), "warning: settings.json will run"); n != 1 {
		t.Fatalf("warnings: %q", h.stderr.String())
	}
}

func TestNewConfigNameForms(t *testing.T) {
	for _, arg := range []string{"foo", ".claude-foo"} {
		t.Run(arg, func(t *testing.T) {
			h := newHarness(t)
			h.initConfig(false)
			out := h.mustRun(runNewConfig, "--name", arg)
			contains(t, out, `~/.claude-foo (config "foo")`)
			if _, ok := h.config().Find("foo"); !ok {
				t.Fatal("foo not registered")
			}
		})
	}
}

func TestNewConfigInvalidName(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	if err := h.run(runNewConfig, "--name", "bad name"); err == nil || !strings.Contains(err.Error(), "invalid config name") {
		t.Fatalf("got %v", err)
	}
}

// The existence check is the os.Mkdir EEXIST itself (no stat-then-create
// window), and a losing racer must leave the winner's dir and registry alone.
func TestNewConfigExistingDir(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	dir := h.mkdir(".claude-foo")
	h.writeFile(filepath.Join(dir, "settings.json"), `{"model":"opus"}`)

	err := h.run(runNewConfig, "--name", "foo")
	if err == nil || !strings.Contains(err.Error(), "julienning adopt ~/.claude-foo") {
		t.Fatalf("got %v, want an adopt suggestion", err)
	}
	if _, ok := h.config().Find("foo"); ok {
		t.Fatal("foo was registered despite the failure")
	}
	if got := h.read(filepath.Join(dir, "settings.json")); got != `{"model":"opus"}` {
		t.Fatalf("existing settings.json was overwritten: %s", got)
	}
}

func TestNewConfigSameNameTwiceRegistersOnce(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.mustRun(runNewConfig)
	err := h.run(runNewConfig, "--name", "julienning1")
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("got %v", err)
	}
	if got := strings.Join(h.names(), ","); got != "julienning1" {
		t.Fatalf("registered = %q, want julienning1 exactly once", got)
	}
}

func TestNewConfigCopySettings(t *testing.T) {
	h := newHarness(t)
	src := h.mkdir(".claude-src")
	h.writeFile(filepath.Join(src, "settings.json"), `{"model":"opus","statusLine":{"type":"command","command":"/old/julienning statusline"}}`)
	h.initConfig(false, config.ConfigDir{Name: "src", Dir: src})

	h.mustRun(runNewConfig, "--name", "copy", "--copy-settings-from", "src")
	dir := filepath.Join(h.home, ".claude-copy")
	contains(t, h.read(filepath.Join(dir, "settings.json")), `"model":"opus"`)
	assertPatched(t, dir)
}

// A failed patch leaves nothing behind, so the same name works on retry.
func TestNewConfigCleansUpOnPatchFailure(t *testing.T) {
	h := newHarness(t)
	src := h.mkdir(".claude-src")
	h.writeFile(filepath.Join(src, "settings.json"), `[]`)
	h.initConfig(false, config.ConfigDir{Name: "src", Dir: src})

	err := h.run(runNewConfig, "--name", "copy", "--copy-settings-from", "src")
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.home, ".claude-copy")); !os.IsNotExist(err) {
		t.Fatal("half-created dir left behind")
	}
	if _, ok := h.config().Find("copy"); ok {
		t.Fatal("registered despite the failure")
	}
}

func TestNewConfigCopyFromUnknown(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	err := h.run(runNewConfig, "--name", "x", "--copy-settings-from", "nope")
	if err == nil || !strings.Contains(err.Error(), `unknown config "nope"`) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".claude-x")); !os.IsNotExist(err) {
		t.Fatal("dir created despite the failure")
	}
}

func TestNewConfigNotSetUp(t *testing.T) {
	h := newHarness(t)
	err := h.run(runNewConfig)
	if err == nil || !strings.Contains(err.Error(), "julienning setup") {
		t.Fatalf("got %v", err)
	}
}

// Signing in is the default: claude starts in the new dir once it is
// created, registered and patched. --login is still accepted and changes
// nothing.
func TestNewConfigSignsInByDefault(t *testing.T) {
	for _, args := range [][]string{{"--name", "fresh"}, {"--name", "fresh", "--login"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHarness(t)
			h.initConfig(false)
			out := h.mustRun(runNewConfig, args...)
			want := "Created ~/.claude-fresh (config \"fresh\").\n" +
				"Starting claude in it so you can sign in.\n" +
				"The account you sign in with will be shared with the team as its email name; julienning does that automatically when your first session starts.\n"
			if out != want {
				t.Fatalf("stdout = %q, want %q", out, want)
			}
			dir := filepath.Join(h.home, ".claude-fresh")
			if wantExec := []execCall{{dir: dir}}; !reflect.DeepEqual(h.execs, wantExec) {
				t.Fatalf("execs = %+v, want %+v", h.execs, wantExec)
			}
			// Everything is in place before claude starts.
			assertPatched(t, dir)
			if _, ok := h.config().Find("fresh"); !ok {
				t.Fatal("fresh not registered before the exec")
			}
		})
	}
}

func TestNewConfigNoLogin(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	out := h.mustRun(runNewConfig, "--name", "fresh", "--no-login")
	want := "Created ~/.claude-fresh (config \"fresh\").\n" +
		"Sign in: julienning login fresh\n" +
		"The account you sign in with will be shared with the team as its email name; julienning does that automatically when your first session starts.\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if len(h.execs) != 0 {
		t.Fatalf("started claude with --no-login: %+v", h.execs)
	}
	if _, ok := h.config().Find("fresh"); !ok {
		t.Fatal("fresh not registered")
	}
}

// Without claude the dir stays created and registered; the error says how
// to sign in later.
func TestNewConfigSignInFailureKeepsTheDir(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	launch.Exec = func(string, []string, string) error { return launch.ErrClaudeNotFound }
	err := h.run(runNewConfig, "--name", "fresh")
	want := "claude not found on PATH (install Claude Code first); ~/.claude-fresh is created, sign in later with: julienning login fresh"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !errors.Is(err, launch.ErrClaudeNotFound) {
		t.Fatalf("err does not wrap ErrClaudeNotFound: %v", err)
	}
	dir := filepath.Join(h.home, ".claude-fresh")
	assertPatched(t, dir)
	if _, ok := h.config().Find("fresh"); !ok {
		t.Fatal("fresh unregistered after a failed sign-in")
	}
}

// --- adopt ---

// Without --name a dir is <prefix><N>: only a trailing number survives from
// the basename, never the rest of it.
func TestAdoptAutoName(t *testing.T) {
	cases := []struct{ rel, want string }{
		{".claude-acme7", "julienning7"},
		{".claude-acme007", "julienning7"},
		{".claude-acme0", "julienning1"},
		{".claude-work", "julienning1"},
		{".myclaude", "julienning1"},
		{"weird dir", "julienning1"},
		{".claude", "default"},
	}
	for _, tc := range cases {
		t.Run(tc.rel, func(t *testing.T) {
			h := newHarness(t)
			h.initConfig(false)
			dir := h.mkdir(tc.rel)
			out := h.mustRun(runAdopt, dir)
			contains(t, out, `config "`+tc.want+`"`)
			notContains(t, out, "claude-"+tc.want) // config names have no aliases
			if cd, ok := h.config().Find(tc.want); !ok || cd.Dir != dir {
				t.Fatalf("%s not registered: %v", tc.want, h.names())
			}
			assertPatched(t, dir)
		})
	}
}

func TestAdoptNumberCollisionTakesNextFree(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false, config.ConfigDir{Name: "julienning1", Dir: h.mkdir(".claude-one")})
	for _, tc := range []struct{ rel, want string }{
		{".claude-acme1", "julienning2"}, // 1 is registered
		{".claude-acme3", "julienning3"}, // free: reused
		{".claude-acme", "julienning4"},  // no number: smallest free
		{".claude-acme2", "julienning5"}, // 2 was taken above
	} {
		h.mkdir(tc.rel)
		out := h.mustRun(runAdopt, "~/"+tc.rel)
		contains(t, out, `Adopted ~/`+tc.rel+` as config "`+tc.want+`"`)
	}
	if got := strings.Join(h.names(), ","); got != "julienning1,julienning2,julienning3,julienning4,julienning5" {
		t.Fatalf("registered = %q", got)
	}
}

func TestAdoptDefaultNameTakenFallsBackToNumber(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false, config.ConfigDir{Name: "default", Dir: h.mkdir("elsewhere")})
	h.mkdir(".claude")
	contains(t, h.mustRun(runAdopt, "~/.claude"), `config "julienning1"`)
}

func TestAdoptTildeNameFlagAndHint(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.cacheEmails("someone@else.io")
	dir := h.loggedIn(".claude-work", "me@work.io")
	h.writeFile(filepath.Join(dir, "settings.json"), `{"statusLine":{"type":"command","command":"line.sh"}}`)
	out := h.mustRun(runAdopt, "--name", "office", "~/.claude-work")
	contains(t, out, `Adopted ~/.claude-work as config "office" (settings.json added)`)
	contains(t, out, `Replaced statusLine "line.sh"`)
	contains(t, out, "me@work.io is not shared with the team yet: julienning share me@work.io")
	cd, ok := h.config().Find("office")
	if !ok || cd.Dir != dir {
		t.Fatalf("registered as %+v, want dir %s", cd, dir)
	}
}

func TestAdoptErrors(t *testing.T) {
	h := newHarness(t)
	reg := h.mkdir(".claude-reg")
	h.initConfig(false, config.ConfigDir{Name: "reg", Dir: reg})
	if err := h.run(runAdopt, filepath.Join(h.home, "nope")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing dir: %v", err)
	}
	h.writeFile(filepath.Join(h.home, "afile"), "x")
	if err := h.run(runAdopt, filepath.Join(h.home, "afile")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file: %v", err)
	}
	h.mkdir("other")
	if err := h.run(runAdopt, "--name", "bad name", "~/other"); err == nil || !strings.Contains(err.Error(), "invalid config name") {
		t.Errorf("bad name: %v", err)
	}
	if err := h.run(runAdopt, reg); err == nil || !strings.Contains(err.Error(), `already registered as "reg"`) {
		t.Errorf("registered: %v", err)
	}
	if err := h.run(runAdopt, "--name", "reg", "~/other"); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("taken name: %v", err)
	}
	var ue *usageErr
	if !asUsage(h.run(runAdopt), &ue) {
		t.Error("no args: want usage error")
	}
}

func TestAdoptFlagAfterPositional(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.mkdir(".claude-work")
	h.mustRun(runAdopt, "~/.claude-work", "--name", "office")
	if _, ok := h.config().Find("office"); !ok {
		t.Fatalf("not registered: %v", h.names())
	}
}

// --- forget ---

func TestForgetUnpatchesAndClearsCurrent(t *testing.T) {
	h := newHarness(t)
	d1 := h.mkdir(".claude-one")
	original := "{\n  \"model\": \"opus\",\n  \"statusLine\": {\"type\": \"command\", \"command\": \"mine.sh\"}\n}\n"
	h.writeFile(filepath.Join(d1, "settings.json"), original)
	h.initConfig(false)
	h.mustRun(runAdopt, "--name", "one", d1)
	if err := config.SetCurrent(config.ConfigDir{Name: "one", Dir: d1}); err != nil {
		t.Fatal(err)
	}

	out := h.mustRun(runForget, "one")
	contains(t, out, "Forgot one (~/.claude-one, not logged in).\n")
	contains(t, out, "Kept ~/.claude-one: no terminal to ask, and --delete was not passed.")
	contains(t, out, "settings.json: restored previous statusLine, removed SessionStart hook, removed SessionEnd hook")
	contains(t, out, "nothing is selected now")
	if _, ok := h.config().Find("one"); ok {
		t.Fatal("still registered")
	}
	if got := h.read(filepath.Join(d1, "settings.json")); got != original {
		t.Fatalf("settings.json not restored:\n%s", got)
	}
	if _, ok, _ := h.config().Current(); ok {
		t.Fatal("current not cleared")
	}
}

func TestForgetKeepsUnrelatedCurrent(t *testing.T) {
	h := newHarness(t)
	d1, d2 := h.mkdir(".claude-one"), h.mkdir(".claude-two")
	h.initConfig(false, config.ConfigDir{Name: "one", Dir: d1}, config.ConfigDir{Name: "two", Dir: d2})
	if err := config.SetCurrent(config.ConfigDir{Name: "one", Dir: d1}); err != nil {
		t.Fatal(err)
	}
	h.mustRun(runForget, "two")
	cur, ok, err := h.config().Current()
	if err != nil || !ok || cur.Name != "one" {
		t.Fatalf("current = %+v ok=%v err=%v", cur, ok, err)
	}
}

func TestForgetKeepsRegistrationWhenUnpatchFails(t *testing.T) {
	h := newHarness(t)
	d1 := h.mkdir(".claude-one")
	h.writeFile(filepath.Join(d1, "settings.json"), "[]")
	h.initConfig(false, config.ConfigDir{Name: "one", Dir: d1})
	err := h.run(runForget, "one")
	if err == nil || !strings.Contains(err.Error(), "re-run `julienning forget one`") {
		t.Fatalf("got %v", err)
	}
	if _, ok := h.config().Find("one"); !ok {
		t.Fatal("unregistered although its settings.json could not be cleaned")
	}
}

func TestForgetUnknown(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false, config.ConfigDir{Name: "one", Dir: h.mkdir(".claude-one")})
	err := h.run(runForget, "nope")
	if err == nil || !strings.Contains(err.Error(), `unknown target "nope"`) {
		t.Fatalf("got %v", err)
	}
	var ue *usageErr
	if !asUsage(h.run(runForget), &ue) {
		t.Error("no args: want usage error")
	}
}

// --- configs ---

func TestConfigsTable(t *testing.T) {
	h := newHarness(t)
	d1 := h.loggedIn(".claude-one", "claude1@team.io")
	d2 := h.mkdir(".claude-two")
	d3 := h.loggedIn(".claude-three", "me@personal.io")
	h.initConfig(false,
		config.ConfigDir{Name: "one", Dir: d1},
		config.ConfigDir{Name: "two", Dir: d2},
		config.ConfigDir{Name: "three", Dir: d3},
	)
	if err := config.SetCurrent(config.ConfigDir{Name: "one", Dir: d1}); err != nil {
		t.Fatal(err)
	}

	// Empty cache: SHARED is unknown.
	out := h.mustRun(runConfigs)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	if got := spaces.ReplaceAllString(strings.TrimSpace(lines[0]), "  "); got != "NAME  NICK  DIR  EMAIL  SHARED" {
		t.Errorf("header = %q", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(lines[1]), "?") {
		t.Errorf("want ? with an empty cache:\n%s", out)
	}

	h.cacheNicks("claude1@team.io=alpha")
	out = h.mustRun(runConfigs)
	byName := map[string]string{}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n")[1:] {
		byName[strings.Fields(strings.TrimPrefix(l, "*"))[0]] = spaces.ReplaceAllString(strings.TrimSpace(l), "  ")
	}
	if got := byName["one"]; got != "*  one  alpha  ~/.claude-one  claude1@team.io  yes" {
		t.Errorf("one = %q", got)
	}
	if got := byName["three"]; got != "three  -  ~/.claude-three  me@personal.io  no" {
		t.Errorf("three = %q", got)
	}
	if got := byName["two"]; got != "two  -  ~/.claude-two  (not logged in)  no" {
		t.Errorf("two = %q", got)
	}
}

func TestConfigsJSON(t *testing.T) {
	h := newHarness(t)
	d1 := h.loggedIn(".claude", "Claude1@Team.io")
	h.initConfig(false, config.ConfigDir{Name: "default", Dir: d1})
	out := h.mustRun(runConfigs, "--json")
	var doc struct {
		Configs []map[string]any `json:"configs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	want := map[string]any{
		"name": "default", "dir": d1, "email": "claude1@team.io", "nickname": "", "logged_in": true,
		"shared": nil, "current": false, "default": true, "share_on_login": false,
	}
	if len(doc.Configs) != 1 || !reflect.DeepEqual(doc.Configs[0], want) {
		t.Fatalf("got %v", doc.Configs)
	}

	h.cacheNicks("claude1@team.io=alpha")
	out = h.mustRun(runConfigs, "--json")
	contains(t, out, `"shared": true`)
	contains(t, out, `"nickname": "alpha"`)
}

func TestConfigsEmpty(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	contains(t, h.mustRun(runConfigs), "No config dirs registered")
	contains(t, h.mustRun(runConfigs, "--json"), `"configs": []`)
}

// --- shell-init ---

func TestShellInitNotSetUpIsSilent(t *testing.T) {
	h := newHarness(t)
	if err := h.run(runShellInit, "zsh"); err != nil {
		t.Fatalf("want exit 0, got %v", err)
	}
	if h.stdout.Len() != 0 || h.stderr.Len() != 0 {
		t.Fatalf("want no output, got %q / %q", h.stdout.String(), h.stderr.String())
	}
}

// One function per team nickname, logged in here or not; legacy records
// without a nickname get none, and a malformed nickname is skipped with a
// warning instead of breaking every shell start.
func TestShellInitEmitsNicknameFunctions(t *testing.T) {
	h := newHarness(t)
	one := h.loggedIn(".claude-one", "claude1@team.io")
	def := h.mkdir(".claude")
	h.initConfig(false, config.ConfigDir{Name: "one", Dir: one}, config.ConfigDir{Name: "default", Dir: def})
	h.cacheNicks("claude1@team.io=alpha", "far@team.io=beta", "legacy@team.io", "bad@team.io=Bad;rm")
	out := h.mustRun(runShellInit, "zsh")
	contains(t, out, "# julienning shell integration (generated; do not edit)")
	contains(t, out, `[ "$_jl_dir" != '`+def+`' ]`)
	contains(t, out, "function claude-alpha { local _jl_dir; _jl_dir=$(command julienning resolve-dir 'alpha') || return 1; "+
		`if [ "$_jl_dir" = '`+def+`' ]; then env -u CLAUDE_CONFIG_DIR claude "$@"; else CLAUDE_CONFIG_DIR="$_jl_dir" command claude "$@"; fi; }`)
	contains(t, out, "function claude-beta {")
	if n := strings.Count(out, "function claude-"); n != 2 {
		t.Errorf("%d nickname functions, want 2:\n%s", n, out)
	}
	for _, gone := range []string{"alias ", "claude-one", "claude-default", "legacy", "bad"} {
		notContains(t, out, gone)
	}
	contains(t, h.stderr.String(), `warning: skipping invalid nickname "bad;rm" of bad@team.io`)
}

func TestShellInitBadArgs(t *testing.T) {
	h := newHarness(t)
	var ue *usageErr
	if !asUsage(h.run(runShellInit), &ue) {
		t.Error("no args: want usage error")
	}
	if !asUsage(h.run(runShellInit, "fish"), &ue) {
		t.Error("fish: want usage error")
	}
	if !asUsage(h.run(runShellInit, "zsh", "extra"), &ue) {
		t.Error("extra arg: want usage error")
	}
}

// --- login ---

func TestSplitLoginArgs(t *testing.T) {
	cases := []struct {
		args    []string
		name    string
		extra   []string
		wantErr bool
	}{
		{args: []string{"a"}, name: "a"},
		{args: []string{"a", "--"}, name: "a", extra: []string{}},
		{args: []string{"a", "--", "--dangerously-skip-permissions", "x"}, name: "a", extra: []string{"--dangerously-skip-permissions", "x"}},
		{args: nil, wantErr: true},
		{args: []string{"--help"}, wantErr: true},
		{args: []string{"a", "b"}, wantErr: true},
	}
	for _, tc := range cases {
		name, extra, err := splitLoginArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%v: want error", tc.args)
			}
			continue
		}
		if err != nil || name != tc.name || strings.Join(extra, " ") != strings.Join(tc.extra, " ") {
			t.Errorf("%v: got %q %v %v", tc.args, name, extra, err)
		}
	}
}

func TestLoginUsesLaunchExec(t *testing.T) {
	h := newHarness(t)
	one := h.mkdir(".claude-one")
	def := h.mkdir(".claude")
	h.initConfig(false, config.ConfigDir{Name: "one", Dir: one}, config.ConfigDir{Name: "default", Dir: def})

	h.mustRun(runLogin, "one", "--", "--model", "opus")
	h.mustRun(runLogin, "default")
	if h.stdout.Len() != 0 {
		t.Errorf("login printed %q before exec", h.stdout.String())
	}
	want := []execCall{{dir: one, args: []string{"--model", "opus"}}, {dir: def}}
	if !reflect.DeepEqual(h.execs, want) {
		t.Fatalf("execs = %+v, want %+v", h.execs, want)
	}
}

func TestLoginUnknownTarget(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	err := h.run(runLogin, "nope")
	if err == nil || !strings.Contains(err.Error(), `unknown target "nope"`) {
		t.Fatalf("got %v", err)
	}
	if len(h.execs) != 0 {
		t.Fatalf("launched despite the error: %+v", h.execs)
	}
}

// --- helpers ---

func TestShortenHomeAndExpandPath(t *testing.T) {
	h := newHarness(t)
	if got := shortenHome(filepath.Join(h.home, ".claude-x")); got != "~/.claude-x" {
		t.Errorf("shortenHome = %q", got)
	}
	if got := shortenHome("/elsewhere/x"); got != "/elsewhere/x" {
		t.Errorf("shortenHome = %q", got)
	}
	if got := shortenHome(h.home + "extra"); got != h.home+"extra" {
		t.Errorf("shortenHome must not match a prefix that is not a path boundary: %q", got)
	}
	got, err := expandPath("~/.claude-x")
	if err != nil || got != filepath.Join(h.home, ".claude-x") {
		t.Errorf("expandPath = %q, %v", got, err)
	}
	if _, err := expandPath(""); err == nil {
		t.Error("expandPath(\"\"): want error")
	}
}

func TestValidateRemoteURL(t *testing.T) {
	good := map[string]string{
		"https://w.example.dev":        "https://w.example.dev",
		"https://w.example.dev/":       "https://w.example.dev",
		" http://localhost:8787/ ":     "http://localhost:8787",
		"https://w.example.dev/prefix": "https://w.example.dev/prefix",
	}
	for in, want := range good {
		if got, err := validateRemoteURL(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "w.example.dev", "ftp://w", "https://", "https://u:p@w.dev", "https://w.dev?token=x", "https://w.dev#x"} {
		if _, err := validateRemoteURL(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestHTTPHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if err := httpHealthz(t.Context(), srv.URL+"/"); err != nil {
		t.Fatalf("healthy Worker: %v", err)
	}
	bad := httptest.NewServer(http.NotFoundHandler())
	defer bad.Close()
	if err := httpHealthz(t.Context(), bad.URL); err == nil || !strings.Contains(err.Error(), "answered 404") {
		t.Fatalf("got %v", err)
	}
}

func TestIsAuthError(t *testing.T) {
	cases := map[error]bool{
		os.ErrNotExist: false,
		&remote.Error{Status: 401, Message: "unauthorized"}: true,
		&remote.Error{Status: 403, Message: "forbidden"}:    true,
		&remote.Error{Status: 500, Message: "boom"}:         false,
	}
	for err, want := range cases {
		if got := isAuthError(fmt.Errorf("wrapped: %w", err)); got != want {
			t.Errorf("%v: got %v", err, got)
		}
	}
	// The harness command must pass the ownership test, or every patch test
	// would silently exercise the "foreign statusLine" path instead.
	if !claudecfg.IsJulienningCommand(wantCommand) {
		t.Fatal("test command fails the ownership test")
	}
}

// --- new-config / rename with names ---

func TestNewConfigNumbersWithCustomPrefix(t *testing.T) {
	h := newHarness(t)
	cfg := h.initConfig(false, config.ConfigDir{Name: "team1", Dir: h.mkdir("elsewhere")})
	cfg.NamePrefix = "team"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	h.mkdir(".claude-team2") // on disk, not registered: 2 is not free

	out := h.mustRun(runNewConfig, "--no-login")
	contains(t, out, `Created ~/.claude-team3 (config "team3").`)
	contains(t, out, "Sign in: julienning login team3\n")
	notContains(t, out, "alias")
	if cd, ok := h.config().Find("team3"); !ok || cd.Dir != filepath.Join(h.home, ".claude-team3") {
		t.Fatalf("registered = %v", h.names())
	}
	// --name still wins over the prefix.
	contains(t, h.mustRun(runNewConfig, "--name", "solo"), `Created ~/.claude-solo (config "solo").`)
}

func TestRename(t *testing.T) {
	h := newHarness(t)
	dir := h.mkdir(".claude-acme1")
	other := h.mkdir(".claude-acme2")
	h.initConfig(false, config.ConfigDir{Name: "julienning1", Dir: dir}, config.ConfigDir{Name: "julienning2", Dir: other})
	if err := config.SetCurrent(config.ConfigDir{Name: "julienning1", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	before := h.read(filepath.Join(h.jl, "current"))

	out := h.mustRun(runRename, "julienning1", "work")
	if want := "Renamed \"julienning1\" to \"work\" (~/.claude-acme1).\n"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}

	cfg := h.config()
	if cd, ok := cfg.Find("work"); !ok || cd.Dir != dir {
		t.Fatalf("not renamed: %v", h.names())
	}
	if _, ok := cfg.Find("julienning1"); ok {
		t.Fatal("old name still registered")
	}
	// `current` stores the path, so the selection follows the rename.
	if got := h.read(filepath.Join(h.jl, "current")); got != before {
		t.Fatalf("current changed: %q", got)
	}
	if cur, ok, err := cfg.Current(); err != nil || !ok || cur.Name != "work" {
		t.Fatalf("current = %+v %v %v", cur, ok, err)
	}
}

func TestRenameErrors(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false,
		config.ConfigDir{Name: "julienning1", Dir: h.mkdir(".claude-acme1")},
		config.ConfigDir{Name: "julienning2", Dir: h.mkdir(".claude-acme2")},
	)
	cases := []struct{ old, new, want string }{
		{"julienning1", "julienning2", `config name "julienning2" is taken by ~/.claude-acme2`},
		{"julienning1", "julienning1", `config "julienning1" already has that name`},
		{"julienning1", "bad name", `invalid config name "bad name"`},
		{"julienning1", "-x", `invalid config name "-x"`},
		{"nope", "work", `unknown config "nope" (known: julienning1, julienning2)`},
	}
	for _, tc := range cases {
		err := h.run(runRename, tc.old, tc.new)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("rename %s %s: got %v, want %q", tc.old, tc.new, err, tc.want)
		}
	}
	if got := strings.Join(h.names(), ","); got != "julienning1,julienning2" {
		t.Fatalf("a failed rename changed config.json: %q", got)
	}
	var ue *usageErr
	for _, args := range [][]string{nil, {"julienning1"}, {"a", "b", "c"}} {
		if !asUsage(h.run(runRename, args...), &ue) {
			t.Errorf("%v: want a usage error", args)
		}
	}
}

func TestRenameNotSetUp(t *testing.T) {
	h := newHarness(t)
	if err := h.run(runRename, "a", "b"); err == nil || !strings.Contains(err.Error(), "julienning setup") {
		t.Fatalf("got %v", err)
	}
}
