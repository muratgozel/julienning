package dirs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/shell"
)

var spaces = regexp.MustCompile(` {2,}`)

// row finds the report line for a dir and collapses its column padding.
func row(t *testing.T, out, dir string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == dir {
			return spaces.ReplaceAllString(strings.TrimSpace(line), "  ")
		}
	}
	t.Fatalf("no row for %s in:\n%s", dir, out)
	return ""
}

func remoteFlags(extra ...string) []string {
	return append([]string{"--remote-url", testURL, "--token", testToken}, extra...)
}

func TestSetupFirstRunClassifiesAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude", "Personal@Example.com")
	team := h.loggedIn(".claude-julienning1", "claude1@team.io")
	h.loggedIn(".claude-x", "other@x.io")
	h.mkdir(".claude-y/projects")
	h.writeFile(filepath.Join(h.home, ".claude-y", "settings.json"), "{}")
	h.allow("claude1@team.io", "claude2@team.io")

	out := h.mustRun(runSetup, remoteFlags("--no-rc")...)
	contains(t, out, "Identity: dev=murat") // $USER lowercased
	contains(t, out, "(created)")
	contains(t, out, "Remote:   "+testURL+" (updated, verified)")
	contains(t, out, "Shared:   2 account(s) on the team allowlist")
	contains(t, out, "Dirs:     4 found")
	if got := row(t, out, "~/.claude-julienning1"); got != "~/.claude-julienning1  claude1  claude1@team.io  shared (registered as julienning1)" {
		t.Errorf("row = %q", got)
	}
	if got := row(t, out, "~/.claude"); got != "~/.claude  -  personal@example.com  not shared (re-run setup in a terminal or pass --share EMAIL)" {
		t.Errorf("row = %q", got)
	}
	if got := row(t, out, "~/.claude-x"); got != "~/.claude-x  -  other@x.io  not shared (re-run setup in a terminal or pass --share EMAIL)" {
		t.Errorf("row = %q", got)
	}
	if got := row(t, out, "~/.claude-y"); got != "~/.claude-y  -  -  not logged in" {
		t.Errorf("row = %q", got)
	}
	contains(t, out, "Settings: julienning1 added")
	contains(t, out, "Next steps:")
	notContains(t, out+h.stderr.String(), testToken)

	cfg := h.config()
	if got := strings.Join(h.names(), ","); got != "julienning1" {
		t.Fatalf("registered = %q", got)
	}
	if cfg.Remote.URL != testURL || cfg.Remote.Token != testToken {
		t.Fatalf("remote not saved: %+v", cfg.Remote.URL)
	}
	assertPatched(t, team)
	cache, err := sharedcache.Load()
	if err != nil || !cache.Contains("claude2@team.io") || !cache.FetchedAt.Equal(fixedNow) {
		t.Fatalf("shared.json = %+v, %v", cache, err)
	}
	machineID := cfg.MachineID

	out = h.mustRun(runSetup, "--no-rc")
	contains(t, out, "(unchanged)")
	contains(t, out, "Remote:   "+testURL+" (unchanged, verified)")
	contains(t, out, "Settings: julienning1 unchanged")
	notContains(t, out, "(created)")
	if h.config().MachineID != machineID {
		t.Error("machine id was regenerated on re-run")
	}
	if ops := strings.Join(h.fake.Ops(), ","); ops != "list,list" {
		t.Errorf("Worker calls = %s, want only listings", ops)
	}
}

