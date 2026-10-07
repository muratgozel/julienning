package switching

import (
	"encoding/json"
	"errors"
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

const usage12 = " — session 12% → 20:00, week 28% → Fri 20:00.\n"

func nacct(email, nick string, session, week float64) remote.Account {
	a := acct(email, session, week)
	a.Nickname = nick
	return a
}

func (f *fixture) listOps() int {
	n := 0
	for _, op := range f.fake.Ops() {
		if op == "list" {
			n++
		}
	}
	return n
}

func TestUseResolvesNicknameEmailAndDirName(t *testing.T) {
	f := setup(t)
	j1 := f.addConfig("julienning1", email1)
	j2 := f.addConfig("julienning2", email2)
	f.save()
	// email2 is a legacy record without a nickname.
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28), nacct(email2, "", 12, 28))

	alpha := "Now using alpha (claude1@x.io) in ~/.claude-julienning1" + usage12
	legacy := "Now using julienning2 (claude2@x.io)" + usage12
	for _, tc := range []struct {
		arg, want string
		dir       config.ConfigDir
	}{
		{"alpha", alpha, j1},
		{"ALPHA", alpha, j1},
		{email1, alpha, j1},
		{"Claude1@X.io", alpha, j1},
		{"julienning1", alpha, j1}, // a dir name still works; the nickname is shown
		{"julienning2", legacy, j2},
		{email2, legacy, j2},
	} {
		f.fake.Reset()
		got := f.run("use", tc.arg)
		assertCode(t, got, 0)
		if got.stdout != tc.want {
			t.Errorf("use %s: stdout = %q\nwant %q", tc.arg, got.stdout, tc.want)
		}
		if got.stderr != "" {
			t.Errorf("use %s: stderr = %q", tc.arg, got.stderr)
		}
		if f.current() != tc.dir.Dir {
			t.Errorf("use %s: current = %q, want %q", tc.arg, f.current(), tc.dir.Dir)
		}
		if n := f.listOps(); n != 1 {
			t.Errorf("use %s: %d listings, want 1", tc.arg, n)
		}
	}
}

// A shared account without a local login is an error that says how to sign
// in, and nothing is switched.
func TestUseNotLocal(t *testing.T) {
	f := setup(t)
	f.addConfig("julienning1", email1)
	f.save()
	f.fake.Listing = listing(nacct(email1, "alpha", 1, 1), nacct(email2, "beta", 1, 1))
	want := "julienning: beta (claude2@x.io) is shared but not logged in on this machine; sign in with `julienning login <config>` or `julienning new-config --login`\n"
	for _, arg := range []string{"beta", email2} {
		got := f.run("use", arg)
		assertCode(t, got, cli.ExitError)
		if got.stderr != want {
			t.Errorf("use %s: stderr = %q\nwant %q", arg, got.stderr, want)
		}
		if got.stdout != "" || f.current() != "" {
			t.Errorf("use %s: must not switch (stdout %q, current %q)", arg, got.stdout, f.current())
		}
	}
}

// A nickname wins over a config dir that happens to carry the same name.
func TestUseNicknameWinsOverConfigName(t *testing.T) {
	f := setup(t)
	clash := f.addConfig("alpha", email2)
	j1 := f.addConfig("julienning1", email1)
	f.save()
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28), nacct(email2, "beta", 12, 28))

	got := f.run("use", "alpha")
	assertCode(t, got, 0)
	if want := "Now using alpha (claude1@x.io) in ~/.claude-julienning1" + usage12; got.stdout != want || f.current() != j1.Dir {
		t.Errorf("stdout = %q current = %q", got.stdout, f.current())
	}
	got = f.run("use", "beta")
	assertCode(t, got, 0)
	if want := "Now using beta (claude2@x.io) in ~/.claude-alpha" + usage12; got.stdout != want || f.current() != clash.Dir {
		t.Errorf("stdout = %q current = %q", got.stdout, f.current())
	}
}

// Among dirs logged into the same account, the current selection wins, then
// the first by name.
func TestUseAccountPrefersCurrentDir(t *testing.T) {
	f := setup(t)
	a := f.addConfig("a", email1)
	b := f.addConfig("b", email1)
	f.save()
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28))
	assertCode(t, f.run("use", "alpha"), 0)
	if f.current() != a.Dir {
		t.Errorf("without a selection the first name wins: %q", f.current())
	}
	f.setCurrent(b)
	for _, arg := range []string{"alpha", email1} {
		got := f.run("use", arg)
		assertCode(t, got, 0)
		if f.current() != b.Dir || !strings.Contains(got.stdout, " in ~/.claude-b ") {
			t.Errorf("use %s: current %q stdout %q", arg, f.current(), got.stdout)
		}
	}
}

