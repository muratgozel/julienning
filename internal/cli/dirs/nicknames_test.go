package dirs

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// takenNicks makes the fake Worker answer 409 to op (share or nickname)
// with any of nicks, like a team where those nicknames are held.
func (h *harness) takenNicks(op string, nicks ...string) {
	taken := map[string]bool{}
	for _, n := range nicks {
		taken[n] = true
	}
	// ErrFor runs inside the Fake's lock right after the call is recorded.
	h.fake.ErrFor = func(gotOp, _ string) error {
		last := h.fake.Calls[len(h.fake.Calls)-1]
		if gotOp == op && taken[last.Nickname] {
			return conflict409()
		}
		return nil
	}
}

func nicksOf(calls []remote.Call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Email+"="+c.Nickname)
	}
	return out
}

func cachedNick(t *testing.T, email string) string {
	t.Helper()
	c, err := sharedcache.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c.Nickname(email)
}

func TestDefaultNickname(t *testing.T) {
	cases := map[string]string{
		"claude1@x.io":                    "claude1",
		"Claude.One@X.io":                 "claude.one",
		"john+team@x.io":                  "john-team",
		"_x@x.io":                         "x",
		"+++@x.io":                        "",
		strings.Repeat("a", 40) + "@x.io": strings.Repeat("a", 32),
	}
	for email, want := range cases {
		if got := defaultNickname(email); got != want {
			t.Errorf("%s: got %q, want %q", email, got, want)
		}
	}
}

func TestParseNickFlags(t *testing.T) {
	got, err := parseNickFlags([]string{"A@X.io=Alpha", "b@x.io=beta", "a@x.io=alpha"})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"a@x.io": "alpha", "b@x.io": "beta"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range [][]string{
		{"a@x.io"}, {"=alpha"}, {"nope=alpha"}, {"a@x.io=Bad Name"}, {"a@x.io="},
		{"a@x.io=one", "a@x.io=two"}, {"a@x.io=same", "b@x.io=same"},
	} {
		var ue *usageErr
		if _, err := parseNickFlags(bad); !asUsage(err, &ue) {
			t.Errorf("%v: want a usage error, got %v", bad, err)
		}
	}
}

// --- setup ---

func TestSetupInteractiveNicknamePromptRetriesConflictAndInvalid(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	h.loggedIn(".claude-a", "a@x.io")
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "b@x.io", Nickname: "a"}}}
	h.takenNicks("share", "a")
	h.stdin = strings.NewReader("y\n\nAlpha Beta\nAlpha\n")

	out := h.mustRun(runSetup, remoteFlags("--dev", "murat", "--no-rc")...)
	contains(t, out, "Share a@x.io (found in ~/.claude-a) with the team? [y/N] Nickname for a@x.io [a]: ")
	contains(t, out, `          nickname "a" is already used by b@x.io; choose another.`+"\nNickname for a@x.io [a]: ")
	contains(t, out, `          invalid nickname "Alpha Beta": 1-32 characters`)
	if got := nicksOf(h.fake.CallsFor("share")); !reflect.DeepEqual(got, []string{"a@x.io=a", "a@x.io=alpha"}) {
		t.Fatalf("share calls = %v", got)
	}
	if got := row(t, out, "~/.claude-a"); got != "~/.claude-a  alpha  a@x.io  shared (registered as julienning1)" {
		t.Errorf("row = %q", got)
	}
	if got := cachedNick(t, "a@x.io"); got != "alpha" {
		t.Fatalf("cached nickname = %q", got)
	}
}

func TestSetupInteractiveNicknameEndOfInputFailsTheShare(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	h.loggedIn(".claude-a", "a@x.io")
	h.allow()
	h.stdin = strings.NewReader("y\n")
	err := h.run(runSetup, remoteFlags("--dev", "murat", "--no-rc")...)
	if err == nil || !strings.Contains(err.Error(), "could not share 1 account") {
		t.Fatalf("got %v", err)
	}
	contains(t, row(t, h.stdout.String(), "~/.claude-a"), "not shared (share failed: no nickname entered)")
	if n := len(h.fake.CallsFor("share")); n != 0 {
		t.Fatalf("%d share calls without a nickname", n)
	}
}