func TestSetupShareFlag(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude-work", "Work@X.io")
	h.loggedIn(".claude-other", "other@x.io")
	h.allow()

	out := h.mustRun(runSetup, remoteFlags("--no-rc", "--share", "work@x.io", "--share", "ghost@x.io")...)
	if got := row(t, out, "~/.claude-work"); got != "~/.claude-work  work  work@x.io  shared (registered as julienning1)" {
		t.Errorf("row = %q", got)
	}
	contains(t, row(t, out, "~/.claude-other"), "not shared (re-run setup")
	contains(t, out, "--share ghost@x.io: no config dir here is logged in as it")

	var share *remote.Call
	for i, c := range h.fake.Calls {
		if c.Op == "share" {
			share = &h.fake.Calls[i]
		}
	}
	if share == nil || share.Email != "work@x.io" || share.Nickname != "work" || share.Identity.Dev != "murat" || len(share.Identity.MachineID) != 12 {
		t.Fatalf("share call = %+v", share)
	}
	if n := strings.Count(strings.Join(h.fake.Ops(), ","), "share"); n != 1 {
		t.Fatalf("%d share calls, want 1 (ghost@x.io is not found locally)", n)
	}
	cache, _ := sharedcache.Load()
	if !cache.Contains("work@x.io") || cache.Nickname("work@x.io") != "work" {
		t.Fatalf("shared.json not updated after sharing: %+v", cache)
	}
	cd, ok := h.config().Find("julienning1")
	if !ok || cd.Dir != dir {
		t.Fatalf("work not registered as julienning1: %v", h.names())
	}
}

func TestSetupInteractivePromptsOncePerEmail(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	h.loggedIn(".claude-a", "a@x.io")
	h.loggedIn(".claude-a2", "a@x.io") // same account in a second dir: asked once
	h.loggedIn(".claude-b", "b@x.io")
	h.allow()
	h.stdin = strings.NewReader("y\n\nn\n") // share a@x.io under the default nickname

	out := h.mustRun(runSetup, remoteFlags("--dev", "murat", "--no-rc")...)
	contains(t, out, "Share a@x.io (found in ~/.claude-a) with the team? [y/N] Nickname for a@x.io [a]: ")
	contains(t, out, "Share b@x.io (found in ~/.claude-b) with the team? [y/N] ")
	if n := strings.Count(out, "Share a@x.io"); n != 1 {
		t.Fatalf("asked %d times about a@x.io", n)
	}
	if got := row(t, out, "~/.claude-a"); got != "~/.claude-a  a  a@x.io  shared (registered as julienning1)" {
		t.Errorf("row = %q", got)
	}
	contains(t, row(t, out, "~/.claude-a2"), "shared (registered as julienning2)")
	if c := h.fake.CallsFor("share"); len(c) != 1 || c[0].Nickname != "a" {
		t.Fatalf("share calls = %+v", c)
	}
	contains(t, row(t, out, "~/.claude-b"), "personal (declined)")
	if got := strings.Join(h.names(), ","); got != "julienning1,julienning2" {
		t.Fatalf("registered = %q", got)
	}
	if got := h.config().DeclinedEmails; len(got) != 1 || got[0] != "b@x.io" {
		t.Fatalf("declined = %v", got)
	}

	// A re-run does not ask again: a@x.io is cached as shared, b@x.io declined.
	h.stdin = strings.NewReader("")
	h.allow("a@x.io")
	out = h.mustRun(runSetup, "--no-rc")
	notContains(t, out, "with the team? [y/N]")
	contains(t, row(t, out, "~/.claude-b"), "personal (declined)")
}

// --yes never prompts and never shares anything not passed with --share: a
// personal account must not leak to the team because of a flag meant to
// skip questions.
func TestSetupYesDoesNotShare(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	h.loggedIn(".claude-a", "a@x.io")
	h.stdin = strings.NewReader("y\n")
	out := h.mustRun(runSetup, remoteFlags("--no-rc", "--yes")...)
	notContains(t, out, "[y/N]")
	contains(t, row(t, out, "~/.claude-a"), "not shared")
	for _, op := range h.fake.Ops() {
		if op == "share" {
			t.Fatal("--yes shared an account")
		}
	}
}

