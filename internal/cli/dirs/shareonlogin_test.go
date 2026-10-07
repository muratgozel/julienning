package dirs

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
)

// pendingOf returns the registered dir's pending share (nil when none).
func (h *harness) pendingOf(name string) *config.ShareOnLogin {
	h.t.Helper()
	cd, ok := h.config().Find(name)
	if !ok {
		h.t.Fatalf("config %q not registered: %v", name, h.names())
	}
	return cd.ShareOnLogin
}

func marked(nick string) *config.ShareOnLogin { return &config.ShareOnLogin{Nickname: nick} }

// --- new-config ---

// Without a terminal and without --nick the share waits for the login and
// takes the email's local part then.
func TestNewConfigMarksTheDirForSharing(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	out := h.mustRun(runNewConfig)
	want := "Created ~/.claude-julienning1 (config \"julienning1\").\n" +
		"Starting claude in it so you can sign in.\n" +
		"The account you sign in with will be shared with the team as its email name; julienning does that automatically when your first session starts.\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if got := h.pendingOf("julienning1"); !reflect.DeepEqual(got, marked("")) {
		t.Fatalf("share_on_login = %+v, want an empty mark", got)
	}
	if !strings.Contains(h.read(filepath.Join(h.jl, config.ConfigFile)), `"share_on_login": {}`) {
		t.Fatalf("config.json lacks the mark:\n%s", h.read(filepath.Join(h.jl, config.ConfigFile)))
	}
	if len(h.execs) != 1 {
		t.Fatalf("execs = %+v, want claude started once", h.execs)
	}
}

func TestNewConfigNickFlag(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.interactive = true // --nick answers the question: nothing is asked
	out := h.mustRun(runNewConfig, "--nick", " Delta ", "--no-login")
	want := "Created ~/.claude-julienning1 (config \"julienning1\").\n" +
		"Sign in: julienning login julienning1\n" +
		"The account you sign in with will be shared with the team as delta; julienning does that automatically when your first session starts.\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if got := h.pendingOf("julienning1"); !reflect.DeepEqual(got, marked("delta")) {
		t.Fatalf("share_on_login = %+v", got)
	}
	if len(h.execs) != 0 {
		t.Fatalf("started claude with --no-login: %+v", h.execs)
	}
}

func TestNewConfigNoShare(t *testing.T) {
	for _, args := range [][]string{{"--no-share"}, {"--no-share", "--no-login"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := newHarness(t)
			h.initConfig(false)
			h.interactive = true
			out := h.mustRun(runNewConfig, args...)
			contains(t, out, "This account stays personal (not shared).\n")
			notContains(t, out, "Nickname for")
			notContains(t, out, "will be shared")
			if got := h.pendingOf("julienning1"); got != nil {
				t.Fatalf("share_on_login = %+v, want none", got)
			}
		})
	}
}

// Bad flags are refused before anything is created.
func TestNewConfigNickFlagErrors(t *testing.T) {
	cases := []struct {
		args  []string
		usage bool
		want  string
	}{
		{[]string{"--nick", "Bad Name"}, true, `invalid nickname "Bad Name"`},
		{[]string{"--nick", "delta", "--no-share"}, true, "cannot be combined with --no-share"},
		{[]string{"--nick", "alpha"}, false, `nickname "alpha" is already used by claude1@team.io; pick another --nick, or leave it out to use the email's local part`},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			h := newHarness(t)
			h.initConfig(false)
			h.cacheNicks("claude1@team.io=alpha")
			err := h.run(runNewConfig, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			var ue *usageErr
			if asUsage(err, &ue) != c.usage {
				t.Fatalf("usage error = %v, want %v", !c.usage, c.usage)
			}
			if len(h.names()) != 0 {
				t.Fatalf("registered %v despite the error", h.names())
			}
		})
	}
}