func TestSetupShareFlagUsesNickFlagOrDefault(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-a", "a@x.io")
	h.loggedIn(".claude-b", "b@x.io")
	h.allow()
	out := h.mustRun(runSetup, remoteFlags("--no-rc", "--share", "a@x.io", "--share", "b@x.io",
		"--nick", "a@x.io=Alpha", "--nick", "ghost@x.io=g", "--nick", "c@x.io=c")...)
	if got := nicksOf(h.fake.CallsFor("share")); !reflect.DeepEqual(got, []string{"a@x.io=alpha", "b@x.io=b"}) {
		t.Fatalf("share calls = %v", got)
	}
	notContains(t, out, "Nickname for")
	contains(t, row(t, out, "~/.claude-a"), "~/.claude-a  alpha  a@x.io  shared")
	contains(t, row(t, out, "~/.claude-b"), "~/.claude-b  b  b@x.io  shared")
	contains(t, out, "Note:     --nick c@x.io=c: no config dir here is logged in as it (use `julienning share c@x.io --nick c` to share it anyway)\n")
	contains(t, out, "Note:     --nick ghost@x.io=g: no config dir here is logged in as it")
}

func TestSetupNickFlagForAnAccountNotSharedThisRun(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-a", "a@x.io")
	h.loggedIn(".claude-b", "b@x.io")
	h.allow("b@x.io")
	out := h.mustRun(runSetup, remoteFlags("--no-rc", "--nick", "a@x.io=alpha", "--nick", "b@x.io=beta")...)
	contains(t, out, "Note:     --nick a@x.io=alpha: not shared in this run (add --share a@x.io)\n")
	contains(t, out, "Note:     --nick b@x.io=beta: it is already called b (rename with: julienning nick b beta)\n")
	if n := len(h.fake.CallsFor("share")) + len(h.fake.CallsFor("nickname")); n != 0 {
		t.Fatalf("%d Worker writes, want none", n)
	}
}

func TestSetupNickFlagWithoutWorkerIsReported(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.cacheEmails("a@x.io")
	h.loggedIn(".claude-a", "a@x.io")
	h.healthErr = errors.New("connection refused")
	out := h.mustRun(runSetup, "--no-rc", "--nick", "a@x.io=alpha")
	contains(t, out, "Note:     --nick a@x.io=alpha: not applied (Worker not reachable; re-run setup later)\n")
}

func TestSetupNonInteractiveNicknameConflictFailsTheShare(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-a", "a@x.io")
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "b@x.io", Nickname: "a"}}}
	h.takenNicks("share", "a")
	err := h.run(runSetup, remoteFlags("--no-rc", "--share", "a@x.io")...)
	if err == nil || !strings.Contains(err.Error(), "could not share 1 account") {
		t.Fatalf("got %v", err)
	}
	contains(t, row(t, h.stdout.String(), "~/.claude-a"), `not shared (share failed: nickname "a" is already used by b@x.io; pass --nick a@x.io=NAME)`)
	if len(h.config().Configs) != 0 {
		t.Fatal("unshared dir registered")
	}
}

func TestSetupNamesLegacyAccountsNonInteractive(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-c1", "c1@team.io")
	h.loggedIn(".claude-c2", "c2@team.io")
	h.allow("named@team.io")
	h.allowLegacy("c1@team.io", "c2@team.io", "far@team.io") // far: not logged in here
	out := h.mustRun(runSetup, remoteFlags("--no-rc", "--nick", "c2@team.io=two")...)
	if got := nicksOf(h.fake.CallsFor("nickname")); !reflect.DeepEqual(got, []string{"c1@team.io=c1", "c2@team.io=two"}) {
		t.Fatalf("nickname calls = %v", got)
	}
	contains(t, out, "Nickname: c1@team.io is now c1 (rename with: julienning nick c1 NEW)\n")
	contains(t, out, "Nickname: c2@team.io is now two (rename with: julienning nick two NEW)\n")
	contains(t, row(t, out, "~/.claude-c1"), "~/.claude-c1  c1  c1@team.io  shared (registered as julienning1)")
	contains(t, row(t, out, "~/.claude-c2"), "~/.claude-c2  two  c2@team.io  shared (registered as julienning2)")
	notContains(t, out, "--nick c2@team.io") // it was used
	if cachedNick(t, "c1@team.io") != "c1" || cachedNick(t, "c2@team.io") != "two" || cachedNick(t, "far@team.io") != "" {
		t.Fatal("nicknames not cached")
	}

	// Named accounts are left alone on the next run.
	h.fake.Reset()
	h.allow("c1@team.io", "c2@team.io", "named@team.io")
	h.mustRun(runSetup, "--no-rc")
	if n := len(h.fake.CallsFor("nickname")); n != 0 {
		t.Fatalf("%d nickname calls on a re-run", n)
	}
}

