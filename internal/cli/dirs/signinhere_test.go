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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, def := secondMachine(t)
			spare := h.mkdir(".claude-spare")
			c.cd.Name, c.cd.Dir = "spare", spare
			cfg := h.config()
			if err := cfg.Add(c.cd); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
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
			}
			if len(h.logins) != 1 || h.logins[0].dir != want || h.logins[0].dir == def {
				t.Fatalf("logins = %+v, want one into %s", h.logins, want)
			}
		})
	}
}

func TestLoginCannotTellWhetherAnUnusedDirIsSignedIn(t *testing.T) {
	h, _ := secondMachine(t)
	spare := h.mkdir(".claude-spare")
	cfg := h.config()
	if err := cfg.Add(config.ConfigDir{Name: "spare", Dir: spare}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	prev := checkSignedIn
	t.Cleanup(func() { checkSignedIn = prev })
	checkSignedIn = func(context.Context, string) (bool, error) { return false, errors.New("claude timed out") }
	err := h.run(runLogin, "claude2")
	if err == nil || err.Error() != "cannot tell whether ~/.claude-spare is still signed in (claude timed out); sign in there yourself with: julienning login spare" || len(h.logins) != 0 {
		t.Fatalf("err %v logins %+v", err, h.logins)
	}
}

// An email that is not a team account gets no dir from login: new-config
// is where sharing it is decided.
func TestLoginRefusesANewDirForAPersonalEmail(t *testing.T) {
	h, _ := secondMachine(t)
	err := h.run(runLogin, "me@home.io")
	if err == nil || !strings.HasPrefix(err.Error(), "me@home.io is not shared with the team and not logged in on this machine; sign it into a new dir with `julienning new-config` (shares it; `--no-share` keeps it personal)") {
		t.Fatalf("got %v", err)
	}
	if len(h.logins) != 0 || len(h.names()) != 1 {
		t.Fatalf("logins %+v, registered %v", h.logins, h.names())
	}
}

// A team account's nickname, typed or passed with --nick, names the account
// to sign in: new-config creates the dir and signs it in like login.
func TestNewConfigSignsInATeamAccount(t *testing.T) {
	for name, run := range map[string]func(h *harness) string{
		"prompt": func(h *harness) string {
			h.interactive = true
			h.stdin = strings.NewReader("Claude2\n")
			return h.mustRun(runNewConfig)
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
			if got := strings.TrimPrefix(out, "Nickname for this account [the email's name]: "); got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
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

// The browser signed in another account: the error says so, and that
// account is not shared behind the user's back (no pending share).
func TestNewConfigTeamAccountGotAnotherAccount(t *testing.T) {
	h, _ := secondMachine(t)
	h.loginAs = "stranger@else.io"
	err := h.run(runNewConfig, "--nick", "claude2")
	if err == nil || !strings.Contains(err.Error(), "~/.claude-julienning1 is now signed in as stranger@else.io, not claude2 (claude2@team.io)") {
		t.Fatalf("got %v", err)
	}
	if got := h.pendingOf("julienning1"); got != nil {
		t.Fatalf("share_on_login = %+v, want none", got)
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

// A team account a dir here holds, or held last and lost to a sign-out, gets
// no new dir: new-config says where it is, and nothing is created.
func TestNewConfigTeamAccountAlreadyHere(t *testing.T) {
	t.Run("signed in", func(t *testing.T) {
		h, _ := secondMachine(t)
		err := h.run(runNewConfig, "--nick", "claude8")
		if err == nil || err.Error() != "claude8 (claude8@team.io) is already signed in on this machine, in ~/.claude; use it with: julienning use claude8" {
			t.Fatalf("got %v", err)
		}
		if len(h.names()) != 1 || len(h.logins) != 0 {
			t.Fatalf("registered %v, logins %+v", h.names(), h.logins)
		}
		if _, err := os.Stat(filepath.Join(h.home, ".claude-julienning1")); !os.IsNotExist(err) {
			t.Fatalf("a dir was created: %v", err)
		}
	})
	t.Run("signed out", func(t *testing.T) {
		h, _ := secondMachine(t)
		gone := h.mkdir(".claude-gone")
		cfg := h.config()
		if err := cfg.Add(config.ConfigDir{Name: "gone", Dir: gone, LastEmail: "claude2@team.io"}); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		h.interactive = true
		h.stdin = strings.NewReader("claude2\n")
		err := h.run(runNewConfig)
		if err == nil || err.Error() != "claude2 (claude2@team.io) is signed out on this machine (~/.claude-gone held it last and has no login now); sign in again with: julienning login claude2" {
			t.Fatalf("got %v", err)
		}
		if len(h.names()) != 2 {
			t.Fatalf("registered %v", h.names())
		}
	})
}
