package switching

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/tui"
)

// --- next: target selection and non-launch output -----------------------

func TestNextPicksBestLocalAccount(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("julienning2", email2)
	f.addConfig("personal", personal)
	f.save()
	// email1 ranks first but has no dir here.
	f.fake.Listing = listing(acct(email1, 12, 28), acct(email2, 50, 60))

	got := f.run("next")
	assertCode(t, got, 0)
	if want := "Now using julienning2 (claude2@x.io) — session 50% → 20:00, week 60% → Fri 20:00.\n"; got.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", got.stdout, want)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q", got.stderr)
	}
	if f.current() != cd.Dir {
		t.Errorf("current = %q", f.current())
	}
	if ops := f.fake.Ops(); len(ops) == 0 || ops[len(ops)-1] != "list" {
		t.Errorf("ops = %v", ops)
	}
	cache, err := sharedcache.Load()
	if err != nil || !cache.Contains(email1) || !cache.Contains(email2) || cache.Contains(personal) {
		t.Errorf("shared cache not refreshed from the listing: %+v %v", cache, err)
	}
	if len(f.execs) != 0 {
		t.Error("non-interactive runs must not launch claude")
	}
}

func TestNextStaysOnCurrent(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("julienning1", email1)
	f.save()
	f.setCurrent(cd)
	f.fake.Listing = listing(acct(email1, 12, 28))
	got := f.run("next")
	assertCode(t, got, 0)
	if want := "Staying on julienning1 (best available) — session 12% → 20:00, week 28% → Fri 20:00.\n"; got.stdout != want {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestNextPrefersCurrentDirForTheSameEmail(t *testing.T) {
	f := setup(t)
	f.addConfig("a", email1)
	b := f.addConfig("b", email1)
	f.save()
	f.fake.Listing = listing(acct(email1, 12, 28))

	got := f.run("next")
	assertCode(t, got, 0)
	if !strings.HasPrefix(got.stdout, "Now using a (") {
		t.Errorf("without a selection the first name wins: %q", got.stdout)
	}
	f.setCurrent(b)
	got = f.run("next")
	assertCode(t, got, 0)
	if !strings.HasPrefix(got.stdout, "Staying on b ") || f.current() != b.Dir {
		t.Errorf("current dir must win: %q", got.stdout)
	}
}

// Accounts that rank level except for the email tie-breaker do not trigger a
// switch away from the current one.
func TestNextStaysOnCurrentWhenTied(t *testing.T) {
	f := setup(t)
	f.addConfig("one", email1)
	two := f.addConfig("two", email2)
	f.save()
	f.setCurrent(two)
	f.fake.Listing = listing(acct(email1, 0, 0), acct(email2, 0, 0))
	got := f.run("next")
	assertCode(t, got, 0)
	if !strings.HasPrefix(got.stdout, "Staying on two ") {
		t.Errorf("stdout = %q", got.stdout)
	}
	// A real difference moves.
	f.fake.Listing = listing(acct(email1, 0, 0), acct(email2, 5, 0))
	got = f.run("next")
	assertCode(t, got, 0)
	if !strings.HasPrefix(got.stdout, "Now using one ") {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestNextWarnsWhenOthersUseIt(t *testing.T) {
	f := setup(t)
	f.addConfig("one", email1)
	f.save()
	a := acct(email1, 10, 10)
	a.BusyBy = []string{"ali", "can"}
	f.fake.Listing = listing(a)
	got := f.run("next")
	assertCode(t, got, 0)
	if !strings.Contains(got.stderr, "claude1@x.io is also in use by ali, can") {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestNextJSON(t *testing.T) {
	f := setup(t)
	f.addConfig("one", email1)
	f.save()
	f.fake.Listing = listing(acct(email1, 12, 28))
	f.fake.Listing.Accounts[0].Raw = json.RawMessage(`{"email":"claude1@x.io","state":"free","future_field":7}`)
	f.tty = true // --json never launches
	got := f.run("next", "--json")
	assertCode(t, got, 0)
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("%v: %q", err, got.stdout)
	}
	if doc["config"] != "one" || doc["email"] != email1 || doc["future_field"] != float64(7) {
		t.Errorf("doc = %v", doc)
	}
	if !strings.Contains(got.stdout, "\n  \"") || len(f.execs) != 0 {
		t.Errorf("want indented JSON and no launch: %q", got.stdout)
	}
}

func TestNextErrors(t *testing.T) {
	t.Run("worker failure", func(t *testing.T) {
		f := setup(t)
		f.addConfig("one", email1)
		f.save()
		f.fake.ListErr = &remote.Error{Status: 401, Message: "unauthorized"}
		got := f.run("next")
		assertCode(t, got, cli.ExitError)
		if !strings.Contains(got.stderr, "cannot rank accounts (401 unauthorized); pick manually with `julienning use NICKNAME`") {
			t.Errorf("stderr = %q", got.stderr)
		}
		if f.current() != "" {
			t.Error("must not switch")
		}
	})
	t.Run("no local account", func(t *testing.T) {
		f := setup(t)
		f.addConfig("fresh", "")
		f.addConfig("personal", personal)
		f.save()
		f.fake.Listing = listing(acct(email1, 1, 1))
		got := f.run("next")
		assertCode(t, got, cli.ExitError)
		if !strings.Contains(got.stderr, email1) || !strings.Contains(got.stderr, "julienning login NICKNAME") {
			t.Errorf("stderr = %q", got.stderr)
		}
	})
	t.Run("empty allowlist", func(t *testing.T) {
		f := setup(t)
		f.addConfig("one", email1)
		f.save()
		got := f.run("next")
		assertCode(t, got, cli.ExitError)
		if !strings.Contains(got.stderr, "julienning share EMAIL") {
			t.Errorf("stderr = %q", got.stderr)
		}
	})
	t.Run("remote not configured", func(t *testing.T) {
		f := setup(t)
		f.cfg.Remote = config.Remote{}
		f.addConfig("one", email1)
		f.save()
		got := f.run("next")
		assertCode(t, got, cli.ExitError)
		if !strings.Contains(got.stderr, "not configured") || len(f.fake.Calls) != 0 {
			t.Errorf("stderr = %q calls %v", got.stderr, f.fake.Calls)
		}
	})
	t.Run("usage", func(t *testing.T) {
		f := setup(t)
		f.save()
		for _, args := range [][]string{{"next", "x"}, {"next", "--limit", "0"}, {"next", "--bogus"}} {
			assertCode(t, f.run(args...), cli.ExitUsage)
		}
	})
	t.Run("not set up", func(t *testing.T) {
		f := setup(t)
		got := f.run("next")
		assertCode(t, got, cli.ExitError)
		if !strings.Contains(got.stderr, "julienning setup") {
			t.Errorf("stderr = %q", got.stderr)
		}
	})
}

// --- use -----------------------------------------------------------------

func TestUseSwitchesWithUsageLine(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("julienning1", email1)
	f.save()
	f.fake.Listing = listing(acct(email1, 12, 28))
	got := f.run("use", "julienning1")
	assertCode(t, got, 0)
	if want := "Now using julienning1 (claude1@x.io) — session 12% → 20:00, week 28% → Fri 20:00.\n"; got.stdout != want {
		t.Errorf("stdout = %q", got.stdout)
	}
	if got.stderr != "" || f.current() != cd.Dir {
		t.Errorf("stderr %q current %q", got.stderr, f.current())
	}
	for _, op := range f.fake.Ops() {
		if op == "claim" || op == "unclaim" {
			t.Errorf("use must not claim any more (claims follow sessions): %v", f.fake.Ops())
		}
	}
	// Flags may follow the name.
	got = f.run("use", "julienning1", "--json")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, `"config": "julienning1"`) {
		t.Errorf("json = %q", got.stdout)
	}
}

func TestUseNotLoggedIn(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("fresh", "")
	f.save()
	got := f.run("use", "fresh")
	assertCode(t, got, 0)
	if got.stdout != "Now using fresh.\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "fresh is not logged in yet; run: julienning login fresh") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if f.current() != cd.Dir {
		t.Error("switch must still happen")
	}
	got = f.run("use", "fresh", "--json")
	assertCode(t, got, 0)
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil || doc["config"] != "fresh" || doc["email"] != nil {
		t.Errorf("json = %q %v", got.stdout, err)
	}
}

func TestUseNotShared(t *testing.T) {
	f := setup(t)
	f.addConfig("personal", personal)
	f.save()
	f.fake.Listing = listing(acct(email1, 1, 1))
	got := f.run("use", "personal")
	assertCode(t, got, 0)
	if got.stdout != "Now using personal (me@personal.io).\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "not shared with the team") {
		t.Errorf("stderr = %q", got.stderr)
	}
}

// Offline, `use` still switches and judges sharing from the cached list.
func TestUseOffline(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("one", email1)
	f.save()
	if _, err := sharedcache.Save([]string{email1}, nowT); err != nil {
		t.Fatal(err)
	}
	f.fake.ListErr = errors.New("request timed out after 10s")
	got := f.run("use", "one")
	assertCode(t, got, 0)
	if got.stdout != "Now using one (claude1@x.io).\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "could not reach the Worker (request timed out after 10s); using the cached shared-account list") ||
		strings.Contains(got.stderr, "not shared") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if f.current() != cd.Dir {
		t.Error("current")
	}
}

func TestUseClear(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("one", email1)
	f.save()
	f.setCurrent(cd)
	got := f.run("use", "--clear")
	assertCode(t, got, 0)
	if got.stdout != "Selection cleared; plain claude uses your default config.\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if f.current() != "" {
		t.Error("current must be removed")
	}
	assertCode(t, f.run("use", "--clear"), 0) // idempotent
	assertCode(t, f.run("use", "--clear", "one"), cli.ExitUsage)
	assertCode(t, f.run("use", "--clear", "--json"), cli.ExitUsage)
}

func TestUseErrors(t *testing.T) {
	f := setup(t)
	f.addConfig("one", email1)
	f.save()
	got := f.run("use", "nope")
	assertCode(t, got, cli.ExitError)
	if want := "julienning: unknown target \"nope\": not a team nickname, an email, or a config name (see `julienning accounts` and `julienning configs`)\n"; got.stderr != want {
		t.Errorf("stderr = %q\nwant     %q", got.stderr, want)
	}
	// The target is resolved against one fresh listing, and nothing else
	// is contacted.
	if ops := strings.Join(f.fake.Ops(), ","); ops != "list" {
		t.Errorf("ops = %s", ops)
	}
	assertCode(t, f.run("use", "a", "b"), cli.ExitUsage)
	assertCode(t, f.run("use", " "), cli.ExitUsage)
	assertCode(t, f.run("use", "one", "--limit", "-3"), cli.ExitUsage)
	if f.current() != "" {
		t.Error("errors must not switch")
	}
}

// --- launch mode -----------------------------------------------------------

func launchFixture(t *testing.T) (f *fixture, target, other config.ConfigDir) {
	f = setup(t)
	f.tty = true
	target = f.addConfig("julienning3", email1)
	other = f.addConfig("sixtynine", email2)
	f.save()
	f.fake.Listing = listing(acct(email1, 12, 28), acct(email2, 50, 60))
	return f, target, other
}

func TestLaunchNewSession(t *testing.T) {
	f, target, other := launchFixture(t)
	f.writeSession(other, sidA, "Checkout fix", "fix checkout", 2*time.Hour)
	f.pick = func([]tui.Item) (int, error) { return 0, nil }
	got := f.run("next")
	assertCode(t, got, 0)
	if len(f.execs) != 1 || f.execs[0].dir != target.Dir || len(f.execs[0].args) != 0 || f.execs[0].chdir != "" {
		t.Fatalf("execs = %+v", f.execs)
	}
	if !strings.HasPrefix(f.pickHdr, "Now using julienning3 (claude1@x.io) — session 12%") {
		t.Errorf("header = %q", f.pickHdr)
	}
}

func TestLaunchPickerRows(t *testing.T) {
	f, target, other := launchFixture(t)
	f.writeSession(other, sidA, "Checkout fix", "fix   the\ncheckout", 2*time.Hour)
	f.writeSession(target, sidB, "", "untitled prompt", 3*24*time.Hour)
	f.writeSession(other, sidC, "Old work", "old prompt", 10*24*time.Hour)
	if err := os.WriteFile(filepath.Join(other.Dir, "sessions", "4242.json"),
		[]byte(`{"pid":4242,"sessionId":"`+sidC+`","cwd":"/x","status":"idle","kind":"interactive"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.pick = func([]tui.Item) (int, error) { return 0, tui.ErrCanceled }
	got := f.run("next")
	assertCode(t, got, 0)
	if len(f.execs) != 0 {
		t.Fatal("cancel must not launch")
	}
	if !strings.Contains(got.stdout, "Not starting claude; julienning3 stays selected.") || f.current() != target.Dir {
		t.Errorf("stdout = %q", got.stdout)
	}
	rows := f.pickRows
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; r.Title != "New session" || !r.Pinned || r.Disabled {
		t.Errorf("row 0 = %+v", r)
	}
	if r := rows[1]; r.Title != "Checkout fix" || r.Meta != "sixtynine · 2h ago · 0b7f6c1e" || r.Detail != "fix the checkout" || r.Disabled {
		t.Errorf("row 1 = %+v", r)
	}
	if r := rows[2]; r.Title != "untitled prompt" || r.Meta != "julienning3 · 3d ago · 1c8a7d2f" || r.Detail != "" {
		t.Errorf("row 2 = %+v", r)
	}
	// Older than six days: absolute local time (Istanbul, +03:00).
	if r := rows[3]; r.Meta != "sixtynine · 2026-09-05 17:30 · 2d9b8e30" || !r.Disabled || r.Note != "(open in another terminal)" {
		t.Errorf("row 3 = %+v", r)
	}
}

func TestLaunchResumeInPlace(t *testing.T) {
	f, target, _ := launchFixture(t)
	f.writeSession(target, sidB, "Here already", "p", time.Hour)
	f.pick = func([]tui.Item) (int, error) { return 1, nil }
	got := f.run("next")
	assertCode(t, got, 0)
	want := execCall{target.Dir, []string{"--resume", sidB}, f.proj}
	if len(f.execs) != 1 || f.execs[0].dir != want.dir || strings.Join(f.execs[0].args, " ") != "--resume "+sidB || f.execs[0].chdir != want.chdir {
		t.Fatalf("execs = %+v, want %+v", f.execs, want)
	}
	if strings.Contains(got.stdout, "Moved") {
		t.Error("nothing to move")
	}
}

func TestLaunchMovesThenResumes(t *testing.T) {
	f, target, other := launchFixture(t)
	src := f.writeSession(other, sidA, "Checkout fix", "fix checkout", 2*time.Hour)
	mem := filepath.Join(other.Dir, "projects", sessions.EncodeCwd(f.proj), "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(mem), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("- [Deploy](deploy.md)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.pick = func([]tui.Item) (int, error) { return 1, nil }
	f.confirm = acceptMoves

	got := f.run("next")
	assertCode(t, got, 0)
	if len(f.confirms) != 1 || f.confirms[0].question != `Move "Checkout fix" from sixtynine to julienning3?` {
		t.Errorf("confirms = %+v", f.confirms)
	}
	if !strings.Contains(got.stdout, `Moved "Checkout fix" from sixtynine to julienning3 (memory: 1 file added).`) {
		t.Errorf("stdout = %q", got.stdout)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Error("source transcript must be gone")
	}
	moved := filepath.Join(target.Dir, "projects", sessions.EncodeCwd(f.proj), sidA+".jsonl")
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("transcript not in target: %v", err)
	}
	if len(f.execs) != 1 || strings.Join(f.execs[0].args, " ") != "--resume "+sidA || f.execs[0].dir != target.Dir {
		t.Fatalf("execs = %+v", f.execs)
	}
	// The report comes before claude takes over the terminal: exec is last.
	if idx := strings.Index(got.stdout, "Moved"); idx < 0 {
		t.Error("report missing")
	}
}

func TestLaunchMoveRefusalDoesNotLaunch(t *testing.T) {
	f, target, other := launchFixture(t)
	f.writeSession(other, sidA, "Checkout fix", "fix checkout", 2*time.Hour)
	// The target already has that id in another project.
	dup := filepath.Join(target.Dir, "projects", "-elsewhere", sidA+".jsonl")
	if err := os.MkdirAll(filepath.Dir(dup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dup, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.pick = func([]tui.Item) (int, error) { return 1, nil }
	f.confirm = acceptMoves
	got := f.run("next")
	assertCode(t, got, cli.ExitError)
	if !strings.Contains(got.stderr, "julienning3 already has a session with id "+sidA) || len(f.execs) != 0 {
		t.Errorf("stderr = %q execs %v", got.stderr, f.execs)
	}
}

func TestLaunchWithoutSessionsStartsFresh(t *testing.T) {
	f, target, _ := launchFixture(t)
	got := f.run("use", "julienning3")
	assertCode(t, got, 0)
	if len(f.execs) != 1 || f.execs[0].dir != target.Dir || len(f.execs[0].args) != 0 || f.pickRows != nil {
		t.Fatalf("execs = %+v rows %v", f.execs, f.pickRows)
	}
}

// A personal or not-logged-in target launches plain claude: team sessions
// are never offered for moving into it.
func TestLaunchPlainForUnsharedTargets(t *testing.T) {
	for _, email := range []string{personal, ""} {
		f, _, other := launchFixture(t)
		cd := f.addConfig("mine", email)
		f.save()
		f.writeSession(other, sidA, "Checkout fix", "fix", time.Hour)
		got := f.run("use", "mine")
		assertCode(t, got, 0)
		if len(f.execs) != 1 || f.execs[0].dir != cd.Dir || len(f.execs[0].args) != 0 || f.pickRows != nil {
			t.Errorf("email %q: execs %+v rows %v", email, f.execs, f.pickRows)
		}
		if got.stderr == "" {
			t.Errorf("email %q: want a warning", email)
		}
	}
}

func TestNoLaunchFlag(t *testing.T) {
	f, _, other := launchFixture(t)
	f.writeSession(other, sidA, "Checkout fix", "fix", time.Hour)
	got := f.run("next", "--no-launch")
	assertCode(t, got, 0)
	if len(f.execs) != 0 || f.pickRows != nil || !strings.HasPrefix(got.stdout, "Now using julienning3") {
		t.Errorf("stdout %q execs %v", got.stdout, f.execs)
	}
}

// The default dir is a regular target: sessions land in ~/.claude/projects
// and claude is launched for it (launch.Env unsets CLAUDE_CONFIG_DIR).
func TestLaunchIntoDefaultDir(t *testing.T) {
	f := setup(t)
	f.tty = true
	def := f.addConfig("default", email1)
	other := f.addConfig("sixtynine", email2)
	f.save()
	f.fake.Listing = listing(acct(email1, 1, 1), acct(email2, 50, 60))
	f.writeSession(other, sidA, "Checkout fix", "fix", time.Hour)
	f.pick = func([]tui.Item) (int, error) { return 1, nil }
	f.confirm = acceptMoves
	got := f.run("use", "default")
	assertCode(t, got, 0)
	if !strings.HasPrefix(got.stdout, "Now using default (claude1@x.io)") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if _, err := os.Stat(filepath.Join(f.home, ".claude", "projects", sessions.EncodeCwd(f.proj), sidA+".jsonl")); err != nil {
		t.Errorf("not moved into ~/.claude: %v", err)
	}
	if len(f.execs) != 1 || f.execs[0].dir != def.Dir {
		t.Errorf("execs = %+v", f.execs)
	}
}

func TestLaunchAllListsOtherProjects(t *testing.T) {
	f, target, other := launchFixture(t)
	f.writeSession(other, sidA, "Here", "p", time.Hour)
	elsewhere := f.proj
	f.proj = filepath.Join(f.home, "Code", "api")
	if err := os.MkdirAll(f.proj, 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeSession(target, sidB, "API work", "p", 2*time.Hour)
	f.proj = elsewhere
	f.pick = func([]tui.Item) (int, error) { return 0, tui.ErrCanceled }

	assertCode(t, f.run("next"), 0)
	if len(f.pickRows) != 2 {
		t.Fatalf("current project only: %+v", f.pickRows)
	}
	// f.proj is symlink-free (/private/var on macOS) while HOME may not be,
	// so the expected path goes through the same shortening.
	if f.pickScope != "1 session in "+shortenHome(f.proj) {
		t.Errorf("scope = %q", f.pickScope)
	}
	assertCode(t, f.run("next", "--all", "--limit", "5"), 0)
	if len(f.pickRows) != 3 || !strings.Contains(f.pickRows[2].Meta, "~/Code/api") {
		t.Fatalf("--all: %+v", f.pickRows)
	}
	if f.pickScope != "2 sessions" {
		t.Errorf("--all scope = %q", f.pickScope)
	}
	assertCode(t, f.run("next", "--all", "--limit", "2"), 0)
	if f.pickScope != "2 most recent sessions" {
		t.Errorf("--all --limit 2 scope = %q", f.pickScope)
	}
}

// Without --limit the picker offers up to 100 sessions; the scope line says
// when the limit cut the list.
func TestLaunchDefaultLimitIs100(t *testing.T) {
	f, target, other := launchFixture(t)
	for i := 0; i < 103; i++ {
		cd := target
		if i%2 == 1 {
			cd = other
		}
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i)
		f.writeSession(cd, id, fmt.Sprintf("Session %03d", i), "p", time.Duration(i+1)*time.Minute)
	}
	f.pick = func([]tui.Item) (int, error) { return 0, tui.ErrCanceled }
	assertCode(t, f.run("next"), 0)
	if len(f.pickRows) != 101 { // New session + 100
		t.Fatalf("%d rows, want 101", len(f.pickRows))
	}
	if f.pickRows[1].Title != "Session 000" || f.pickRows[100].Title != "Session 099" {
		t.Errorf("rows 1 and 100: %q, %q", f.pickRows[1].Title, f.pickRows[100].Title)
	}
	if f.pickScope != "100 most recent sessions in "+shortenHome(f.proj) {
		t.Errorf("scope = %q", f.pickScope)
	}
	assertCode(t, f.run("next", "--limit", "500"), 0)
	if len(f.pickRows) != 104 || f.pickScope != "103 sessions in "+shortenHome(f.proj) {
		t.Errorf("--limit 500: %d rows, scope %q", len(f.pickRows), f.pickScope)
	}
	assertCode(t, f.run("next", "--limit", "501"), cli.ExitUsage)
}

func TestScopeLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shop := filepath.Join(home, "Code", "shop")
	cases := []struct {
		n, limit int
		all      bool
		cwd      string
		want     string
	}{
		{100, 100, false, shop, "100 most recent sessions in ~/Code/shop"},
		{37, 100, false, shop, "37 sessions in ~/Code/shop"},
		{1, 100, false, shop, "1 session in ~/Code/shop"},
		{1, 1, false, shop, "1 most recent session in ~/Code/shop"},
		{100, 100, true, shop, "100 most recent sessions"},
		{5, 100, true, shop, "5 sessions"},
		{5, 100, false, "", "5 sessions"}, // cwd unknown: every project was listed
		{2, 10, false, "/srv/app", "2 sessions in /srv/app"},
	}
	for _, tc := range cases {
		if got := scopeLine(tc.n, tc.limit, tc.all, tc.cwd); got != tc.want {
			t.Errorf("scopeLine(%d, %d, %v, %q) = %q, want %q", tc.n, tc.limit, tc.all, tc.cwd, got, tc.want)
		}
	}
}

func TestPromptPreview(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fix   the\ncheckout", "fix the checkout"},
		{"> quoted request", "quoted request"},
		{">> nested > quote", "nested > quote"}, // only the leading marker is quoting
		{">no space", "no space"},
		{"```go func main() {} ``` why does this panic?", "func main() {} why does this panic?"},
		{"> ```sh make build ``` fails on arm64", "make build fails on arm64"},
		{"~~~ text ~~~", "text"},
		{"use ```inline``` code and ~~strike~~", "use ```inline``` code and ~~strike~~"},
		{"a > b", "a > b"},
		{"```", "```"}, // nothing but markup: keep it rather than show nothing
		{"> ", ">"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := promptPreview(tc.in); got != tc.want {
			t.Errorf("promptPreview(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// "(may be open)" rows stay selectable but are drawn dim; live rows are
// disabled; prompts are cleaned for both the title and the second line.
func TestRowsCleanPromptsAndMarkState(t *testing.T) {
	now := time.Unix(nowEpoch, 0)
	list := []sessions.Session{
		{ID: sidA, ConfigName: "sixtynine", Title: "Checkout fix", FirstPrompt: "> ```go x := 1 ``` why?", LastActive: now.Add(-time.Hour)},
		{ID: sidB, ConfigName: "julienning3", FirstPrompt: "> untitled ask", LastActive: now.Add(-time.Minute), MaybeOpen: true},
		{ID: sidC, ConfigName: "sixtynine", Title: "Busy", FirstPrompt: "p", LastActive: now, Live: true, LiveIn: "sixtynine"},
	}
	items := rows(list, config.ConfigDir{Name: "julienning3"}, nil, false, now, time.UTC)
	if len(items) != 4 {
		t.Fatalf("items = %+v", items)
	}
	if r := items[0]; r.Title != "New session" || r.Muted || r.Disabled {
		t.Errorf("new session = %+v", r)
	}
	if r := items[1]; r.Detail != "x := 1 why?" || r.Muted || r.Disabled {
		t.Errorf("titled row = %+v", r)
	}
	if r := items[2]; r.Title != "untitled ask" || r.Detail != "" || !r.Muted || r.Disabled || r.Note != "(may be open)" {
		t.Errorf("may-be-open row = %+v", r)
	}
	if r := items[3]; !r.Disabled || r.Muted || r.Note != "(open in another terminal)" {
		t.Errorf("live row = %+v", r)
	}
}

func TestUseWithoutNameUsesCurrent(t *testing.T) {
	f := setup(t)
	one := f.addConfig("one", email1)
	f.addConfig("two", email2)
	f.save()
	f.fake.Listing = listing(acct(email2, 1, 1), acct(email1, 90, 90)) // two ranks first
	if err := config.SetCurrent(one); err != nil {
		t.Fatal(err)
	}
	got := f.run("use")
	assertCode(t, got, 0)
	if f.current() != one.Dir || !strings.Contains(got.stdout, "Now using one (") {
		t.Errorf("use without a name must stay on the current config: stdout=%q current=%q", got.stdout, f.current())
	}
}

func TestUseWithoutNameFallsBackToNext(t *testing.T) {
	f := setup(t)
	f.addConfig("one", email1)
	two := f.addConfig("two", email2)
	f.save()
	f.fake.Listing = listing(acct(email2, 1, 1), acct(email1, 90, 90))
	got := f.run("use", "--no-launch")
	assertCode(t, got, 0)
	if f.current() != two.Dir || !strings.Contains(got.stdout, "Now using two (") {
		t.Errorf("use without a selection must rank like next: stdout=%q current=%q", got.stdout, f.current())
	}
}