func TestSetupNamesLegacyAccountsInteractive(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	h.loggedIn(".claude-c1", "c1@team.io")
	h.loggedIn(".claude-c2", "c2@team.io")
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "holder@team.io", Nickname: "first"}}}
	h.allowLegacy("c1@team.io", "c2@team.io")
	h.takenNicks("nickname", "first")
	h.stdin = strings.NewReader("first\nuno\n\n")
	out := h.mustRun(runSetup, remoteFlags("--dev", "murat", "--no-rc")...)
	contains(t, out, "Nickname for c1@team.io [c1]: ")
	contains(t, out, `nickname "first" is already used by holder@team.io; choose another.`)
	contains(t, out, "Nickname for c2@team.io [c2]: ")
	if got := nicksOf(h.fake.CallsFor("nickname")); !reflect.DeepEqual(got, []string{"c1@team.io=first", "c1@team.io=uno", "c2@team.io=c2"}) {
		t.Fatalf("nickname calls = %v", got)
	}
	contains(t, out, "Nickname: c1@team.io is now uno (rename with: julienning nick uno NEW)\n")
	contains(t, row(t, out, "~/.claude-c2"), "~/.claude-c2  c2  c2@team.io")
}

// A legacy account that cannot get its default nickname stays shared:
// setup warns with the fix and still succeeds.
func TestSetupLegacyNamingFailureIsAWarning(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(".claude-c1", "c1@team.io")
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "other@team.io", Nickname: "c1"}}}
	h.allowLegacy("c1@team.io")
	h.takenNicks("nickname", "c1")
	out := h.mustRun(runSetup, remoteFlags("--no-rc")...)
	contains(t, h.stderr.String(), `julienning: warning: c1@team.io has no nickname yet: nickname "c1" is already used by other@team.io; name it with: julienning nick c1@team.io NAME`)
	contains(t, row(t, out, "~/.claude-c1"), "~/.claude-c1  -  c1@team.io  shared (registered as julienning1)")
	notContains(t, out, "Nickname: c1@team.io")

	h.fake.ErrFor = nil
	h.fake.SetNicknameErr = &remote.Error{Status: 500, Message: "internal error"}
	h.mustRun(runSetup, "--no-rc")
	contains(t, h.stderr.String(), "julienning: warning: c1@team.io has no nickname yet: set nickname: 500 internal error; name it with: julienning nick c1@team.io NAME\n")
}

// --- share / unshare ---

func TestShareNickFlagAndConflictNamesTheHolder(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.mustRun(runShare, "x@team.io", "--nick", "Alpha")
	if got := nicksOf(h.fake.CallsFor("share")); !reflect.DeepEqual(got, []string{"x@team.io=alpha"}) {
		t.Fatalf("share calls = %v", got)
	}
	contains(t, h.stdout.String(), "Shared x@team.io with the team as alpha.\n")
	if cachedNick(t, "x@team.io") != "alpha" {
		t.Fatal("nickname not cached")
	}

	// The conflict refreshes the allowlist to name who holds it.
	h.fake.Reset()
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "other@team.io", Nickname: "y"}}}
	h.takenNicks("share", "y")
	err := h.run(runShare, "y@team.io")
	if err == nil || err.Error() != `nickname "y" is already used by other@team.io; pick another: julienning share y@team.io --nick NAME` {
		t.Fatalf("got %v", err)
	}
	if ops := h.fake.Ops(); !reflect.DeepEqual(ops, []string{"share", "list"}) {
		t.Fatalf("ops = %v", ops)
	}

	// Holder unknown: the Worker's answer stands in.
	h.fake.Listing = nil
	err = h.run(runShare, "y@team.io")
	if err == nil || !strings.HasPrefix(err.Error(), `nickname "y" is already used by another team account; pick another`) {
		t.Fatalf("got %v", err)
	}
}