func TestSetupPromptsForRemoteAndHidesToken(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	h.secrets = []string{testToken}
	h.stdin = strings.NewReader("\nhttps://w.example.dev/\n") // dev name default, then URL
	out := h.mustRun(runSetup, "--no-rc")
	contains(t, out, "Your dev name [murat]: ")
	contains(t, out, "Worker URL")
	contains(t, out, "Remote:   "+testURL+" (updated, verified)")
	notContains(t, out+h.stderr.String(), testToken)
	if h.secretCalls != 1 {
		t.Fatalf("token read %d times", h.secretCalls)
	}
	if cfg := h.config(); cfg.Remote.URL != testURL || cfg.Remote.Token != testToken {
		t.Fatal("prompted remote not saved")
	}
}

func TestSetupRejectedTokenIsAnError(t *testing.T) {
	h := newHarness(t)
	h.fake.ListErr = &remote.Error{Status: 401, Message: "unauthorized"}
	err := h.run(runSetup, remoteFlags("--no-rc")...)
	if !errors.Is(err, errTokenRejected) || !strings.Contains(err.Error(), "token was rejected") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.jl, "config.json")); !os.IsNotExist(err) {
		t.Fatal("a rejected token was saved")
	}
	notContains(t, h.stdout.String()+err.Error(), testToken)
}

func TestSetupRejectedSavedTokenPromptsAgain(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.interactive = true
	good := &remote.Fake{}
	newClient = func(cfg *config.Config) remote.Client {
		if cfg.Remote.Token == testToken {
			return &remote.Fake{ListErr: &remote.Error{Status: 401, Message: "unauthorized"}}
		}
		return good
	}
	h.secrets = []string{"rotated-token"}
	out := h.mustRun(runSetup, "--no-rc")
	contains(t, out, "rejected the team token")
	contains(t, out, "(updated, verified)")
	if got := h.config().Remote.Token; got != "rotated-token" {
		t.Fatal("new token not saved")
	}
}

func TestSetupUnreachableNewRemoteIsAnError(t *testing.T) {
	h := newHarness(t)
	h.healthErr = errors.New("cannot reach the Worker at " + testURL + ": connection refused")
	err := h.run(runSetup, remoteFlags("--no-rc")...)
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("got %v", err)
	}
}

// A saved Worker that is down must not block setup: it falls back to the
// cached allowlist.
func TestSetupUnreachableSavedRemoteUsesCache(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.cacheEmails("a@x.io")
	h.loggedIn(".claude-a", "a@x.io")
	h.loggedIn(".claude-b", "b@x.io")
	h.healthErr = errors.New("connection refused")
	out := h.mustRun(runSetup, "--no-rc")
	contains(t, out, "(unchanged, unreachable: connection refused)")
	contains(t, out, "Shared:   1 account(s) (cached 0s ago)")
	contains(t, row(t, out, "~/.claude-a"), "shared (registered as julienning1)")
	contains(t, row(t, out, "~/.claude-b"), "not shared (Worker not reachable")
}

func TestSetupWithoutRemote(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-a", "a@x.io")
	out := h.mustRun(runSetup, "--no-rc")
	contains(t, out, "Remote:   not configured")
	contains(t, out, "Shared:   unknown")
	contains(t, out, "Settings: no config dirs registered yet")
	contains(t, out, "julienning new-config --login")
	if len(h.fake.Calls) != 0 {
		t.Fatalf("Worker called without a remote: %v", h.fake.Ops())
	}
}

