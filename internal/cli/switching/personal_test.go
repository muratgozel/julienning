package switching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/tui"
)

// personalDir logs an unregistered dir (relative to HOME) into email, the
// way Claude Code leaves it; ".claude" is the default dir, whose login lives
// in ~/.claude.json.
func (f *fixture) personalDir(rel, email string) config.ConfigDir {
	f.t.Helper()
	dir := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	acct := filepath.Join(dir, ".claude.json")
	if rel == ".claude" {
		acct = filepath.Join(f.home, ".claude.json")
	}
	body := `{"oauthAccount":{}}`
	if email != "" {
		body = `{"oauthAccount":{"emailAddress":"` + email + `"}}`
	}
	if err := os.WriteFile(acct, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return config.ConfigDir{Dir: dir}
}

// noCallsFor fails when the fake Worker saw any request about email.
func (f *fixture) noCallsFor(email string) {
	f.t.Helper()
	for _, c := range f.fake.Calls {
		if strings.EqualFold(c.Email, email) {
			f.t.Errorf("the Worker was told about %s: %+v", email, c)
		}
	}
}

const personalLine = "Using me@personal.io in ~/.claude (personal; your selection is unchanged).\n"

// personalFixture: two team accounts (julienning3 selected) and a personal
// account in the unregistered default dir, each with a session in f.proj.
func personalFixture(t *testing.T) (f *fixture, team, def config.ConfigDir) {
	f, team, other := launchFixture(t)
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28), nacct(email2, "beta", 50, 60))
	def = f.personalDir(".claude", personal)
	f.setCurrent(team)
	f.writeSession(team, sidA, "Team here", "a", time.Hour)
	f.writeSession(other, sidB, "Team there", "b", 2*time.Hour)
	f.writeSession(def, sidC, "Personal work", "c", 3*time.Hour)
	return f, team, def
}

func TestUsePersonalWithoutTerminal(t *testing.T) {
	f, team, def := personalFixture(t)
	f.tty = false
	got := f.run("use", "Me@Personal.io")
	assertCode(t, got, 0)
	if got.stdout != personalLine || got.stderr != "" {
		t.Errorf("stdout %q stderr %q", got.stdout, got.stderr)
	}
	if f.current() != team.Dir {
		t.Errorf("the selection moved to %q", f.current())
	}
	if len(f.execs) != 0 {
		t.Errorf("execs = %+v", f.execs)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := cfg.PersonalDirOf(personal); !ok || d != def.Dir {
		t.Errorf("not remembered: %q %v", d, ok)
	}
	if len(cfg.Configs) != 2 {
		t.Errorf("the personal dir must not be registered: %+v", cfg.Configs)
	}
	f.noCallsFor(personal)

	got = f.run("use", personal, "--json")
	assertCode(t, got, 0)
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["config"] != nil || doc["email"] != personal || doc["nickname"] != nil || doc["dir"] != def.Dir || doc["personal"] != true {
		t.Errorf("json = %v", doc)
	}
}

// The personal picker lists that account's sessions only, resumes in place
// and never asks to move anything; the selection stays where it was.
func TestUsePersonalPicker(t *testing.T) {
	f, team, def := personalFixture(t)
	f.pick = func([]tui.Item) (int, error) { return 1, nil }
	got := f.run("use", personal)
	assertCode(t, got, 0)
	if f.pickHdr != strings.TrimSuffix(personalLine, "\n") {
		t.Errorf("header = %q", f.pickHdr)
	}
	if len(f.pickRows) != 2 || f.pickRows[0].Meta != "in me@personal.io" || f.pickRows[1].Title != "Personal work" ||
		f.pickRows[1].Meta != "me@personal.io · 3h ago · 2d9b8e30" {
		t.Fatalf("rows = %+v", f.pickRows)
	}
	if len(f.execs) != 1 || f.execs[0].dir != def.Dir || strings.Join(f.execs[0].args, " ") != "--resume "+sidC || f.execs[0].chdir != f.proj {
		t.Errorf("execs = %+v", f.execs)
	}
	if len(f.confirms) != 0 || f.current() != team.Dir {
		t.Errorf("confirms %+v current %q", f.confirms, f.current())
	}

	f.execs = nil
	f.pick = func([]tui.Item) (int, error) { return 0, nil }
	assertCode(t, f.run("use", personal), 0)
	if len(f.execs) != 1 || f.execs[0].dir != def.Dir || len(f.execs[0].args) != 0 {
		t.Errorf("new session: execs = %+v", f.execs)
	}

	f.execs = nil
	f.pick = func([]tui.Item) (int, error) { return 0, tui.ErrCanceled }
	got = f.run("use", personal)
	assertCode(t, got, 0)
	if !strings.HasSuffix(got.stdout, "Not starting claude; your selection is unchanged.\n") || len(f.execs) != 0 {
		t.Errorf("cancel: stdout %q execs %+v", got.stdout, f.execs)
	}
	if f.current() != team.Dir {
		t.Errorf("current = %q", f.current())
	}
	f.noCallsFor(personal)
}

// A personal account logged into a registered dir: by email it takes the
// personal path (its own sessions, no selection change); by config name it
// keeps the old behaviour (selected, plain launch, warning).
func TestUsePersonalInRegisteredDir(t *testing.T) {
	f, team, _ := launchFixture(t)
	mine := f.addConfig("mine", personal)
	f.save()
	f.setCurrent(team)
	f.writeSession(team, sidA, "Team here", "a", time.Hour)
	f.writeSession(mine, sidC, "Personal work", "c", 3*time.Hour)

	f.pick = func([]tui.Item) (int, error) { return 0, tui.ErrCanceled }
	got := f.run("use", personal)
	assertCode(t, got, 0)
	if len(f.pickRows) != 2 || f.pickRows[1].Title != "Personal work" || f.current() != team.Dir {
		t.Fatalf("rows %+v current %q stdout %q", f.pickRows, f.current(), got.stdout)
	}
	got = f.run("use", personal, "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil || doc["config"] != "mine" {
		t.Errorf("json %q (%v)", got.stdout, err)
	}

	f.pickRows = nil
	got = f.run("use", "mine")
	assertCode(t, got, 0)
	if f.current() != mine.Dir || f.pickRows != nil || !strings.Contains(got.stderr, "not shared with the team") {
		t.Errorf("config name: current %q rows %v stderr %q", f.current(), f.pickRows, got.stderr)
	}
}

func TestUsePersonalErrors(t *testing.T) {
	f, team, _ := personalFixture(t)
	f.tty = false
	got := f.run("use", "nobody@example.com")
	assertCode(t, got, cli.ExitError)
	if !strings.Contains(got.stderr, "no Claude config dir on this machine is logged in as nobody@example.com") ||
		!strings.Contains(got.stderr, "CLAUDE_CONFIG_DIR=~/.claude-nobody command claude") {
		t.Errorf("stderr = %q", got.stderr)
	}
	got = f.run("use", "not an@email")
	assertCode(t, got, cli.ExitError)
	if !strings.Contains(got.stderr, "is not a valid email address") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if f.current() != team.Dir {
		t.Errorf("current = %q", f.current())
	}
	// A team account keeps the team path and its error.
	f.fake.Listing = listing(nacct(email1, "alpha", 1, 1), nacct("claude7@x.io", "seven", 1, 1))
	got = f.run("use", "claude7@x.io")
	assertCode(t, got, cli.ExitError)
	if !strings.Contains(got.stderr, "seven (claude7@x.io) is shared but not logged in on this machine") {
		t.Errorf("stderr = %q", got.stderr)
	}
}