// A nickname the cache does not know yet (a teammate just shared or renamed
// it) resolves against the fresh listing, which is also used for usage.
func TestUseRefreshesStaleCacheOnce(t *testing.T) {
	f := setup(t)
	j1 := f.addConfig("julienning1", email1)
	f.save()
	if _, err := sharedcache.SaveEntries([]sharedcache.Entry{{Email: email1, Nickname: "old"}}, nowT); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing(nacct(email1, "gamma", 12, 28))
	got := f.run("use", "gamma")
	assertCode(t, got, 0)
	if want := "Now using gamma (claude1@x.io) in ~/.claude-julienning1" + usage12; got.stdout != want || f.current() != j1.Dir {
		t.Errorf("stdout = %q current = %q", got.stdout, f.current())
	}
	if n := f.listOps(); n != 1 {
		t.Errorf("%d listings, want 1", n)
	}
	if c, err := sharedcache.Load(); err != nil || c.Nickname(email1) != "gamma" {
		t.Errorf("cache not refreshed: %+v %v", c, err)
	}
}

// Offline (or without a Worker), nicknames come from the cache.
func TestUseOfflineUsesCachedNicknames(t *testing.T) {
	f := setup(t)
	j1 := f.addConfig("julienning1", email1)
	f.save()
	if _, err := sharedcache.SaveEntries([]sharedcache.Entry{{Email: email1, Nickname: "alpha"}}, nowT); err != nil {
		t.Fatal(err)
	}
	f.fake.ListErr = errors.New("request timed out after 10s")
	got := f.run("use", "alpha")
	assertCode(t, got, 0)
	if want := "Now using alpha (claude1@x.io) in ~/.claude-julienning1.\n"; got.stdout != want || f.current() != j1.Dir {
		t.Errorf("stdout = %q current = %q", got.stdout, f.current())
	}
	if !strings.Contains(got.stderr, "could not reach the Worker") || strings.Contains(got.stderr, "not shared") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if n := f.listOps(); n != 1 {
		t.Errorf("%d listings, want 1", n)
	}

	f.cfg.Remote = config.Remote{}
	f.save()
	f.fake.Reset()
	got = f.run("use", "alpha")
	assertCode(t, got, 0)
	if want := "Now using alpha (claude1@x.io) in ~/.claude-julienning1.\n"; got.stdout != want {
		t.Errorf("no remote: stdout = %q", got.stdout)
	}
	if len(f.fake.Calls) != 0 || !strings.Contains(got.stderr, "using the cached shared-account list") {
		t.Errorf("no remote: calls %v stderr %q", f.fake.Calls, got.stderr)
	}
}

func TestUseWithoutNameShowsNickname(t *testing.T) {
	f := setup(t)
	j1 := f.addConfig("julienning1", email1)
	f.save()
	f.setCurrent(j1)
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28))
	got := f.run("use")
	assertCode(t, got, 0)
	if want := "Now using alpha (claude1@x.io) in ~/.claude-julienning1" + usage12; got.stdout != want {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestNextNicknameOutput(t *testing.T) {
	f := setup(t)
	j1 := f.addConfig("julienning1", email1)
	f.save()
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28))
	got := f.run("next")
	assertCode(t, got, 0)
	if want := "Now using alpha (claude1@x.io) in ~/.claude-julienning1" + usage12; got.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", got.stdout, want)
	}
	if f.current() != j1.Dir {
		t.Errorf("current = %q", f.current())
	}
	got = f.run("next")
	assertCode(t, got, 0)
	if want := "Staying on alpha (claude1@x.io) in ~/.claude-julienning1 (best available)" + usage12; got.stdout != want {
		t.Errorf("stdout = %q\nwant     %q", got.stdout, want)
	}

	busy := nacct(email1, "alpha", 12, 28)
	busy.BusyBy = []string{"ali"}
	f.fake.Listing = listing(busy)
	got = f.run("next")
	assertCode(t, got, 0)
	if !strings.Contains(got.stderr, "alpha (claude1@x.io) is also in use by ali") {
		t.Errorf("stderr = %q", got.stderr)
	}

	f.fake.Listing = listing(nacct(email2, "beta", 1, 1))
	got = f.run("next")
	assertCode(t, got, cli.ExitError)
	if !strings.Contains(got.stderr, "(best ranked: beta (claude2@x.io))") {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestJSONNickname(t *testing.T) {
	f := setup(t)
	f.addConfig("julienning1", email1)
	f.addConfig("julienning2", email2)
	f.addConfig("fresh", "")
	f.save()
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28), nacct(email2, "", 50, 60))
	decode := func(r result) map[string]any {
		t.Helper()
		assertCode(t, r, 0)
		var doc map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
			t.Fatalf("%v: %q", err, r.stdout)
		}
		return doc
	}
	doc := decode(f.run("next", "--json"))
	if doc["nickname"] != "alpha" || doc["config"] != "julienning1" || doc["email"] != email1 {
		t.Errorf("next: %v", doc)
	}
	doc = decode(f.run("use", "alpha", "--json"))
	if doc["nickname"] != "alpha" || doc["config"] != "julienning1" {
		t.Errorf("use alpha: %v", doc)
	}
	for _, arg := range []string{"julienning2", "fresh"} {
		doc = decode(f.run("use", arg, "--json"))
		if v, ok := doc["nickname"]; !ok || v != nil {
			t.Errorf("use %s: want \"nickname\": null, got %v", arg, doc)
		}
	}
}