func TestSetupRegisteredDirsStayRegistered(t *testing.T) {
	h := newHarness(t)
	a := h.loggedIn(".claude-a", "now-personal@x.io")
	b := h.mkdir(".claude-b")
	gone := filepath.Join(h.home, ".claude-gone")
	h.initConfig(true,
		config.ConfigDir{Name: "a", Dir: a},
		config.ConfigDir{Name: "b", Dir: b},
		config.ConfigDir{Name: "gone", Dir: gone},
	)
	h.allow()
	out := h.mustRun(runSetup, "--no-rc")
	if got := row(t, out, "~/.claude-a"); got != "~/.claude-a  -  now-personal@x.io  now logged in as now-personal@x.io (not shared; registered as a)" {
		t.Errorf("row = %q", got)
	}
	contains(t, row(t, out, "~/.claude-b"), "not logged in (registered as b)")
	contains(t, row(t, out, "~/.claude-gone"), "missing (registered as gone; run `julienning forget gone`)")
	contains(t, out, "gone missing")
	if got := strings.Join(h.names(), ","); got != "a,b,gone" {
		t.Fatalf("registered = %q", got)
	}
	assertPatched(t, a)
}

func TestSetupDefaultDirIsRegisteredAsDefault(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude", "claude1@team.io")
	h.allow("claude1@team.io")
	out := h.mustRun(runSetup, remoteFlags("--no-rc")...)
	if got := row(t, out, "~/.claude"); got != "~/.claude  claude1  claude1@team.io  shared (registered as default)" {
		t.Errorf("row = %q", got)
	}
	cd, ok := h.config().Find("default")
	if !ok || cd.Dir != dir {
		t.Fatalf("default dir registered as %v", h.names())
	}
	assertPatched(t, dir) // ~/.claude/settings.json
}

func TestSetupShareFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-a", "a@x.io")
	h.fake.ShareErr = &remote.Error{Status: 500, Message: "internal error"}
	err := h.run(runSetup, remoteFlags("--no-rc", "--share", "a@x.io")...)
	if err == nil || !strings.Contains(err.Error(), "could not share 1 account") {
		t.Fatalf("got %v", err)
	}
	out := h.stdout.String()
	contains(t, row(t, out, "~/.claude-a"), "not shared (share failed: share: 500 internal error)")
	contains(t, out, "Next steps:") // the other steps still ran
	if len(h.config().Configs) != 0 {
		t.Fatal("unshared dir registered")
	}
}

func TestSetupReplacedStatusLineNote(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude-a", "a@x.io")
	h.writeFile(filepath.Join(dir, "settings.json"), `{"statusLine":{"type":"command","command":"~/bin/line.sh"}}`)
	h.allow("a@x.io")
	out := h.mustRun(runSetup, remoteFlags("--no-rc")...)
	contains(t, out, `note: julienning1: replaced statusLine "~/bin/line.sh" (saved;`)
}

func TestSetupWarnsOnceWhenCommandIsNotInstalled(t *testing.T) {
	h := newHarness(t)
	h.stable = false
	h.loggedIn(".claude-a", "a@x.io")
	h.loggedIn(".claude-b", "b@x.io")
	h.allow("a@x.io", "b@x.io")
	h.mustRun(runSetup, remoteFlags("--no-rc")...)
	if n := strings.Count(h.stderr.String(), "settings.json will run "+fakeExe); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, h.stderr.String())
	}
}

func TestSetupReportsNonObjectSettingsAndContinues(t *testing.T) {
	h := newHarness(t)
	d1 := h.loggedIn(".claude-a", "a@x.io")
	h.writeFile(filepath.Join(d1, "settings.json"), "[]")
	h.loggedIn(".claude-b", "b@x.io")
	h.allow("a@x.io", "b@x.io")

	// Every step still runs, but setup exits 1 like a failed share.
	err := h.run(runSetup, remoteFlags("--no-rc")...)
	if err == nil || !strings.Contains(err.Error(), "could not update settings.json of 1 config dir(s)") {
		t.Fatalf("got %v", err)
	}
	out := h.stdout.String()
	contains(t, out, "Settings: julienning1 failed, julienning2 added")
	contains(t, out, "Next steps:")
	contains(t, h.stderr.String(), "not a JSON object")
	if h.read(filepath.Join(d1, "settings.json")) != "[]" {
		t.Fatal("non-object settings.json was modified")
	}
}

