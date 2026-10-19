package dirs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

// Signing a team account in on a machine where no dir holds it yet (a
// second machine): login and new-config both create the dir and sign in,
// pre-filled and checked.

// The second machine as set up: ~/.claude holds claude8, claude2 is shared
// and nothing here held it.
func secondMachine(t *testing.T) (*harness, string) {
	h := newHarness(t)
	def := h.loggedIn(".claude", "claude8@team.io")
	h.initConfig(false, config.ConfigDir{Name: "default", Dir: def})
	h.cacheNicks("claude8@team.io=claude8", "claude2@team.io=claude2")
	return h, def
}

// register adds cd to config.json.
func (h *harness) register(cd config.ConfigDir) {
	h.t.Helper()
	cfg := h.config()
	if err := cfg.Add(cd); err != nil {
		h.t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		h.t.Fatal(err)
	}
}

func TestLoginCreatesADirForATeamAccount(t *testing.T) {
	for _, target := range []string{"claude2", "Claude2@team.io"} {
		t.Run(target, func(t *testing.T) {
			h, _ := secondMachine(t)
			h.loginAs = "claude2@team.io"
			out := h.mustRun(runLogin, target)
			dir := filepath.Join(h.home, ".claude-julienning1")
			if want := []loginCall{{dir, "claude2@team.io"}}; !reflect.DeepEqual(h.logins, want) {
				t.Fatalf("logins = %+v, want %+v", h.logins, want)
			}
			if out != "Signed in: claude2 (claude2@team.io) in ~/.claude-julienning1.\n" {
				t.Errorf("stdout = %q", out)
			}
			contains(t, h.stderr.String(), `julienning: note: no dir here holds claude2 (claude2@team.io); created ~/.claude-julienning1 (config "julienning1") for it`)
			cd, ok := h.config().Find("julienning1")
			if !ok || cd.ShareOnLogin != nil || cd.LastEmail != "claude2@team.io" {
				t.Fatalf("julienning1 = %+v (registered %v), want registered, no pending share, the login recorded", cd, ok)
			}
			assertPatched(t, dir)
			if got, _ := claudecfg.ReadEmail(filepath.Join(h.home, ".claude")); got != "claude8@team.io" {
				t.Fatalf("~/.claude was signed over: %q", got)
			}
		})
	}
}

// Claude args after -- start plain claude in the dir login created.
func TestLoginCreatesADirForClaudeArgs(t *testing.T) {
	h, _ := secondMachine(t)
	h.mustRun(runLogin, "claude2", "--", "--model", "opus")
	if want := []execCall{{dir: filepath.Join(h.home, ".claude-julienning1"), args: []string{"--model", "opus"}}}; !reflect.DeepEqual(h.execs, want) || len(h.logins) != 0 {
		t.Fatalf("execs = %+v, logins = %+v", h.execs, h.logins)
	}
}