func TestShareOfAnAlreadyNamedAccountKeepsItsNickname(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "x@team.io", Nickname: "old"}}}
	out := h.mustRun(runShare, "x@team.io", "--nick", "new")
	contains(t, out, "x@team.io was already shared as old (rename with: julienning nick old NEW).\n")
	if cachedNick(t, "x@team.io") != "old" {
		t.Fatalf("cache = %q", cachedNick(t, "x@team.io"))
	}
	if n := len(h.fake.CallsFor("nickname")); n != 0 {
		t.Fatal("share renamed an existing account")
	}
}

func TestShareNamesALegacyRecord(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.allowLegacy("x@team.io")
	out := h.mustRun(runShare, "x@team.io")
	if ops := h.fake.Ops(); !reflect.DeepEqual(ops, []string{"share", "list", "nickname"}) {
		t.Fatalf("ops = %v", ops)
	}
	contains(t, out, "Shared x@team.io with the team as x.\n")
	if cachedNick(t, "x@team.io") != "x" {
		t.Fatal("nickname not cached")
	}

	h.takenNicks("nickname", "x")
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "x@team.io"}, {Email: "holder@team.io", Nickname: "x"}}}
	err := h.run(runShare, "x@team.io")
	if err == nil || err.Error() != `x@team.io has no nickname yet: nickname "x" is already used by holder@team.io; name it with: julienning nick x@team.io NAME` {
		t.Fatalf("got %v", err)
	}
	contains(t, h.stdout.String(), "Shared x@team.io with the team.\n")
}

func TestShareNicknameValidation(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	var ue *usageErr
	if err := h.run(runShare, "x@team.io", "--nick", "Bad Name"); !asUsage(err, &ue) {
		t.Fatalf("got %v", err)
	}
	if err := h.run(runShare, "+++@team.io"); !asUsage(err, &ue) || !strings.Contains(err.Error(), "pass --nick NAME") {
		t.Fatalf("got %v", err)
	}
	if len(h.fake.Calls) != 0 {
		t.Fatalf("Worker called: %v", h.fake.Ops())
	}
}

func TestUnshareByNicknameAndConfigName(t *testing.T) {
	h := newHarness(t)
	one := h.loggedIn(".claude-one", "one@team.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning1", Dir: one})
	h.cacheNicks("gone@team.io=gone", "one@team.io=uno")

	h.interactive = true
	h.stdin = strings.NewReader("n\n")
	out := h.mustRun(runUnshare, "GONE")
	contains(t, out, "Unshare gone (gone@team.io)? This deletes its usage and claims for the whole team. [y/N] ")
	if len(h.fake.Calls) != 0 {
		t.Fatalf("Worker called: %v", h.fake.Ops())
	}

	out = h.mustRun(runUnshare, "gone", "--yes")
	contains(t, out, "Unshared gone (gone@team.io); its usage and claims were removed from the Worker.\n")
	if c := h.fake.CallsFor("unshare"); len(c) != 1 || c[0].Email != "gone@team.io" {
		t.Fatalf("unshare calls = %+v", c)
	}

	h.fake.Reset()
	h.mustRun(runUnshare, "julienning1", "--yes")
	if c := h.fake.CallsFor("unshare"); len(c) != 1 || c[0].Email != "one@team.io" {
		t.Fatalf("unshare calls = %+v", c)
	}
}