func TestSetupReportsShareAndPatchFailuresTogether(t *testing.T) {
	h := newHarness(t)
	d1 := h.loggedIn(".claude-a", "a@x.io")
	h.writeFile(filepath.Join(d1, "settings.json"), "{nope")
	h.loggedIn(".claude-b", "b@x.io")
	h.allow("a@x.io")
	h.fake.ShareErr = &remote.Error{Status: 500, Message: "internal error"}
	err := h.run(runSetup, remoteFlags("--no-rc", "--share", "b@x.io")...)
	if err == nil || err.Error() != "could not share 1 account(s) with the team; could not update settings.json of 1 config dir(s) (see above); fix and re-run setup to retry" {
		t.Fatalf("got %v", err)
	}
}

func TestSetupBadFlags(t *testing.T) {
	h := newHarness(t)
	var ue *usageErr
	for _, args := range [][]string{
		{"oops"},
		{"--share", "not-an-email"},
		{"--remote-url", "ftp://x"},
		{"--remote-url", "https://user:pw@w.example.dev"},
		{"--shell", "fish"},
	} {
		if !asUsage(h.run(runSetup, args...), &ue) {
			t.Errorf("%v: want a usage error", args)
		}
	}
}

// --- identity ---

func TestSetupInteractiveDevPrompt(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	h.stdin = strings.NewReader("Ali\n\n") // dev name, then skip the Worker URL
	h.mustRun(runSetup, "--no-rc")
	if got := h.config().Dev; got != "ali" {
		t.Fatalf("dev = %q", got)
	}
}

func TestSetupNonInteractiveIgnoresStdin(t *testing.T) {
	h := newHarness(t)
	h.stdin = strings.NewReader("ali\n")
	out := h.mustRun(runSetup, "--no-rc")
	notContains(t, out, "Your dev name")
	if h.config().Dev != "murat" {
		t.Fatalf("dev = %q, want murat from $USER", h.config().Dev)
	}
}

func TestSetupDevFlag(t *testing.T) {
	h := newHarness(t)
	h.mustRun(runSetup, "--dev", "Ali", "--no-rc")
	if got := h.config().Dev; got != "ali" {
		t.Fatalf("dev = %q, want ali", got)
	}
	h.mustRun(runSetup, "--dev", "veli", "--no-rc")
	if got := h.config().Dev; got != "veli" {
		t.Fatalf("dev = %q, want veli", got)
	}
}