// A registered dir nobody ever used (an aborted sign-in, new-config
// --no-login) is signed in before another is created; any trace of use
// keeps it out.
func TestLoginReusesAnUnusedDir(t *testing.T) {
	cases := []struct {
		name   string
		cd     config.ConfigDir
		prep   func(h *harness, dir string)
		reused bool
	}{
		{name: "unused", reused: true},
		{name: "new-config's empty mark", cd: config.ConfigDir{ShareOnLogin: &config.ShareOnLogin{}}, reused: true},
		{name: "made for a new account", cd: config.ConfigDir{ShareOnLogin: &config.ShareOnLogin{Nickname: "delta"}}},
		{name: "held another account", cd: config.ConfigDir{LastEmail: "claude7@team.io"}},
		{name: "has sessions", prep: func(h *harness, dir string) {
			h.transcript(dir, filepath.Join(h.home, "proj"), "11111111-1111-4111-8111-111111111111", "hello")
		}},
		{name: "Claude Code says signed in", prep: func(h *harness, dir string) { h.signedIn = map[string]bool{dir: true} }},
		{name: "deleted by hand", prep: func(h *harness, dir string) {
			if err := os.Remove(dir); err != nil {
				h.t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, def := secondMachine(t)
			spare := h.mkdir(".claude-spare")
			c.cd.Name, c.cd.Dir = "spare", spare
			h.register(c.cd)
			if c.prep != nil {
				c.prep(h, spare)
			}
			h.loginAs = "claude2@team.io"
			h.mustRun(runLogin, "claude2")
			want := filepath.Join(h.home, ".claude-julienning1")
			if c.reused {
				want = spare
				contains(t, h.stderr.String(), "nobody has used ~/.claude-spare yet; signing claude2 (claude2@team.io) in there")
				if _, ok := h.config().Find("julienning1"); ok {
					t.Fatal("created a dir although one was unused")
				}
				// A pending share left in it would have shared whatever
				// account the browser signed in.
				if got := h.pendingOf("spare"); got != nil {
					t.Fatalf("share_on_login = %+v, want it cleared", got)
				}
			}
			if len(h.logins) != 1 || h.logins[0].dir != want || h.logins[0].dir == def {
				t.Fatalf("logins = %+v, want one into %s", h.logins, want)
			}
		})
	}
}

// An unused dir Claude Code cannot be asked about is skipped with a
// warning, not a reason to stop.
func TestLoginSkipsAnUnusedDirItCannotCheck(t *testing.T) {
	h, _ := secondMachine(t)
	h.register(config.ConfigDir{Name: "spare", Dir: h.mkdir(".claude-spare")})
	prev := checkSignedIn
	t.Cleanup(func() { checkSignedIn = prev })
	checkSignedIn = func(context.Context, string) (bool, error) { return false, errors.New("claude timed out") }
	h.loginAs = "claude2@team.io"
	h.mustRun(runLogin, "claude2")
	contains(t, h.stderr.String(), "julienning: warning: cannot tell whether ~/.claude-spare is still signed in (claude timed out); not signing in there")
	if want := []loginCall{{filepath.Join(h.home, ".claude-julienning1"), "claude2@team.io"}}; !reflect.DeepEqual(h.logins, want) {
		t.Fatalf("logins = %+v", h.logins)
	}
}

// A dir julienning does not manage that holds the account is adopted, not
// doubled.
func TestLoginAdoptsRatherThanDoubles(t *testing.T) {
	h, _ := secondMachine(t)
	h.loggedIn(".claude-other", "claude2@team.io") // discoverable, not registered
	err := h.run(runLogin, "claude2")
	if err == nil || err.Error() != "claude2 (claude2@team.io) is signed in on this machine in ~/.claude-other, which julienning does not manage yet; register it with: julienning adopt ~/.claude-other" {
		t.Fatalf("got %v", err)
	}
	if len(h.logins) != 0 || len(h.names()) != 1 {
		t.Fatalf("logins %+v, registered %v", h.logins, h.names())
	}
}

// An email that is not a team account gets no dir from login: new-config
// is where sharing it is decided.
func TestLoginRefusesANewDirForAPersonalEmail(t *testing.T) {
	h, _ := secondMachine(t)
	err := h.run(runLogin, "me@home.io")
	if err == nil || err.Error() != "me@home.io is not shared with the team and not logged in on this machine; sign it into a new dir with `julienning new-config` (shares it; `--no-share` keeps it personal), or into an existing one with `julienning login CONFIG` (see `julienning configs`)" {
		t.Fatalf("got %v", err)
	}
	if len(h.logins) != 0 || len(h.names()) != 1 {
		t.Fatalf("logins %+v, registered %v", h.logins, h.names())
	}
}

// The cached allowlist can predate a teammate's share: login asks the
// Worker once before calling an email personal.
func TestLoginRefreshesTheAllowlistBeforeRefusing(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.initConfig(true)
		h.cacheNicks("claude8@team.io=claude8")
		return h
	}
	t.Run("shared since", func(t *testing.T) {
		h := setup(t)
		h.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: "claude9@team.io", Nickname: "nine"}}}
		h.loginAs = "claude9@team.io"
		out := h.mustRun(runLogin, "claude9@team.io")
		if out != "Signed in: nine (claude9@team.io) in ~/.claude-julienning1.\n" {
			t.Fatalf("stdout %q stderr %q", out, h.stderr.String())
		}
		contains(t, h.stderr.String(), "no dir here holds nine (claude9@team.io)")
	})
	t.Run("refresh fails", func(t *testing.T) {
		h := setup(t)
		h.fake.ListErr = errors.New("connection refused")
		err := h.run(runLogin, "claude9@team.io")
		if err == nil || !strings.HasPrefix(err.Error(), "claude9@team.io is not shared with the team") ||
			!strings.HasSuffix(err.Error(), "(the team allowlist could not be refreshed: list accounts: connection refused)") {
			t.Fatalf("got %v", err)
		}
		if len(h.logins) != 0 || len(h.names()) != 0 {
			t.Fatalf("logins %+v, registered %v", h.logins, h.names())
		}
	})
}

