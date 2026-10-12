package dirs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/pins"
	"github.com/muratgozel/julienning/internal/shell"
)

// pinHarness: a team dir (alpha, registered) and a personal account in the
// unregistered default dir, julienning set up, the cache knowing alpha.
func pinHarness(t *testing.T) (h *harness, team, def string) {
	h = newHarness(t)
	team = h.loggedIn(".claude-julienning1", "claude1@x.io")
	def = h.loggedIn(".claude", "me@personal.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning1", Dir: team})
	h.cacheNicks("claude1@x.io=alpha", "claude2@x.io=beta")
	return h, team, def
}

func TestPinSetShowListUnpin(t *testing.T) {
	h, _, def := pinHarness(t)
	side := h.mkdir("Code/side")
	web := h.mkdir("Code/side/web")
	t.Chdir(side)

	out := h.mustRun(runPin, "Me@Personal.io")
	want := "Pinned ~/Code/side to me@personal.io (personal).\n" +
		"Plain claude and julienning use open it in ~/Code/side and every directory below it; julienning next is unaffected. Remove the pin with: julienning unpin\n" +
		"A terminal opened before this julienning version ignores pins until you run: source ~/.zshrc\n"
	if out != want {
		t.Errorf("first pin:\n got %q\nwant %q", out, want)
	}
	if d, ok := h.config().PersonalDirOf("me@personal.io"); !ok || d != def {
		t.Errorf("the personal dir is not remembered: %q %v", d, ok)
	}
	if len(h.config().Configs) != 1 {
		t.Error("pinning a personal account must not register its dir")
	}
	fi, err := os.Stat(filepath.Join(h.jl, config.PinsFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pins.json: %v %v", fi, err)
	}

	if out := h.mustRun(runPin, "me@personal.io"); out != "~/Code/side is already pinned to me@personal.io (personal).\n" {
		t.Errorf("same pin: %q", out)
	}
	out = h.mustRun(runPin, "alpha")
	contains(t, out, "Pinned ~/Code/side to alpha (claude1@x.io), replacing me@personal.io (personal).\n")
	notContains(t, out, "A terminal opened before")

	if out := h.mustRun(runPin); out != "~/Code/side is pinned to alpha (claude1@x.io).\n" {
		t.Errorf("show: %q", out)
	}
	t.Chdir(web)
	if out := h.mustRun(runPin); out != "~/Code/side/web is inside ~/Code/side, which is pinned to alpha (claude1@x.io).\n" {
		t.Errorf("show from below: %q", out)
	}
	if h.stderr.Len() != 0 {
		t.Errorf("a usable pin warns nothing: %q", h.stderr.String())
	}
	h.mustRun(runPin, personal) // a deeper pin
	out = h.mustRun(runPin, "--list")
	if want := "DIR              ACCOUNT\n~/Code/side      alpha (claude1@x.io)\n~/Code/side/web  me@personal.io (personal)\n"; out != want {
		t.Errorf("list:\n got %q\nwant %q", out, want)
	}

	// unpin removes the pin on the directory itself only.
	if out := h.mustRun(runUnpin); out != "Unpinned ~/Code/side/web (was me@personal.io (personal)).\n" {
		t.Errorf("unpin: %q", out)
	}
	err = h.run(runUnpin)
	if err == nil || err.Error() != "~/Code/side/web has no pin of its own; it is inside ~/Code/side, which is pinned to alpha (claude1@x.io); to remove that pin: julienning unpin ~/Code/side" {
		t.Errorf("unpin below a pin: %v", err)
	}
	if out := h.mustRun(runUnpin, "~/Code/side"); out != "Unpinned ~/Code/side (was alpha (claude1@x.io)).\n" {
		t.Errorf("unpin DIR: %q", out)
	}
	if _, err := os.Stat(filepath.Join(h.jl, config.PinsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the last unpin must remove pins.json: %v", err)
	}
	if out := h.mustRun(runUnpin); out != "~/Code/side/web is not pinned; nothing to unpin.\n" {
		t.Errorf("unpin without a pin: %q", out)
	}
	if out := h.mustRun(runPin, "--list"); !strings.HasPrefix(out, "No pins.") {
		t.Errorf("empty list: %q", out)
	}
	if out := h.mustRun(runPin); out != "No pin covers ~/Code/side/web; plain claude follows the selection.\n" {
		t.Errorf("show without a pin: %q", out)
	}
}

const personal = "me@personal.io"

// A config name whose dir holds a personal login pins that account and
// remembers the named dir, even when discovery would find another first.
func TestPinConfigNameWithPersonalLogin(t *testing.T) {
	h, _, _ := pinHarness(t)
	mine := h.loggedIn(".claude-mine", personal) // also logged in in ~/.claude
	cfg := h.config()
	if err := cfg.Add(config.ConfigDir{Name: "mine", Dir: mine}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(h.mkdir("Code/side"))
	contains(t, h.mustRun(runPin, "mine"), "Pinned ~/Code/side to me@personal.io (personal).")
	if d, _ := h.config().PersonalDirOf(personal); d != mine {
		t.Errorf("remembered %q, want %q", d, mine)
	}
}

func TestPinErrors(t *testing.T) {
	h, _, _ := pinHarness(t)
	fresh := h.mkdir(".claude-julienning2")
	cfg := h.config()
	if err := cfg.Add(config.ConfigDir{Name: "julienning2", Dir: fresh}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(h.mkdir("Code/side"))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"nobody@example.com"}, "no Claude config dir on this machine is logged in as nobody@example.com"},
		{[]string{"beta"}, "beta (claude2@x.io) is shared but not logged in on this machine"},
		{[]string{"julienning2"}, "julienning2 is not logged in, and a pin names an account; sign in first with: julienning login julienning2"},
		{[]string{"ghost"}, `unknown target "ghost"`},
		{[]string{"bad@"}, `"bad@" is not a valid email address`},
	} {
		err := h.run(runPin, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("pin %v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(h.jl, config.PinsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed pin must not create pins.json")
	}
	var ue *usageErr
	for _, args := range [][]string{{"--list", "alpha"}, {"alpha", "beta"}, {" "}, {"--bogus"}} {
		if err := h.run(runPin, args...); !asUsage(err, &ue) {
			t.Errorf("pin %v: want a usage error, got %v", args, err)
		}
		// cli.Main prints the error once; the flag package must not add its own.
		if h.stderr.Len() != 0 {
			t.Errorf("pin %v printed to stderr itself: %q", args, h.stderr.String())
		}
	}
	for _, args := range [][]string{{"a", "b"}, {"--bogus"}, {" "}} {
		if err := h.run(runUnpin, args...); !asUsage(err, &ue) || h.stderr.Len() != 0 {
			t.Errorf("unpin %v: err %v stderr %q", args, err, h.stderr.String())
		}
	}
}

// `pin` with no target warns when the pinned account cannot be opened here,
// which is exactly when plain claude refuses to start.
func TestPinShowWarnsAboutABrokenPin(t *testing.T) {
	h, _, _ := pinHarness(t)
	side := h.mkdir("Code/side")
	t.Chdir(side)
	h.mustRun(runPin, personal)
	h.writeFile(filepath.Join(h.home, ".claude.json"), `{"oauthAccount":{}}`)
	out := h.mustRun(runPin)
	if out != "~/Code/side is pinned to me@personal.io (personal).\n" {
		t.Errorf("stdout %q", out)
	}
	contains(t, h.stderr.String(), "julienning: warning: plain claude refuses to start here until this is fixed: me@personal.io is no longer logged in at ~/.claude")
}

func TestResolveDirPin(t *testing.T) {
	h, team, def := pinHarness(t)
	side := h.mkdir("Code/side")
	shop := h.mkdir("Code/shop")
	elsewhere := h.mkdir("elsewhere")

	// No pins file at all: nothing printed, success.
	t.Chdir(side)
	if out := h.mustRun(runResolveDir, "--pin"); out != "" {
		t.Errorf("no pins: %q", out)
	}
	if _, _, _, err := pins.Set(side, personal); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := pins.Set(shop, "claude1@x.io"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ cwd, want string }{
		{side, def + "\n"}, // the default dir, printed exactly like claudecfg.DefaultDir
		{h.mkdir("Code/side/web"), def + "\n"},
		{shop, team + "\n"},
		{elsewhere, ""},
	} {
		t.Chdir(tc.cwd)
		if out := h.mustRun(runResolveDir, "--pin"); out != tc.want {
			t.Errorf("in %s: %q, want %q", tc.cwd, out, tc.want)
		}
	}
	if d, ok := h.config().PersonalDirOf(personal); !ok || d != def {
		t.Errorf("resolve-dir --pin remembers the personal dir: %q %v", d, ok)
	}

	// The account moved: found again, with a note on stderr only.
	h.writeFile(filepath.Join(h.home, ".claude.json"), `{"oauthAccount":{"emailAddress":"other@x.io"}}`)
	moved := h.loggedIn(".claude-me", personal)
	t.Chdir(side)
	if out := h.mustRun(runResolveDir, "--pin"); out != moved+"\n" {
		t.Errorf("moved: %q", out)
	}
	contains(t, h.stderr.String(), "julienning: note: me@personal.io is no longer logged in at ~/.claude; found it at ~/.claude-me")

	// Logged out everywhere: an error, never an empty answer (the wrapper
	// would otherwise fall back to the selection).
	h.writeFile(filepath.Join(moved, ".claude.json"), `{"oauthAccount":{}}`)
	err := h.run(runResolveDir, "--pin")
	if err == nil || !strings.Contains(err.Error(), "~/Code/side is pinned to me@personal.io, which cannot be opened") ||
		!strings.HasSuffix(err.Error(), "(or remove the pin: julienning unpin ~/Code/side)") || h.stdout.Len() != 0 {
		t.Errorf("broken pin: err %v stdout %q", err, h.stdout.String())
	}

	// An unreadable selection only breaks ties: the pin still resolves.
	h.loggedIn(".claude-me", personal)
	cur := filepath.Join(h.jl, config.CurrentFile)
	if err := os.MkdirAll(cur, 0o700); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	if out := h.mustRun(runResolveDir, "--pin"); out != moved+"\n" {
		t.Errorf("unreadable current: %q", out)
	}
	contains(t, h.stderr.String(), "julienning: warning: ignoring the current selection")
	if err := os.Remove(cur); err != nil {
		t.Fatal(err)
	}

	// A corrupt pins.json is an error too.
	h.writeFile(filepath.Join(h.jl, config.PinsFile), "{")
	if err := h.run(runResolveDir, "--pin"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("corrupt pins.json: %v", err)
	}
	var ue *usageErr
	if err := h.run(runResolveDir, "--pin", "x"); !asUsage(err, &ue) {
		t.Errorf("extra argument: %v", err)
	}
}

// internal/shell cannot import config (config imports shell), so the
// wrapper spells the pins file name itself; it must stay config.PinsFile.
func TestWrapperNamesThePinsFile(t *testing.T) {
	code, err := shell.Init(shell.Zsh, nil, "/h/.claude")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, code, `[ -f "$_jl_home/`+config.PinsFile+`" ]`)
}