func TestUnshareUnknownNicknameRefreshesOnce(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.cacheNicks("a@team.io=a")
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "new@team.io", Nickname: "fresh"}}}
	h.mustRun(runUnshare, "fresh", "--yes")
	if ops := h.fake.Ops(); !reflect.DeepEqual(ops, []string{"list", "unshare", "list"}) {
		t.Fatalf("ops = %v", ops)
	}
	if c := h.fake.CallsFor("unshare"); c[0].Email != "new@team.io" {
		t.Fatalf("unshared %s", c[0].Email)
	}

	h.fake.Reset()
	err := h.run(runUnshare, "nobody", "--yes")
	if err == nil || !strings.Contains(err.Error(), `unknown nickname "nobody"`) {
		t.Fatalf("got %v", err)
	}
	if ops := h.fake.Ops(); !reflect.DeepEqual(ops, []string{"list"}) {
		t.Fatalf("ops = %v", ops)
	}
	var ue *usageErr
	if err := h.run(runUnshare, "Not A Nick", "--yes"); !asUsage(err, &ue) {
		t.Fatalf("got %v", err)
	}
}

// --- nick ---

func TestNickRenamesByNicknameEmailOrConfig(t *testing.T) {
	h := newHarness(t)
	one := h.loggedIn(".claude-one", "c1@team.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning1", Dir: one})
	h.cacheNicks("c1@team.io=alpha", "c2@team.io")

	out := h.mustRun(runNick, "alpha", "Beta")
	if want := "Renamed alpha to beta (c1@team.io).\nShell function claude-beta replaces claude-alpha in new terminals (or run: exec $SHELL).\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if got := nicksOf(h.fake.CallsFor("nickname")); !reflect.DeepEqual(got, []string{"c1@team.io=beta"}) {
		t.Fatalf("nickname calls = %v", got)
	}
	if cachedNick(t, "c1@team.io") != "beta" {
		t.Fatal("cache not updated")
	}

	out = h.mustRun(runNick, "C2@team.io", "two")
	if want := "Named c2@team.io two.\nShell function claude-two is available in new terminals (or run: exec $SHELL).\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	h.fake.Reset()
	out = h.mustRun(runNick, "julienning1", "beta")
	contains(t, out, "c1@team.io is already called beta.")
	if len(h.fake.Calls) != 0 {
		t.Fatalf("Worker called: %v", h.fake.Ops())
	}
}

func TestNickErrors(t *testing.T) {
	h := newHarness(t)
	var ue *usageErr
	for _, args := range [][]string{nil, {"a"}, {"a", "b", "c"}, {"a", "Bad Name"}} {
		if !asUsage(h.run(runNick, args...), &ue) {
			t.Errorf("%v: want a usage error", args)
		}
	}
	if err := h.run(runNick, "a@team.io", "b"); err == nil || !strings.Contains(err.Error(), "julienning setup") {
		t.Fatalf("not set up: %v", err)
	}
	h.initConfig(false)
	if err := h.run(runNick, "a@team.io", "b"); !errors.Is(err, config.ErrRemoteNotConfigured) {
		t.Fatalf("no remote: %v", err)
	}

	h.initConfig(true)
	h.cacheNicks("a@team.io=a", "b@team.io=b")
	h.takenNicks("nickname", "b")
	h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "a@team.io", Nickname: "a"}, {Email: "b@team.io", Nickname: "b"}}}
	if err := h.run(runNick, "a", "b"); err == nil || err.Error() != `nickname "b" is already used by b@team.io; pick another name` {
		t.Fatalf("conflict: %v", err)
	}
	if cachedNick(t, "a@team.io") != "a" {
		t.Fatal("cache changed after a conflict")
	}

	h.fake.ErrFor = nil
	h.fake.SetNicknameErr = remote.NotSharedError()
	err := h.run(runNick, "a", "z")
	if err == nil || err.Error() != "a@team.io is not shared with the team (share it with: julienning share a@team.io --nick z)" {
		t.Fatalf("not shared: %v", err)
	}
	if c, _ := sharedcache.Load(); c.Contains("a@team.io") {
		t.Fatal("a not-shared account stayed in the cache")
	}

	h.fake.SetNicknameErr = &remote.Error{Status: 401, Message: "unauthorized"}
	if err := h.run(runNick, "b@team.io", "z"); !errors.Is(err, errTokenRejected) {
		t.Fatalf("401: %v", err)
	}
}

// --- login / resolve-dir ---