// Picker rows, the header, the cancel line and the move report name
// accounts by nickname; dirs without one keep their config name.
func TestLaunchPickerUsesNicknames(t *testing.T) {
	f := setup(t)
	f.tty = true
	target := f.addConfig("julienning3", email1)
	other := f.addConfig("sixtynine", email2)
	mine := f.addConfig("personal", personal)
	f.save()
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28), nacct(email2, "beta", 50, 60))
	f.writeSession(other, sidA, "Checkout fix", "fix checkout", 2*time.Hour)
	f.writeSession(mine, sidB, "Side project", "p", 3*time.Hour)

	f.pick = func([]tui.Item) (int, error) { return 0, tui.ErrCanceled }
	got := f.run("use", "alpha")
	assertCode(t, got, 0)
	if want := "Now using alpha (claude1@x.io) in ~/.claude-julienning3 — session 12% → 20:00, week 28% → Fri 20:00."; f.pickHdr != want {
		t.Errorf("header = %q\nwant     %q", f.pickHdr, want)
	}
	if !strings.Contains(got.stdout, "Not starting claude; alpha stays selected.") {
		t.Errorf("stdout = %q", got.stdout)
	}
	rows := f.pickRows
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Meta != "in alpha" {
		t.Errorf("new session row = %+v", rows[0])
	}
	if rows[1].Meta != "beta · 2h ago · 0b7f6c1e" {
		t.Errorf("nicknamed dir row = %+v", rows[1])
	}
	if rows[2].Meta != "personal · 3h ago · 1c8a7d2f" {
		t.Errorf("dir without a nickname row = %+v", rows[2])
	}

	f.pick = func([]tui.Item) (int, error) { return 1, nil }
	got = f.run("use", "alpha")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, `Moved "Checkout fix" from beta to alpha`) {
		t.Errorf("stdout = %q", got.stdout)
	}
	if len(f.execs) != 1 || f.execs[0].dir != target.Dir {
		t.Errorf("execs = %+v", f.execs)
	}
}

func TestMoveLabels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := sessions.Session{ConfigName: "a", ConfigDir: filepath.Join(home, ".claude-a")}
	target := config.ConfigDir{Name: "b", Dir: filepath.Join(home, ".claude-b")}
	if from, to := moveLabels(map[string]string{"a": "beta", "b": "alpha"}, s, target); from != "beta" || to != "alpha" {
		t.Errorf("distinct accounts: %s → %s", from, to)
	}
	if from, to := moveLabels(nil, s, target); from != "a" || to != "b" {
		t.Errorf("no labels: %s → %s", from, to)
	}
	// Two dirs of one account: the nickname cannot tell them apart.
	if from, to := moveLabels(map[string]string{"a": "alpha", "b": "alpha"}, s, target); from != "~/.claude-a" || to != "~/.claude-b" {
		t.Errorf("same account: %s → %s", from, to)
	}
}

func TestRowsUseLabels(t *testing.T) {
	now := time.Unix(nowEpoch, 0)
	list := []sessions.Session{
		{ID: sidA, ConfigName: "sixtynine", Title: "T", LastActive: now.Add(-time.Hour)},
		{ID: sidB, ConfigName: "legacy", Title: "U", LastActive: now.Add(-time.Hour)},
	}
	labels := map[string]string{"julienning3": "alpha", "sixtynine": "beta"}
	items := rows(list, config.ConfigDir{Name: "julienning3"}, labels, false, now, time.UTC)
	if items[0].Meta != "in alpha" || items[1].Meta != "beta · 1h ago · 0b7f6c1e" || items[2].Meta != "legacy · 1h ago · 1c8a7d2f" {
		t.Errorf("items = %+v", items)
	}
}