// In a terminal the nickname is asked before anything is created; invalid
// answers and nicknames the cached allowlist says are taken are explained
// and asked again.
func TestNewConfigAsksForTheNickname(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.cacheNicks("claude1@team.io=alpha")
	h.interactive = true
	h.stdin = strings.NewReader("Bad Name\nAlpha\n Delta \n")
	out := h.mustRun(runNewConfig)
	const q = "Nickname for this account [the email's name]: "
	if !strings.HasPrefix(out, q) {
		t.Fatalf("does not start with the question:\n%s", out)
	}
	contains(t, out, q+`invalid nickname "Bad Name": 1-32 characters`)
	contains(t, out, "\n"+q+`nickname "alpha" is already used by claude1@team.io; choose another (or press Enter for the email's name)`+"\n"+q)
	if n := strings.Count(out, q); n != 3 {
		t.Fatalf("asked %d times, want 3:\n%s", n, out)
	}
	contains(t, out, "The account you sign in with will be shared with the team as delta;")
	if got := h.pendingOf("julienning1"); !reflect.DeepEqual(got, marked("delta")) {
		t.Fatalf("share_on_login = %+v", got)
	}
}

// Enter, or the end of input, leaves the choice to the email.
func TestNewConfigNicknameDefault(t *testing.T) {
	for name, stdin := range map[string]string{"enter": "\n", "eof": ""} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.initConfig(false)
			h.interactive = true
			h.stdin = strings.NewReader(stdin)
			out := h.mustRun(runNewConfig)
			contains(t, out, "Nickname for this account [the email's name]: ")
			contains(t, out, "shared with the team as its email name;")
			if got := h.pendingOf("julienning1"); !reflect.DeepEqual(got, marked("")) {
				t.Fatalf("share_on_login = %+v", got)
			}
		})
	}
}

// --- forget ---

func TestForgetDropsThePendingShare(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.mustRun(runNewConfig, "--nick", "delta", "--no-login")
	h.mustRun(runForget, "julienning1")
	if raw := h.read(filepath.Join(h.jl, config.ConfigFile)); strings.Contains(raw, "share_on_login") {
		t.Fatalf("mark survived forget:\n%s", raw)
	}
}

// --- configs ---