func TestSetupInvalidDev(t *testing.T) {
	h := newHarness(t)
	err := h.run(runSetup, "--dev", "Bad Name!")
	if err == nil || !strings.Contains(err.Error(), "invalid dev name") || !strings.Contains(err.Error(), "--dev") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestSetupMissingUserAndNoFlag(t *testing.T) {
	h := newHarness(t)
	t.Setenv("USER", "")
	err := h.run(runSetup)
	if err == nil || !strings.Contains(err.Error(), "--dev NAME") {
		t.Fatalf("got %v, want an actionable error", err)
	}
}

// --- shell hook ---

func TestSetupShellHookIdempotent(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun(runSetup)
	contains(t, out, "hook added to ~/.zshrc")
	out = h.mustRun(runSetup)
	contains(t, out, "~/.zshrc unchanged (hook already present)")
	want := `command -v julienning >/dev/null 2>&1 && eval "$(julienning shell-init zsh)"  # julienning-shell-hook` + "\n"
	if rc := h.read(filepath.Join(h.home, ".zshrc")); rc != want {
		t.Fatalf("rc =\n%q\nwant\n%q", rc, want)
	}
}

// The marker used to be matched against the whole file, so any mention of
// "# julienning" anywhere made setup skip the hook forever.
func TestSetupUnrelatedCommentDoesNotCountAsHook(t *testing.T) {
	h := newHarness(t)
	rc := filepath.Join(h.home, ".zshrc")
	h.writeFile(rc, "# julienning is great\n")

	out := h.mustRun(runSetup)
	contains(t, out, "hook added to ~/.zshrc")

	body := h.read(rc)
	contains(t, body, "# julienning is great")
	if n := strings.Count(body, rcMarker); n != 1 {
		t.Fatalf("rc has %d hook lines, want 1:\n%s", n, body)
	}
}

func TestSetupCommentedOutHookIsReAdded(t *testing.T) {
	h := newHarness(t)
	rc := filepath.Join(h.home, ".zshrc")
	h.writeFile(rc, "# "+hookLine(shell.Zsh)+"\n")

	out := h.mustRun(runSetup)
	contains(t, out, "hook added to ~/.zshrc")

	lines := strings.Split(strings.TrimRight(h.read(rc), "\n"), "\n")
	if len(lines) != 2 || lines[1] != hookLine(shell.Zsh) {
		t.Fatalf("rc = %q", lines)
	}
	out = h.mustRun(runSetup)
	contains(t, out, "unchanged (hook already present)")
	if n := strings.Count(h.read(rc), "\n"); n != 2 {
		t.Fatalf("rc grew on re-run:\n%s", h.read(rc))
	}
}

// A machine set up by an older build carries the legacy marker. It must be
// rewritten in place, never duplicated.
func TestSetupLegacyMarkerIsRewrittenOnce(t *testing.T) {
	h := newHarness(t)
	rc := filepath.Join(h.home, ".zshrc")
	legacy := `command -v julienning >/dev/null 2>&1 && eval "$(julienning shell-init zsh)"  ` + legacyRCMarker
	h.writeFile(rc, "export FOO=1\n"+legacy+"\nexport BAR=2\n")

	out := h.mustRun(runSetup)
	contains(t, out, "~/.zshrc updated marker")

	want := "export FOO=1\n" + hookLine(shell.Zsh) + "\nexport BAR=2\n"
	if got := h.read(rc); got != want {
		t.Fatalf("rc =\n%q\nwant\n%q", got, want)
	}
	out = h.mustRun(runSetup)
	contains(t, out, "unchanged (hook already present)")
	if got := h.read(rc); got != want {
		t.Fatalf("second run changed the rc file:\n%q", got)
	}
}

func TestSetupNoRC(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun(runSetup, "--no-rc")
	contains(t, out, "Shell:    skipped (--no-rc)")
	if _, err := os.Stat(filepath.Join(h.home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatal("rc file was created despite --no-rc")
	}
}

// Dir-name aliases are gone: a hand-written alias that selects a registered
// dir is replaced by its account's claude-<nickname> function, and one named
// like a generated function hides it. Setup says so and never edits them.
func TestSetupAppendsNewlineAndWarnsAboutHandWrittenAliases(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false,
		config.ConfigDir{Name: "julienning1", Dir: h.loggedIn(".claude-team1", "claude1@team.io")},
		config.ConfigDir{Name: "julienning2", Dir: h.mkdir(".claude-team2")},
	)
	h.cacheNicks("claude1@team.io=alpha", "other@team.io=beta")
	rc := filepath.Join(h.home, ".zshrc")
	h.writeFile(rc, "export FOO=1\n"+
		"alias claude-julienning1='CLAUDE_CONFIG_DIR=~/.claude-team1 claude'\n"+
		"alias work2='CLAUDE_CONFIG_DIR=~/.claude-team2 claude'\n"+
		"  alias claude-beta='x'\n"+
		"alias claude-old='y'\n"+
		"alias claude-alpha='CLAUDE_CONFIG_DIR=~/.claude-team1 claude'") // no trailing newline

	out := h.mustRun(runSetup)
	contains(t, out, "note: ~/.zshrc:2 has a hand-written `alias claude-julienning1` for ~/.claude-team1; julienning's `claude-alpha` function replaces it, so you can remove that line\n")
	contains(t, out, "note: ~/.zshrc:3 has a hand-written `alias work2` for ~/.claude-team2; its account's `claude-<nickname>` function replaces it, so you can remove that line\n")
	contains(t, out, "note: ~/.zshrc:4 has a hand-written `alias claude-beta`; it hides julienning's claude-beta function, so remove it\n")
	contains(t, out, "note: ~/.zshrc:6 has a hand-written `alias claude-alpha`; it hides julienning's claude-alpha function, so remove it\n")
	if n := strings.Count(out, "~/.zshrc:6"); n != 1 {
		t.Errorf("line 6 reported %d times, want once", n)
	}
	notContains(t, out, "claude-old")

	body := h.read(rc)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if last := lines[len(lines)-1]; last != hookLine(shell.Zsh) {
		t.Fatalf("hook not appended on its own line: %q", last)
	}
	if !strings.Contains(body, "alias claude-julienning1=") {
		t.Fatal("hand-written alias was removed; setup must never edit them")
	}
}

func TestSetupBashRC(t *testing.T) {
	h := newHarness(t)
	h.mustRun(runSetup, "--shell", "bash")
	want := filepath.Join(h.home, ".bash_profile")
	if _, err := os.Stat(want); err != nil {
		want = filepath.Join(h.home, ".bashrc") // Linux
		if _, err := os.Stat(want); err != nil {
			t.Fatal("no bash rc file created")
		}
	}
	if !strings.Contains(h.read(want), "shell-init bash") {
		t.Fatal("hook does not name bash")
	}
}

func TestSetupUnknownShellEnvFallsBackToZsh(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SHELL", "/usr/bin/fish")
	out := h.mustRun(runSetup)
	contains(t, out, "using zsh")
	contains(t, out, "~/.zshrc")
}

func TestStringListFlag(t *testing.T) {
	var s stringList
	_ = s.Set("a")
	_ = s.Set("b")
	if s.String() != "a,b" {
		t.Fatalf("got %q", s.String())
	}
}

func TestHTTPHealthzReportsStatus(t *testing.T) {
	err := httpHealthz(context.Background(), "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "cannot reach the Worker at http://127.0.0.1:1") {
		t.Fatalf("got %v", err)
	}
}

// --- config names ---

// Team dirs are often named after the company: only their trailing number
// carries over, so aliases never leak the dir name.
func TestSetupNamesReuseBasenameNumbers(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude", "c0@team.io")
	h.loggedIn(".claude-acme", "c3@team.io")
	h.loggedIn(".claude-acme1", "c1@team.io")
	h.loggedIn(".claude-acme2", "c2@team.io")
	h.allow("c0@team.io", "c1@team.io", "c2@team.io", "c3@team.io")

	out := h.mustRun(runSetup, remoteFlags("--no-rc")...)
	for dir, want := range map[string]string{
		"~/.claude":       "~/.claude  c0  c0@team.io  shared (registered as default)",
		"~/.claude-acme":  "~/.claude-acme  c3  c3@team.io  shared (registered as julienning3)",
		"~/.claude-acme1": "~/.claude-acme1  c1  c1@team.io  shared (registered as julienning1)",
		"~/.claude-acme2": "~/.claude-acme2  c2  c2@team.io  shared (registered as julienning2)",
	} {
		if got := row(t, out, dir); got != want {
			t.Errorf("row = %q, want %q", got, want)
		}
	}
	contains(t, out, "Settings: default added, julienning1 added, julienning2 added, julienning3 added")
	if got := strings.Join(h.names(), ","); got != "default,julienning1,julienning2,julienning3" {
		t.Fatalf("registered = %q", got)
	}

	// A re-run keeps the names; a new dir whose number is taken gets the
	// smallest free one.
	h.loggedIn(".claude-other1", "c4@team.io")
	h.allow("c0@team.io", "c1@team.io", "c2@team.io", "c3@team.io", "c4@team.io")
	out = h.mustRun(runSetup, "--no-rc")
	contains(t, row(t, out, "~/.claude-acme"), "julienning3")
	if got := row(t, out, "~/.claude-other1"); got != "~/.claude-other1  c4  c4@team.io  shared (registered as julienning4)" {
		t.Errorf("row = %q", got)
	}
}

// Dirs setup registers get the low numbers; a personal dir that is only
// listed never takes one from a team dir. Unregistered dirs show no config
// name: names are internal labels, nicknames are what people type.
func TestSetupRegisteredDirsGetNumbersFirst(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-me", "me@personal.io")
	h.mkdir(".claude-new/projects")
	h.writeFile(filepath.Join(h.home, ".claude-new", "settings.json"), "{}")
	h.loggedIn(".claude-team", "t@team.io")
	h.allow("t@team.io")

	out := h.mustRun(runSetup, remoteFlags("--no-rc")...)
	if got := row(t, out, "~/.claude-team"); got != "~/.claude-team  t  t@team.io  shared (registered as julienning1)" {
		t.Errorf("row = %q", got)
	}
	if got := row(t, out, "~/.claude-me"); got != "~/.claude-me  -  me@personal.io  not shared (re-run setup in a terminal or pass --share EMAIL)" {
		t.Errorf("row = %q", got)
	}
	if got := row(t, out, "~/.claude-new"); got != "~/.claude-new  -  -  not logged in" {
		t.Errorf("row = %q", got)
	}
	if got := strings.Join(h.names(), ","); got != "julienning1" {
		t.Fatalf("registered = %q", got)
	}
}

func TestSetupNamePrefix(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-acme2", "a@team.io")
	h.allow("a@team.io")

	out := h.mustRun(runSetup, remoteFlags("--no-rc", "--name-prefix", "crew")...)
	contains(t, out, "Names:    crew1, crew2, … for newly registered dirs (updated)")
	contains(t, row(t, out, "~/.claude-acme2"), "~/.claude-acme2  a  a@team.io  shared (registered as crew2)")
	if got := strings.Join(h.names(), ","); got != "crew2" {
		t.Fatalf("registered = %q", got)
	}
	if cfg := h.config(); cfg.NamePrefix != "crew" || cfg.Prefix() != "crew" {
		t.Fatalf("prefix not saved: %q", cfg.NamePrefix)
	}
	contains(t, h.read(filepath.Join(h.jl, "config.json")), `"name_prefix": "crew"`)

	// Kept without the flag; used by new-config.
	out = h.mustRun(runSetup, "--no-rc")
	notContains(t, out, "Names:")
	contains(t, h.mustRun(runNewConfig), `Created ~/.claude-crew1 (config "crew1").`)

	// A new prefix only affects dirs registered from now on.
	out = h.mustRun(runSetup, "--no-rc", "--name-prefix", "pool")
	contains(t, out, "Names:    pool1, pool2, … for newly registered dirs (updated; registered configs keep their names, see `julienning rename`)")
	if got := strings.Join(h.names(), ","); got != "crew1,crew2" {
		t.Fatalf("registered = %q", got)
	}
	out = h.mustRun(runSetup, "--no-rc", "--name-prefix", "pool")
	contains(t, out, "Names:    pool1, pool2, … for newly registered dirs (unchanged)")
	h.loggedIn(".claude-z", "z@team.io")
	h.allow("a@team.io", "z@team.io")
	contains(t, row(t, h.mustRun(runSetup, "--no-rc"), "~/.claude-z"), "~/.claude-z  z  z@team.io  shared (registered as pool1)")
}

func TestSetupInvalidNamePrefix(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"abc1", "a b", "-x", "team-2"} {
		var ue *usageErr
		err := h.run(runSetup, "--no-rc", "--name-prefix", p)
		if !asUsage(err, &ue) || !strings.Contains(err.Error(), "not ending in a digit") {
			t.Errorf("%q: got %v, want a usage error", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.jl, "config.json")); !os.IsNotExist(err) {
		t.Fatal("config.json written despite the invalid flag")
	}
}