// A team account's nickname, typed (and confirmed) or passed with --nick,
// names the account to sign in: new-config creates the dir and signs it in
// like login.
func TestNewConfigSignsInATeamAccount(t *testing.T) {
	const q = "Nickname for this account [the email's name]: claude2 is the team account claude2@team.io. Sign it in here? [Y/n] "
	for name, run := range map[string]func(h *harness) string{
		"prompt": func(h *harness) string {
			h.interactive = true
			h.stdin = strings.NewReader("Claude2\n\n")
			out := h.mustRun(runNewConfig)
			if !strings.HasPrefix(out, q) {
				t.Fatalf("does not start with the question:\n%s", out)
			}
			return strings.TrimPrefix(out, q)
		},
		"flag": func(h *harness) string { return h.mustRun(runNewConfig, "--nick", "claude2") },
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := secondMachine(t)
			h.loginAs = "claude2@team.io"
			out := run(h)
			want := "Created ~/.claude-julienning1 (config \"julienning1\").\n" +
				"claude2 (claude2@team.io) is already shared with the team; signing it in here.\n" +
				"Signed in: claude2 (claude2@team.io) in ~/.claude-julienning1.\n"
			if out != want {
				t.Fatalf("stdout = %q, want %q", out, want)
			}
			dir := filepath.Join(h.home, ".claude-julienning1")
			if want := []loginCall{{dir, "claude2@team.io"}}; !reflect.DeepEqual(h.logins, want) || len(h.execs) != 0 {
				t.Fatalf("logins = %+v, execs = %+v", h.logins, h.execs)
			}
			if got := h.pendingOf("julienning1"); got != nil {
				t.Fatalf("share_on_login = %+v, want none: the account is shared", got)
			}
			assertPatched(t, dir)
		})
	}
}

// A no keeps the question open: meaning a new nickname and hitting a
// teammate's is not a reason to sign in their account.
func TestNewConfigTeamAccountDeclined(t *testing.T) {
	h, _ := secondMachine(t)
	h.interactive = true
	h.stdin = strings.NewReader("claude2\nn\n Delta \n")
	out := h.mustRun(runNewConfig)
	contains(t, out, "Sign it in here? [Y/n] Then choose another nickname (or press Enter for the email's name).\nNickname for this account")
	if got := h.pendingOf("julienning1"); !reflect.DeepEqual(got, marked("delta")) {
		t.Fatalf("share_on_login = %+v", got)
	}
	if len(h.logins) != 0 || len(h.execs) != 1 {
		t.Fatalf("logins %+v execs %+v", h.logins, h.execs)
	}
}

// The browser signed in another account: new-config signs it out of the
// new dir again, so nothing is shared or recorded and the retry the error
// names signs in there.
func TestNewConfigTeamAccountGotAnotherAccount(t *testing.T) {
	h, _ := secondMachine(t)
	h.loginAs = "stranger@else.io"
	err := h.run(runNewConfig, "--nick", "claude2")
	dir := filepath.Join(h.home, ".claude-julienning1")
	want := "~/.claude-julienning1 was signed in as stranger@else.io, not claude2 (claude2@team.io): the browser was signed into stranger@else.io, so julienning signed it out of ~/.claude-julienning1 again. " +
		"Sign out of claude.ai there (or use a private window) and run: julienning login claude2"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
	if !reflect.DeepEqual(h.logouts, []string{dir}) {
		t.Fatalf("logouts = %+v", h.logouts)
	}
	if cd, _ := h.config().Find("julienning1"); cd.ShareOnLogin != nil || cd.LastEmail != "" {
		t.Fatalf("julienning1 = %+v, want no pending share and no recorded login", cd)
	}
	h.loginAs, h.logins = "claude2@team.io", nil
	h.mustRun(runLogin, "claude2")
	if want := []loginCall{{dir, "claude2@team.io"}}; !reflect.DeepEqual(h.logins, want) {
		t.Fatalf("the retry made another dir: logins = %+v", h.logins)
	}
}

// When the wrong account cannot be signed out, the dir keeps it and says so.
func TestLoginWrongAccountSignOutFails(t *testing.T) {
	h, _ := secondMachine(t)
	h.loginAs = "stranger@else.io"
	h.logoutErr = errors.New("claude auth logout did not finish (exit status 1)")
	err := h.run(runLogin, "claude2")
	if err == nil || !strings.HasPrefix(err.Error(), "~/.claude-julienning1 is now signed in as stranger@else.io, not claude2 (claude2@team.io)") {
		t.Fatalf("got %v", err)
	}
	contains(t, h.stderr.String(), "julienning: warning: could not sign stranger@else.io out of ~/.claude-julienning1 again: claude auth logout did not finish (exit status 1)")
	if cd, _ := h.config().Find("julienning1"); cd.LastEmail != "stranger@else.io" {
		t.Fatalf("what the dir holds is not recorded: %+v", cd)
	}
}