func TestLoginResolvesNicknameEmailAndConfig(t *testing.T) {
	h := newHarness(t)
	one := h.loggedIn(".claude-one", "c1@team.io")
	fresh := h.mkdir(".claude-fresh")
	h.initConfig(false, config.ConfigDir{Name: "julienning1", Dir: one}, config.ConfigDir{Name: "julienning2", Dir: fresh})
	h.cacheNicks("c1@team.io=alpha", "far@team.io=far")

	h.mustRun(runLogin, "alpha", "--", "--model", "opus")
	h.mustRun(runLogin, "C1@team.io")
	h.mustRun(runLogin, "julienning2")
	want := []execCall{{dir: one, args: []string{"--model", "opus"}}, {dir: one}, {dir: fresh}}
	if !reflect.DeepEqual(h.execs, want) {
		t.Fatalf("execs = %+v, want %+v", h.execs, want)
	}

	h.execs = nil
	err := h.run(runLogin, "far")
	var nl *resolve.NotLocalError
	if !errors.As(err, &nl) || !strings.Contains(err.Error(), "far (far@team.io) is shared but not logged in on this machine") {
		t.Fatalf("got %v", err)
	}
	if len(h.execs) != 0 {
		t.Fatalf("launched despite the error: %+v", h.execs)
	}
}

func TestResolveDir(t *testing.T) {
	h := newHarness(t)
	def := h.loggedIn(".claude", "home@team.io")
	a := h.loggedIn(".claude-a", "c1@team.io")
	b := h.loggedIn(".claude-b", "c1@team.io") // same account in two dirs
	fresh := h.mkdir(".claude-fresh")
	h.initConfig(false,
		config.ConfigDir{Name: "default", Dir: def + "/"},
		config.ConfigDir{Name: "julienning1", Dir: a},
		config.ConfigDir{Name: "julienning2", Dir: b},
		config.ConfigDir{Name: "julienning3", Dir: fresh},
	)
	h.cacheNicks("home@team.io=home", "c1@team.io=alpha", "far@team.io=far")

	for target, want := range map[string]string{
		"home":        filepath.Join(h.home, ".claude"), // exactly claudecfg.DefaultDir, as shell-init bakes in
		"alpha":       a,
		"c1@team.io":  a,
		"julienning3": fresh,
	} {
		if got := h.mustRun(runResolveDir, target); got != want+"\n" {
			t.Errorf("%s: got %q, want %q", target, got, want+"\n")
		}
	}
	// The current selection wins when several dirs hold the account.
	if err := config.SetCurrent(config.ConfigDir{Name: "julienning2", Dir: b}); err != nil {
		t.Fatal(err)
	}
	if got := h.mustRun(runResolveDir, "alpha"); got != b+"\n" {
		t.Errorf("with current: got %q", got)
	}

	err := h.run(runResolveDir, "far")
	var nl *resolve.NotLocalError
	if !errors.As(err, &nl) || h.stdout.Len() != 0 {
		t.Fatalf("not local: %v, stdout %q", err, h.stdout.String())
	}
	if err := h.run(runResolveDir, "ghost"); err == nil || !strings.Contains(err.Error(), `unknown target "ghost"`) || h.stdout.Len() != 0 {
		t.Fatalf("unknown: %v, stdout %q", err, h.stdout.String())
	}
	var ue *usageErr
	if !asUsage(h.run(runResolveDir), &ue) || !asUsage(h.run(runResolveDir, "a", "b"), &ue) {
		t.Fatal("want usage errors")
	}
}

// resolve-dir and its errors as the shell functions see them: path only on
// stdout, `julienning: <message>` on stderr, exit 1. It is hidden from help.
func TestResolveDirThroughMain(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.cacheNicks("far@team.io=far")
	var out, errOut bytes.Buffer
	code := cli.Main([]string{"resolve-dir", "far"}, strings.NewReader(""), &out, &errOut)
	if code != cli.ExitError || out.Len() != 0 || !strings.HasPrefix(errOut.String(), "julienning: far (far@team.io) is shared but not logged in on this machine") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	out.Reset()
	cli.Main([]string{"help"}, strings.NewReader(""), &out, &errOut)
	notContains(t, out.String(), "resolve-dir")
	contains(t, out.String(), "  nick ")
}