func TestConfigsShowsPendingShares(t *testing.T) {
	h := newHarness(t)
	fresh := h.mkdir(".claude-julienning1")
	landed := h.loggedIn(".claude-julienning2", "claude2@team.io")
	waiting := h.loggedIn(".claude-julienning3", "claude3@team.io")
	h.initConfig(false,
		config.ConfigDir{Name: "julienning1", Dir: fresh, ShareOnLogin: marked("delta")},
		config.ConfigDir{Name: "julienning2", Dir: landed, ShareOnLogin: marked("")},
		config.ConfigDir{Name: "julienning3", Dir: waiting, ShareOnLogin: marked("")},
	)
	h.cacheNicks("claude2@team.io=beta")

	out := h.mustRun(runConfigs)
	byName := map[string]string{}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n")[1:] {
		byName[strings.Fields(l)[0]] = spaces.ReplaceAllString(strings.TrimSpace(l), "  ")
	}
	want := map[string]string{
		"julienning1": "julienning1  -  ~/.claude-julienning1  (not logged in)  no (shares on login)",
		"julienning2": "julienning2  beta  ~/.claude-julienning2  claude2@team.io  yes", // landed, mark not cleared yet
		"julienning3": "julienning3  -  ~/.claude-julienning3  claude3@team.io  no (shares on login)",
	}
	if !reflect.DeepEqual(byName, want) {
		t.Fatalf("rows:\n%v\nwant:\n%v", byName, want)
	}

	out = h.mustRun(runConfigs, "--json")
	var doc struct {
		Configs []configRow `json:"configs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var got []bool
	for _, r := range doc.Configs {
		got = append(got, r.ShareOnLogin)
	}
	if !reflect.DeepEqual(got, []bool{true, false, true}) {
		t.Fatalf("share_on_login = %v, want [true false true]", got)
	}
}

// --- setup ---

// A dir new-config marked is offered like any other unshared account, with
// the mark's nickname as the default; sharing it clears the mark.
func TestSetupOffersPendingShareWithItsNickname(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	dir := h.loggedIn(".claude-julienning4", "claude4@team.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning4", Dir: dir, ShareOnLogin: marked("delta")})
	h.allow()
	h.stdin = strings.NewReader("y\n\n")

	out := h.mustRun(runSetup, "--no-rc")
	contains(t, out, "Share claude4@team.io (found in ~/.claude-julienning4) with the team? [y/N] Nickname for claude4@team.io [delta]: ")
	if got := nicksOf(h.fake.CallsFor("share")); !reflect.DeepEqual(got, []string{"claude4@team.io=delta"}) {
		t.Fatalf("share calls = %v", got)
	}
	if got := row(t, out, "~/.claude-julienning4"); got != "~/.claude-julienning4  delta  claude4@team.io  shared (registered as julienning4)" {
		t.Errorf("row = %q", got)
	}
	if got := h.pendingOf("julienning4"); got != nil {
		t.Fatalf("mark not cleared after sharing: %+v", got)
	}
}

func TestSetupDecliningPendingShareClearsIt(t *testing.T) {
	h := newHarness(t)
	h.interactive = true
	dir := h.loggedIn(".claude-julienning4", "me@personal.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning4", Dir: dir, ShareOnLogin: marked("")})
	h.allow()
	h.stdin = strings.NewReader("n\n")

	out := h.mustRun(runSetup, "--no-rc")
	contains(t, row(t, out, "~/.claude-julienning4"), "personal, declined; registered as julienning4")
	if got := h.pendingOf("julienning4"); got != nil {
		t.Fatalf("mark not cleared after declining: %+v", got)
	}
	if !h.config().Declined("me@personal.io") {
		t.Fatal("not declined")
	}
	if n := len(h.fake.CallsFor("share")); n != 0 {
		t.Fatalf("%d share calls after a no", n)
	}
}

// Without a terminal setup shares only what --share names, as always: a
// pending share is left to the session hooks and listed as such. --share
// takes --nick, else the mark's nickname, else the email's local part.
func TestSetupNonInteractivePendingShares(t *testing.T) {
	h := newHarness(t)
	a := h.loggedIn(".claude-julienning1", "a@team.io")
	b := h.loggedIn(".claude-julienning2", "b@team.io")
	c := h.loggedIn(".claude-julienning3", "c@team.io")
	d := h.loggedIn(".claude-julienning4", "d@team.io")
	h.initConfig(true,
		config.ConfigDir{Name: "julienning1", Dir: a, ShareOnLogin: marked("")},
		config.ConfigDir{Name: "julienning2", Dir: b, ShareOnLogin: marked("bravo")},
		config.ConfigDir{Name: "julienning3", Dir: c, ShareOnLogin: marked("ignored")},
		config.ConfigDir{Name: "julienning4", Dir: d, ShareOnLogin: marked("delta")},
	)
	h.allow()
	h.interactive = true // --yes: no questions all the same

	out := h.mustRun(runSetup, "--no-rc", "--yes",
		"--share", "a@team.io", "--share", "b@team.io", "--share", "c@team.io", "--nick", "c@team.io=charlie")
	notContains(t, out, "[y/N]")
	want := []string{"a@team.io=a", "b@team.io=bravo", "c@team.io=charlie"}
	if got := nicksOf(h.fake.CallsFor("share")); !reflect.DeepEqual(got, want) {
		t.Fatalf("share calls = %v, want %v", got, want)
	}
	contains(t, row(t, out, "~/.claude-julienning2"), "bravo  b@team.io  shared (registered as julienning2)")
	if got := row(t, out, "~/.claude-julienning4"); got != "~/.claude-julienning4  -  d@team.io  not shared yet (shares on login; registered as julienning4)" {
		t.Errorf("row = %q", got)
	}
	for _, n := range []string{"julienning1", "julienning2", "julienning3"} {
		if got := h.pendingOf(n); got != nil {
			t.Errorf("%s: mark not cleared: %+v", n, got)
		}
	}
	if got := h.pendingOf("julienning4"); !reflect.DeepEqual(got, marked("delta")) {
		t.Errorf("julienning4: mark = %+v, want it kept for the session hooks", got)
	}
}

// A taken nickname fails that share like any --share, and keeps the mark
// so a later run (or the next session) retries.
func TestSetupPendingConflictKeepsTheMark(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude-julienning1", "a@team.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning1", Dir: dir, ShareOnLogin: marked("alpha")})
	h.allow()
	h.takenNicks("share", "alpha")

	err := h.run(runSetup, "--no-rc", "--yes", "--share", "a@team.io")
	if err == nil || !strings.Contains(err.Error(), "could not share 1 account") {
		t.Fatalf("got %v", err)
	}
	contains(t, row(t, h.stdout.String(), "~/.claude-julienning1"), `not shared (share failed: nickname "alpha" is already used by another team account; pass --nick a@team.io=NAME)`)
	if got := h.pendingOf("julienning1"); !reflect.DeepEqual(got, marked("alpha")) {
		t.Fatalf("mark = %+v, want it kept", got)
	}
}

// Already shared (a teammate, or the background share got there first):
// nothing to share, the mark is settled.
func TestSetupPendingAlreadySharedClearsTheMark(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude-julienning1", "a@team.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning1", Dir: dir, ShareOnLogin: marked("")})
	h.allow("a@team.io")

	out := h.mustRun(runSetup, "--no-rc", "--yes")
	contains(t, row(t, out, "~/.claude-julienning1"), "shared (registered as julienning1)")
	if n := len(h.fake.CallsFor("share")); n != 0 {
		t.Fatalf("%d share calls for a shared account", n)
	}
	if got := h.pendingOf("julienning1"); got != nil {
		t.Fatalf("mark not cleared: %+v", got)
	}
}

// Rows say a share is pending; without the Worker nothing is attempted and
// the mark stays for the background share.
func TestSetupPendingRows(t *testing.T) {
	h := newHarness(t)
	fresh := h.mkdir(".claude-julienning1")
	waiting := h.loggedIn(".claude-julienning2", "b@team.io")
	h.initConfig(false,
		config.ConfigDir{Name: "julienning1", Dir: fresh, ShareOnLogin: marked("")},
		config.ConfigDir{Name: "julienning2", Dir: waiting, ShareOnLogin: marked("")},
	)
	out := h.mustRun(runSetup, "--no-rc", "--yes")
	if got := row(t, out, "~/.claude-julienning1"); got != "~/.claude-julienning1  -  -  not logged in (registered as julienning1; shares on login)" {
		t.Errorf("row = %q", got)
	}
	if got := row(t, out, "~/.claude-julienning2"); got != "~/.claude-julienning2  -  b@team.io  not shared yet (shares on login; registered as julienning2)" {
		t.Errorf("row = %q", got)
	}
	for _, n := range []string{"julienning1", "julienning2"} {
		if got := h.pendingOf(n); got == nil {
			t.Errorf("%s: mark dropped", n)
		}
	}
	if ops := h.fake.Ops(); len(ops) != 0 {
		t.Fatalf("Worker calls without a Worker: %v", ops)
	}
}

// A dir whose account this machine declined before is never shared by the
// pending mark; the mark is settled.
func TestSetupPendingDeclinedEmailIsNotShared(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude-julienning1", "me@personal.io")
	cfg := h.initConfig(true, config.ConfigDir{Name: "julienning1", Dir: dir, ShareOnLogin: marked("")})
	cfg.Decline("me@personal.io")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	h.allow()
	out := h.mustRun(runSetup, "--no-rc", "--yes")
	contains(t, row(t, out, "~/.claude-julienning1"), "personal, declined")
	if n := len(h.fake.CallsFor("share")); n != 0 {
		t.Fatalf("%d share calls for a declined account", n)
	}
	if got := h.pendingOf("julienning1"); got != nil {
		t.Fatalf("mark not cleared: %+v", got)
	}
}