// A dir the user named is never signed out: only one login chose fresh.
func TestLoginNeverSignsOutADirItDidNotChoose(t *testing.T) {
	h := newHarness(t)
	gone := h.mkdir(".claude-gone")
	h.initConfig(false, config.ConfigDir{Name: "gone", Dir: gone, LastEmail: "claude2@team.io"})
	h.cacheNicks("claude2@team.io=claude2")
	h.loginAs = "stranger@else.io"
	if err := h.run(runLogin, "claude2"); err == nil || !strings.Contains(err.Error(), "is now signed in as stranger@else.io") {
		t.Fatalf("got %v", err)
	}
	if len(h.logouts) != 0 {
		t.Fatalf("signed out a dir that held the account: %+v", h.logouts)
	}
}

// A sign-in that does not finish, and --no-login, leave the new dir unused,
// and the login the hint names signs in there.
func TestNewConfigTeamAccountSignInLater(t *testing.T) {
	t.Run("aborted", func(t *testing.T) {
		h, _ := secondMachine(t)
		h.loginErr = errors.New("claude auth login did not finish (exit status 1)")
		err := h.run(runNewConfig, "--nick", "claude2")
		if err == nil || err.Error() != "claude auth login did not finish (exit status 1); ~/.claude-julienning1 is created, sign in later with: julienning login claude2" {
			t.Fatalf("got %v", err)
		}
		h.loginErr, h.loginAs, h.logins = nil, "claude2@team.io", nil
		h.mustRun(runLogin, "claude2")
		if want := []loginCall{{filepath.Join(h.home, ".claude-julienning1"), "claude2@team.io"}}; !reflect.DeepEqual(h.logins, want) {
			t.Fatalf("the retry made another dir: logins = %+v", h.logins)
		}
	})
	t.Run("no-login", func(t *testing.T) {
		h, _ := secondMachine(t)
		out := h.mustRun(runNewConfig, "--nick", "claude2", "--no-login")
		if out != "Created ~/.claude-julienning1 (config \"julienning1\").\nSign in: julienning login claude2\n" || len(h.logins) != 0 || len(h.execs) != 0 {
			t.Fatalf("stdout %q logins %+v execs %+v", out, h.logins, h.execs)
		}
	})
}

// A team account a dir here holds, registered or not, or one a registered
// dir held last and lost to a sign-out, gets no new dir: --nick says where
// it is and creates nothing; the question explains and asks again.
func TestNewConfigTeamAccountAlreadyHere(t *testing.T) {
	cases := []struct {
		name, nick, want string
		prep             func(h *harness)
	}{
		{name: "signed in", nick: "claude8",
			want: "claude8 (claude8@team.io) is already signed in on this machine, in ~/.claude; use it with: julienning use claude8"},
		{name: "unregistered", nick: "claude2",
			want: "claude2 (claude2@team.io) is already signed in on this machine, in ~/.claude-other, which julienning does not manage yet; register it with: julienning adopt ~/.claude-other",
			prep: func(h *harness) { h.loggedIn(".claude-other", "claude2@team.io") }},
		{name: "signed out", nick: "claude2",
			want: "claude2 (claude2@team.io) is signed out on this machine (~/.claude-gone held it last and has no login now); sign in again with: julienning login claude2",
			prep: func(h *harness) {
				h.register(config.ConfigDir{Name: "gone", Dir: h.mkdir(".claude-gone"), LastEmail: "claude2@team.io"})
			}},
	}
	for _, c := range cases {
		t.Run(c.name+" --nick", func(t *testing.T) {
			h, _ := secondMachine(t)
			if c.prep != nil {
				c.prep(h)
			}
			before := h.names()
			err := h.run(runNewConfig, "--nick", c.nick)
			if err == nil || err.Error() != c.want {
				t.Fatalf("got %v\nwant %s", err, c.want)
			}
			if !reflect.DeepEqual(h.names(), before) || len(h.logins) != 0 {
				t.Fatalf("registered %v, logins %+v", h.names(), h.logins)
			}
			if _, err := os.Stat(filepath.Join(h.home, ".claude-julienning1")); !os.IsNotExist(err) {
				t.Fatalf("a dir was created: %v", err)
			}
		})
		t.Run(c.name+" asked", func(t *testing.T) {
			h, _ := secondMachine(t)
			if c.prep != nil {
				c.prep(h)
			}
			h.interactive = true
			h.stdin = strings.NewReader(c.nick + "\ny\n Delta \n")
			out := h.mustRun(runNewConfig)
			contains(t, out, "Sign it in here? [Y/n] "+c.want+"\nOr choose another nickname (or press Enter for the email's name).\nNickname for this account")
			if got := h.pendingOf("julienning1"); !reflect.DeepEqual(got, marked("delta")) {
				t.Fatalf("share_on_login = %+v", got)
			}
		})
	}
}
