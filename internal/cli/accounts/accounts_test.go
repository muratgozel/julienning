package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

const (
	nowEpoch  = 1789482657 // 2026-09-15T14:30:57Z = Tue 17:30:57 +03:00
	friReset  = 1789714800 // 2026-09-18T07:00:00Z = Fri 10:00 +03:00
	lateReset = 1789504800 // 2026-09-15T20:40:00Z = 23:40 +03:00
	fiveReset = 1789491600 // 2026-09-15T17:00:00Z = 20:00 +03:00
	weekReset = 1793631600 // 2026-11-02T15:00:00Z = 18:00 +03:00
	email1    = "claude1@sixtynine.agency"
	email2    = "claude2@sixtynine.agency"
)

type fixture struct {
	t       *testing.T
	home    string
	jul     string
	fake    *remote.Fake
	cfg     *config.Config
	dead    map[int]bool // pids livesess.Alive reports as gone
	parents map[int]int  // pid → parent pid, for parentOf; the hook's own parent is hookPPID
	hints   int          // selfupdate.Hint calls
	ucheck  int          // selfupdate.AutoUpdateIfDue calls
}

// hookPPID is the pid the stubbed hookParent reports for the hook process.
const hookPPID = 4242

// setup builds a hermetic environment: temp HOME, temp JULIENNING_HOME, a
// frozen clock, an explicit timezone, a fake Worker, a stubbed process table
// and no update checks.
func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, home: filepath.Join(root, "home"), jul: filepath.Join(root, "julienning"), dead: map[int]bool{}, parents: map[int]int{}}
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f.home)
	t.Setenv("JULIENNING_HOME", f.jul)
	t.Setenv("JULIENNING_NOW_EPOCH", fmt.Sprint(nowEpoch))
	t.Setenv("TZ", "Europe/Istanbul")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("JULIENNING_REMOTE_URL", "")
	t.Setenv("JULIENNING_TOKEN", "")

	f.fake = &remote.Fake{}
	prevClient, prevHint, prevCheck := newClient, updateHint, autoUpdateIfDue
	prevAlive, prevLock, prevStart, prevPoll, prevStdin := livesess.Alive, lockWait, startWait, pollEvery, hookStdinWait
	prevHookParent, prevParentOf := hookParent, parentOf
	newClient = func(*config.Config, time.Duration) remote.Client { return f.fake }
	updateHint = func(w io.Writer) { f.hints++ }
	autoUpdateIfDue = func(context.Context, *config.Config) error { f.ucheck++; return nil }
	livesess.Alive = func(s livesess.Session) bool { return !f.dead[s.PID] }
	lockWait, startWait = 0, 0
	hookParent = func() int { return hookPPID }
	parentOf = func(pid int) (int, error) {
		if p, ok := f.parents[pid]; ok {
			return p, nil
		}
		return 0, fmt.Errorf("no process %d", pid)
	}
	t.Cleanup(func() {
		newClient, updateHint, autoUpdateIfDue = prevClient, prevHint, prevCheck
		livesess.Alive, lockWait, startWait, pollEvery, hookStdinWait = prevAlive, prevLock, prevStart, prevPoll, prevStdin
		hookParent, parentOf = prevHookParent, prevParentOf
	})

	f.cfg = &config.Config{
		Version:            config.SchemaVersion,
		Dev:                "murat",
		MachineID:          "3fa9c2d1e07b",
		Remote:             config.Remote{URL: "https://worker.test", Token: "tok"},
		SendMinIntervalSec: 300,
	}
	return f
}

// addConfig registers a config dir (with an empty session registry) and
// optionally logs an account into it. The name "default" is ~/.claude.
func (f *fixture) addConfig(name, email string) config.ConfigDir {
	f.t.Helper()
	dir := filepath.Join(f.home, ".claude-"+name)
	if name == "default" {
		dir = filepath.Join(f.home, ".claude")
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if email != "" {
		f.writeAccount(dir, email)
	}
	cd := config.ConfigDir{Name: name, Dir: dir}
	if err := f.cfg.Add(cd); err != nil {
		f.t.Fatal(err)
	}
	return cd
}

// writeAccount logs email into dir, honouring the default-dir rule.
func (f *fixture) writeAccount(dir, email string) {
	f.t.Helper()
	p := filepath.Join(dir, ".claude.json")
	if dir == filepath.Join(f.home, ".claude") {
		p = filepath.Join(f.home, ".claude.json")
	}
	body := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// session adds a live-session registry entry to dir.
func (f *fixture) session(dir string, pid int, id string) {
	f.t.Helper()
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":"/tmp/p"}`, pid, id)
	p := filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", pid))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// share writes a fresh shared.json.
func (f *fixture) share(emails ...string) {
	f.t.Helper()
	f.shareAt(time.Unix(nowEpoch, 0), emails...)
}

// shareNicks writes a fresh shared.json with team nicknames; "" stands for a
// record shared before nicknames existed.
func (f *fixture) shareNicks(nicks map[string]string) {
	f.t.Helper()
	entries := make([]sharedcache.Entry, 0, len(nicks))
	for e, n := range nicks {
		entries = append(entries, sharedcache.Entry{Email: e, Nickname: n})
	}
	if _, err := sharedcache.SaveEntries(entries, time.Unix(nowEpoch, 0)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) shareAt(at time.Time, emails ...string) {
	f.t.Helper()
	if _, err := sharedcache.Save(emails, at); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) sharedCache() *sharedcache.Cache {
	f.t.Helper()
	c, err := sharedcache.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) held() claims.Held {
	f.t.Helper()
	h, err := claims.LoadHeld()
	if err != nil {
		f.t.Fatal(err)
	}
	return h
}

func (f *fixture) save() {
	f.t.Helper()
	if err := f.cfg.Save(); err != nil {
		f.t.Fatal(err)
	}
}

type result struct {
	code   int
	stdout string
	stderr string
}

func (f *fixture) run(stdin string, args ...string) result {
	f.t.Helper()
	var out, errOut strings.Builder
	code := cli.Main(args, strings.NewReader(stdin), &out, &errOut)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func (f *fixture) errorLog() string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.jul, config.ErrorLog))
	if err != nil {
		return ""
	}
	return string(raw)
}

func (f *fixture) ops() string { return strings.Join(f.fake.Ops(), ",") }

func ts(epoch int64) *time.Time {
	t := time.Unix(epoch, 0).UTC()
	return &t
}

func win(used float64, reset int64) *remote.Window {
	return &remote.Window{Used: used, ResetsAt: ts(reset)}
}

func listing() *remote.Listing {
	return &remote.Listing{
		GeneratedAt: time.Unix(nowEpoch, 0).UTC(),
		TZ:          "Europe/Istanbul",
		Accounts: []remote.Account{
			{
				Rank: 1, Email: email1, Nickname: "alpha",
				Session: win(12, fiveReset), Week: win(28, weekReset),
				CollectedAt: ts(nowEpoch - 120),
				Reporter:    &remote.Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"},
				State:       "free",
			},
			{
				// A legacy record: shared before nicknames existed.
				Rank: 2, Email: email2,
				Session: win(50, fiveReset), Week: win(60, weekReset),
				CollectedAt: ts(nowEpoch - 720),
				Reporter:    &remote.Identity{Dev: "ali", MachineID: "bbbbbbbbbbbb"},
				State:       "in_use", BusyBy: []string{"ali", "can"},
			},
		},
	}
}

func assertCode(t *testing.T, got result, want int) {
	t.Helper()
	if got.code != want {
		t.Fatalf("exit code = %d, want %d (stdout %q, stderr %q)", got.code, want, got.stdout, got.stderr)
	}
}

// --- current -----------------------------------------------------------

func TestCurrentNone(t *testing.T) {
	f := setup(t)
	f.addConfig("sixtynine1", email1)
	f.save()
	got := f.run("", "current")
	assertCode(t, got, 0)
	if got.stdout != "none\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	got = f.run("", "current", "--json")
	assertCode(t, got, 0)
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	if doc["config"] != nil {
		t.Errorf("config = %v, want null", doc["config"])
	}
	for _, k := range []string{"email", "nickname", "account", "usage_error"} {
		v, ok := doc[k]
		if !ok || v != nil {
			t.Errorf("%s = %v (present %v), want an explicit null", k, v, ok)
		}
	}
	if f.hints != 2 {
		t.Errorf("update hint shown %d times, want once per run", f.hints)
	}
}

// The spec pins the shape: usage_error is always a key, null on success.
func TestCurrentJSONAlwaysHasUsageError(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	l := listing()
	f.fake.Accounts = map[string]*remote.Account{email1: &l.Accounts[0]}

	got := f.run("", "current", "--json")
	assertCode(t, got, 0)
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	v, ok := doc["usage_error"]
	if !ok || v != nil {
		t.Errorf("usage_error = %v (present %v), want null", v, ok)
	}
	if doc["account"] == nil || doc["email"] != email1 || doc["nickname"] != "alpha" || doc["config"] != "sixtynine1" {
		t.Errorf("doc = %v", doc)
	}
	// The lookup carries dev, so the Worker does not call our own report "in use".
	if get := f.fake.CallsFor("get"); len(get) != 1 || get[0].Dev != "murat" {
		t.Errorf("get calls = %+v, want one with dev=murat", get)
	}

	// A failure fills the same key.
	f.fake.Accounts = nil
	f.fake.GetErr = errors.New("request timed out after 10s")
	got = f.run("", "current", "--json")
	assertCode(t, got, 0)
	doc = nil
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	if doc["usage_error"] != "request timed out after 10s" {
		t.Errorf("usage_error = %v", doc["usage_error"])
	}
}

func TestCurrentWithUsage(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	l := listing()
	f.fake.Accounts = map[string]*remote.Account{email1: &l.Accounts[0]}

	got := f.run("", "current")
	assertCode(t, got, 0)
	// The Worker's nickname names the account even when shared.json has none.
	want := "alpha (" + email1 + ") in ~/.claude-sixtynine1\nusage: session 12% → 20:00, week 28% → 2026-11-02 18:00\n"
	if got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
	if f.hints != 1 {
		t.Errorf("update hint shown %d times", f.hints)
	}

	// Shared but never reported, and a legacy record without a nickname: the
	// config name stands in.
	f.fake.Accounts = map[string]*remote.Account{email1: {Email: email1, State: "free"}}
	got = f.run("", "current")
	if want := "sixtynine1 (" + email1 + ") in ~/.claude-sixtynine1\nusage: no usage reported yet\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
}

// The default dir's login lives in ~/.claude.json (default-dir rule).
func TestCurrentDefaultDir(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("default", email1)
	f.save()
	f.share(email1)
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cd.Dir, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("fixture must not write ~/.claude/.claude.json: %v", err)
	}
	got := f.run("", "current")
	assertCode(t, got, 0)
	if !strings.HasPrefix(got.stdout, "default ("+email1+") in ~/.claude\n") {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestCurrentUsageUnavailable(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}

	// The Worker's 404: the allowlist moved since shared.json was fetched.
	got := f.run("", "current")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, "usage: unavailable (account is not shared)") {
		t.Errorf("stdout = %q", got.stdout)
	}

	f.fake.GetErr = errors.New("request timed out after 10s")
	got = f.run("", "current")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, "usage: unavailable (request timed out after 10s)") {
		t.Errorf("stdout = %q", got.stdout)
	}
}

// A registered dir logged into a personal account must not send that address
// to the Worker.
func TestCurrentNeverAsksAboutUnsharedAccounts(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", "me@personal.com")
	f.save()
	f.share(email1)
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	got := f.run("", "current")
	assertCode(t, got, 0)
	if got.stdout != "sixtynine1 (me@personal.com) in ~/.claude-sixtynine1\nusage: unavailable (account is not shared)\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("personal email reached the Worker: %+v", f.fake.Calls)
	}
}

func TestCurrentNotLoggedIn(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("fresh", "")
	f.save()
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	got := f.run("", "current")
	assertCode(t, got, 0)
	if got.stdout != "fresh in ~/.claude-fresh\nusage: unavailable (config not logged in)\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestCurrentWithoutRemote(t *testing.T) {
	f := setup(t)
	f.cfg.Remote.Token = ""
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	got := f.run("", "current")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, "usage: unavailable (Worker URL or token not configured") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("calls = %+v", f.fake.Calls)
	}
}

// --- accounts ----------------------------------------------------------

func TestAccountsTable(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.addConfig("sixtynine9", email1) // same account, two dirs
	f.save()
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	lines := strings.Split(strings.TrimRight(got.stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %q", len(lines), got.stdout)
	}
	if !strings.HasPrefix(lines[0], "#  NICK   ACCOUNT") || !strings.Contains(lines[0], "UPDATED") {
		t.Errorf("header = %q", lines[0])
	}
	for _, want := range []string{"1", "alpha", email1, "12% → 20:00", "28% → 2026-11-02 18:00", "free", "*~/.claude-sixtynine1,~/.claude-sixtynine9", "2m ago"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row 1 = %q, missing %q", lines[1], want)
		}
	}
	for _, want := range []string{"2  -      " + email2, "50% → 20:00", "in use by ali, can (12m)", " - ", "12m ago"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("row 2 = %q, missing %q", lines[2], want)
		}
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q", got.stderr)
	}
	if f.hints != 1 {
		t.Errorf("update hint shown %d times", f.hints)
	}
}

func TestAccountsRendersResetAndClaimStates(t *testing.T) {
	f := setup(t)
	f.save()
	l := listing()
	l.Accounts[0].Session = &remote.Window{Used: 80, ResetsAt: ts(nowEpoch - 60), ResetPassed: true}
	l.Accounts[0].Week = nil
	l.Accounts[0].CollectedAt = nil
	l.Accounts[1].State = "claimed"
	l.Accounts[1].BusyBy = []string{"ali", "can"}
	l.Accounts[1].Claims = []remote.Claim{
		{Dev: "murat", MachineID: "3fa9c2d1e07b", At: time.Unix(nowEpoch-600, 0).UTC()}, // our own: ignored
		{Dev: "can", MachineID: "cccccccccccc", At: time.Unix(nowEpoch-60, 0).UTC()},
		{Dev: "ali", MachineID: "bbbbbbbbbbbb", At: time.Unix(nowEpoch-180, 0).UTC()},
	}
	f.fake.Listing = l

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	for _, want := range []string{"0% → reset", "never", "claimed by ali, can (3m)"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("stdout = %q, missing %q", got.stdout, want)
		}
	}
	// An absent window renders as "-", not as 0%.
	row := strings.Split(got.stdout, "\n")[1]
	if !strings.Contains(row, "reset") || !strings.Contains(row, " - ") {
		t.Errorf("absent week window: %q", row)
	}
}

func TestAccountsStateFallbacks(t *testing.T) {
	n := time.Unix(nowEpoch, 0)
	loc := location()
	claimed := &remote.Account{State: "claimed", Claims: []remote.Claim{
		{Dev: "murat", At: n.Add(-time.Hour)},
		{Dev: "can", At: n.Add(-2 * time.Minute)},
		{Dev: "ali", At: n.Add(-5 * time.Minute)},
		{Dev: "ali", At: n.Add(-time.Minute)},
	}}
	if got := state(claimed, "murat", n, loc); got != "claimed by ali, can (5m)" {
		t.Errorf("claimed fallback = %q", got)
	}
	inUse := &remote.Account{State: "in_use", Reporter: &remote.Identity{Dev: "ali"}, CollectedAt: ts(nowEpoch - 30)}
	if got := state(inUse, "murat", n, loc); got != "in use by ali (30s)" {
		t.Errorf("in use fallback = %q", got)
	}
	if got := state(&remote.Account{State: "claimed"}, "murat", n, loc); got != "claimed" {
		t.Errorf("bare claimed = %q", got)
	}
	if got := state(&remote.Account{State: "something-new"}, "murat", n, loc); got != "free" {
		t.Errorf("unknown state = %q", got)
	}
}

// Allowlisted accounts that never reported show "-" windows.
func TestAccountsNeverReported(t *testing.T) {
	f := setup(t)
	f.save()
	f.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Rank: 1, Email: email1, State: "free"}}}
	got := f.run("", "accounts")
	assertCode(t, got, 0)
	row := strings.Split(got.stdout, "\n")[1]
	if strings.Count(row, " - ") < 2 || !strings.Contains(row, "never") {
		t.Errorf("row = %q", row)
	}
}

func TestAccountsEmpty(t *testing.T) {
	f := setup(t)
	f.save()
	f.fake.Listing = &remote.Listing{TZ: "Europe/Istanbul"}
	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if got.stdout != "no shared accounts yet (share one: julienning share EMAIL)\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestAccountsJSON(t *testing.T) {
	f := setup(t)
	f.addConfig("sixtynine1", email1)
	cd9 := f.addConfig("sixtynine9", email1)
	f.save()
	if err := config.SetCurrent(cd9); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()

	got := f.run("", "accounts", "--json")
	assertCode(t, got, 0)
	var doc struct {
		TZ       string `json:"tz"`
		Accounts []struct {
			Email        string   `json:"email"`
			LocalConfig  string   `json:"local_config"`
			LocalConfigs []string `json:"local_configs"`
			Claims       []any    `json:"claims"`
			BusyBy       []string `json:"busy_by"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	if doc.TZ != "Europe/Istanbul" || len(doc.Accounts) != 2 {
		t.Fatalf("doc = %+v", doc)
	}
	a, b := doc.Accounts[0], doc.Accounts[1]
	if a.LocalConfig != "sixtynine9" || strings.Join(a.LocalConfigs, ",") != "sixtynine1,sixtynine9" {
		t.Errorf("account 1 local = %q %v", a.LocalConfig, a.LocalConfigs)
	}
	if b.LocalConfig != "" || b.LocalConfigs == nil || len(b.LocalConfigs) != 0 {
		t.Errorf("account 2 local = %q %#v, want \"\" and []", b.LocalConfig, b.LocalConfigs)
	}
	if a.Claims == nil || strings.Join(b.BusyBy, ",") != "ali,can" {
		t.Errorf("claims/busy_by not arrays: %+v", doc.Accounts)
	}
	if !strings.Contains(got.stdout, "\n  \"") {
		t.Errorf("want 2-space indentation: %q", got.stdout)
	}
	if !strings.Contains(got.stdout, `"local_configs": []`) {
		t.Errorf("empty local_configs must be [], not null: %q", got.stdout)
	}
}

// accounts reconciles claims first and refreshes shared.json from its listing.
func TestAccountsSyncsClaimsAndSharedCache(t *testing.T) {
	f := setup(t)
	a := f.addConfig("sixtynine1", email1)
	f.addConfig("sixtynine2", email2)
	f.save()
	f.session(a.Dir, 100, "s1")
	f.shareAt(time.Unix(nowEpoch-7200, 0), email1, email2)
	if err := claims.SaveHeld(claims.Held{email2: time.Unix(nowEpoch-60, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()
	f.fake.Listing.Accounts = append(f.fake.Listing.Accounts, remote.Account{Email: "claude3@sixtynine.agency", State: "free"})

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if f.ops() != "claim,unclaim,list" {
		t.Fatalf("ops = %s, want reconcile before listing", f.ops())
	}
	h := f.held()
	if _, ok := h[email1]; !ok {
		t.Errorf("live account not claimed: %v", h)
	}
	if _, ok := h[email2]; ok {
		t.Errorf("idle account still held: %v", h)
	}
	c := f.sharedCache()
	if !c.Contains("claude3@sixtynine.agency") || c.FetchedAt.Unix() != nowEpoch {
		t.Errorf("shared.json not refreshed from the listing: %+v", c)
	}
	if claims.ReconcileDue(time.Unix(nowEpoch, 0)) {
		t.Errorf("reconcile marker not stamped")
	}
}

func TestAccountsWarnsWhenClaimSyncFails(t *testing.T) {
	f := setup(t)
	a := f.addConfig("sixtynine1", email1)
	f.save()
	f.session(a.Dir, 100, "s1")
	f.share(email1)
	f.fake.PutClaimErr = &remote.Error{Status: 503, Message: "service unavailable"}
	f.fake.Listing = listing()

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if !strings.Contains(got.stderr, "julienning: warning: could not sync this machine's claims (claim sixtynine1: 503 service unavailable)") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if !strings.Contains(got.stdout, email1) {
		t.Errorf("listing must still render: %q", got.stdout)
	}
}

// Another process reconciling right now is not a failure.
func TestAccountsSkipsClaimSyncWhileLocked(t *testing.T) {
	f := setup(t)
	a := f.addConfig("sixtynine1", email1)
	f.save()
	f.session(a.Dir, 100, "s1")
	f.share(email1)
	release, ok, err := claims.AcquireLock(time.Unix(nowEpoch, 0))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer release()
	f.fake.Listing = listing()

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if f.ops() != "list" || got.stderr != "" {
		t.Errorf("ops = %s, stderr = %q", f.ops(), got.stderr)
	}
}

func TestAccountsWarnsAboutUnreadableConfigs(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("broken", "")
	f.save()
	if err := os.WriteFile(filepath.Join(cd.Dir, ".claude.json"), []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()
	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if !strings.Contains(got.stderr, "cannot read the account of config broken") {
		t.Errorf("stderr = %q", got.stderr)
	}
}

func TestAccountsErrors(t *testing.T) {
	t.Run("remote failure", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.fake.ListErr = &remote.Error{Status: 401, Message: "unauthorized"}
		got := f.run("", "accounts")
		assertCode(t, got, cli.ExitError)
		if got.stderr != "julienning: 401 unauthorized\n" {
			t.Errorf("stderr = %q", got.stderr)
		}
	})
	t.Run("remote not configured", func(t *testing.T) {
		f := setup(t)
		f.cfg.Remote.URL = ""
		f.save()
		got := f.run("", "accounts")
		assertCode(t, got, cli.ExitError)
		if !strings.Contains(got.stderr, "Worker URL or token not configured") {
			t.Errorf("stderr = %q", got.stderr)
		}
		if len(f.fake.Calls) != 0 {
			t.Errorf("calls = %+v", f.fake.Calls)
		}
	})
	t.Run("arguments", func(t *testing.T) {
		f := setup(t)
		f.save()
		assertCode(t, f.run("", "accounts", "extra"), cli.ExitUsage)
		assertCode(t, f.run("", "current", "extra"), cli.ExitUsage)
	})
}

// --- nicknames ---------------------------------------------------------

// The exact table: NICK from the Worker ("-" for a legacy record), LOCAL as
// home-shortened dir paths with the selection starred.
func TestAccountsTableFormat(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.addConfig("sixtynine9", email1)
	f.save()
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	want := "" +
		"#  NICK   ACCOUNT                   SESSION      WEEK                    STATE                     LOCAL                                       UPDATED\n" +
		"1  alpha  claude1@sixtynine.agency  12% → 20:00  28% → 2026-11-02 18:00  free                      *~/.claude-sixtynine1,~/.claude-sixtynine9  2m ago\n" +
		"2  -      claude2@sixtynine.agency  50% → 20:00  60% → 2026-11-02 18:00  in use by ali, can (12m)  -                                           12m ago\n"
	if got.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", got.stdout, want)
	}
}

// LOCAL covers the default dir (~/.claude) and dirs outside $HOME, which keep
// their absolute path; config names never appear.
func TestAccountsLocalPaths(t *testing.T) {
	f := setup(t)
	f.addConfig("default", email1)
	outside := filepath.Join(filepath.Dir(f.home), "elsewhere", ".claude-x")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeAccount(outside, email2)
	if err := f.cfg.Add(config.ConfigDir{Name: "julienning7", Dir: outside}); err != nil {
		t.Fatal(err)
	}
	f.save()
	f.fake.Listing = listing()

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	lines := strings.Split(got.stdout, "\n")
	if !strings.Contains(lines[1], "  ~/.claude  ") {
		t.Errorf("default dir row = %q", lines[1])
	}
	if !strings.Contains(lines[2], "  "+outside+"  ") {
		t.Errorf("outside-home row = %q, want %s", lines[2], outside)
	}
	if strings.Contains(got.stdout, "julienning7") || strings.Contains(got.stdout, "*") {
		t.Errorf("config names or a selection marker leaked into LOCAL: %q", got.stdout)
	}
}

// --json passes the Worker's nickname through as sent (string, null, or
// absent for legacy records) and keeps local_config/local_configs as config
// names; nothing else is added.
func TestAccountsJSONNicknames(t *testing.T) {
	f := setup(t)
	f.addConfig("sixtynine1", email1)
	f.save()
	raw := `{"generated_at":"2026-09-15T14:30:57Z","tz":"Europe/Istanbul","accounts":[` +
		`{"rank":1,"email":"` + email1 + `","nickname":"alpha","state":"free","claims":[],"busy_by":[]},` +
		`{"rank":2,"email":"` + email2 + `","nickname":null,"state":"free","claims":[],"busy_by":[]},` +
		`{"rank":3,"email":"claude3@sixtynine.agency","state":"free","claims":[],"busy_by":[]}]}`
	l := &remote.Listing{Raw: json.RawMessage(raw), Accounts: []remote.Account{
		{Rank: 1, Email: email1, Nickname: "alpha", State: "free"},
		{Rank: 2, Email: email2, State: "free"},
		{Rank: 3, Email: "claude3@sixtynine.agency", State: "free"},
	}}
	f.fake.Listing = l

	got := f.run("", "accounts", "--json")
	assertCode(t, got, 0)
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	if len(doc.Accounts) != 3 {
		t.Fatalf("accounts = %v", doc.Accounts)
	}
	a, b, c := doc.Accounts[0], doc.Accounts[1], doc.Accounts[2]
	if a["nickname"] != "alpha" || a["local_config"] != "sixtynine1" {
		t.Errorf("account 1 = %v", a)
	}
	if locals, _ := a["local_configs"].([]any); len(locals) != 1 || locals[0] != "sixtynine1" {
		t.Errorf("account 1 local_configs = %#v", a["local_configs"])
	}
	if v, ok := b["nickname"]; !ok || v != nil {
		t.Errorf("account 2 nickname = %#v (present %v), want the Worker's null", v, ok)
	}
	if _, ok := c["nickname"]; ok {
		t.Errorf("account 3 gained a nickname key: %v", c)
	}
	wantKeys := "busy_by,claims,email,local_config,local_configs,nickname,rank,state"
	if keys := strings.Join(slices.Sorted(maps.Keys(a)), ","); keys != wantKeys {
		t.Errorf("account 1 keys = %s, want %s", keys, wantKeys)
	}

	// The table renders both legacy shapes as "-".
	got = f.run("", "accounts")
	assertCode(t, got, 0)
	lines := strings.Split(got.stdout, "\n")
	if !strings.HasPrefix(lines[2], "2  -    ") || !strings.HasPrefix(lines[3], "3  -    ") {
		t.Errorf("legacy rows = %q", lines[2:4])
	}
}

// currentFixture selects a dir logged into email1 and has the Worker answer
// with account (nil: the Worker's 404).
func currentFixture(t *testing.T, cacheNick string, account *remote.Account) *fixture {
	t.Helper()
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.shareNicks(map[string]string{email1: cacheNick, email2: "beta"})
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	if account != nil {
		f.fake.Accounts = map[string]*remote.Account{email1: account}
	}
	return f
}

func currentJSON(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	got := f.run("", "current", "--json")
	assertCode(t, got, 0)
	var doc map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	return doc
}

func TestCurrentNicknames(t *testing.T) {
	usageLine := "usage: session 12% → 20:00, week 28% → 2026-11-02 18:00\n"
	reported := func(nick string) *remote.Account {
		a := listing().Accounts[0]
		a.Nickname = nick
		return &a
	}
	cases := []struct {
		name      string
		cacheNick string
		account   *remote.Account
		getErr    error
		text      string
		nick      any // JSON value: string or nil
	}{
		{"from shared.json", "alpha", reported(""), nil,
			"alpha (" + email1 + ") in ~/.claude-sixtynine1\n" + usageLine, "alpha"},
		{"the Worker's answer wins over a stale cache", "alpha", reported("gamma"), nil,
			"gamma (" + email1 + ") in ~/.claude-sixtynine1\n" + usageLine, "gamma"},
		{"legacy record: config name stands in", "", reported(""), nil,
			"sixtynine1 (" + email1 + ") in ~/.claude-sixtynine1\n" + usageLine, nil},
		{"offline: the cached nickname still names it", "alpha", nil, errors.New("request timed out after 10s"),
			"alpha (" + email1 + ") in ~/.claude-sixtynine1\nusage: unavailable (request timed out after 10s)\n", "alpha"},
		{"unshared per the Worker: no team name", "alpha", nil, nil,
			"sixtynine1 (" + email1 + ") in ~/.claude-sixtynine1\nusage: unavailable (account is not shared)\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := currentFixture(t, tc.cacheNick, tc.account)
			f.fake.GetErr = tc.getErr
			got := f.run("", "current")
			assertCode(t, got, 0)
			if got.stdout != tc.text {
				t.Errorf("stdout = %q, want %q", got.stdout, tc.text)
			}
			doc := currentJSON(t, f)
			if v, ok := doc["nickname"]; !ok || v != tc.nick {
				t.Errorf("nickname = %#v (present %v), want %#v", v, ok, tc.nick)
			}
			if doc["config"] != "sixtynine1" || doc["email"] != email1 {
				t.Errorf("doc = %v", doc)
			}
			wantKeys := "account,config,email,nickname,usage_error"
			if keys := strings.Join(slices.Sorted(maps.Keys(doc)), ","); keys != wantKeys {
				t.Errorf("keys = %s, want %s", keys, wantKeys)
			}
		})
	}
}

// A personal login in a registered dir never borrows a team nickname, and an
// unreadable cache only costs the name, not the command.
func TestCurrentNicknameEdgeCases(t *testing.T) {
	t.Run("personal account", func(t *testing.T) {
		f := setup(t)
		cd := f.addConfig("sixtynine1", "me@personal.com")
		f.save()
		f.shareNicks(map[string]string{email1: "alpha"})
		if err := config.SetCurrent(cd); err != nil {
			t.Fatal(err)
		}
		if doc := currentJSON(t, f); doc["nickname"] != nil || doc["usage_error"] != "account is not shared" {
			t.Errorf("doc = %v", doc)
		}
		if len(f.fake.Calls) != 0 {
			t.Errorf("personal email reached the Worker: %+v", f.fake.Calls)
		}
	})
	t.Run("broken cache", func(t *testing.T) {
		f := setup(t)
		cd := f.addConfig("sixtynine1", email1)
		f.save()
		if err := config.SetCurrent(cd); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(f.jul, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.jul, sharedcache.File), []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := f.run("", "current")
		assertCode(t, got, 0)
		if !strings.HasPrefix(got.stdout, "sixtynine1 ("+email1+") in ~/.claude-sixtynine1\nusage: unavailable (cannot tell whether the account is shared") {
			t.Errorf("stdout = %q", got.stdout)
		}
	})
	t.Run("outside home", func(t *testing.T) {
		f := setup(t)
		dir := filepath.Join(filepath.Dir(f.home), "elsewhere")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f.writeAccount(dir, email1)
		cd := config.ConfigDir{Name: "julienning7", Dir: dir}
		if err := f.cfg.Add(cd); err != nil {
			t.Fatal(err)
		}
		f.save()
		f.shareNicks(map[string]string{email1: "alpha"})
		if err := config.SetCurrent(cd); err != nil {
			t.Fatal(err)
		}
		f.fake.Accounts = map[string]*remote.Account{email1: {Email: email1, State: "free"}}
		got := f.run("", "current")
		if want := "alpha (" + email1 + ") in " + dir + "\n"; !strings.HasPrefix(got.stdout, want) {
			t.Errorf("stdout = %q, want prefix %q", got.stdout, want)
		}
	})
}

func TestShortenHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]string{
		home:                           "~",
		filepath.Join(home, ".claude"): "~/.claude",
		filepath.Join(home, "a", "b"):  "~/a/b",
		home + "-sibling":              home + "-sibling",
		"/opt/claude":                  "/opt/claude",
	}
	for in, want := range cases {
		if got := shortenHome(in); got != want {
			t.Errorf("shortenHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- not set up --------------------------------------------------------

func TestCommandsRequireSetup(t *testing.T) {
	for _, name := range []string{"current", "accounts"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			got := f.run("", name)
			assertCode(t, got, cli.ExitError)
			if !strings.Contains(got.stderr, "not set up") {
				t.Errorf("stderr = %q", got.stderr)
			}
			if _, err := os.Stat(f.jul); !os.IsNotExist(err) {
				t.Errorf("a failed command created %s: %v", f.jul, err)
			}
		})
	}
}

// --- exhausted ---------------------------------------------------------

// exhausted is an account at 100% in one or both windows, with the Worker's
// exhausted_until (the later reset of the full windows).
func exhausted(email string, session, week *remote.Window, until int64, busy ...string) remote.Account {
	a := remote.Account{Email: email, Session: session, Week: week, State: "exhausted", Exhausted: true, BusyBy: busy}
	if until != 0 {
		a.ExhaustedUntil = ts(until)
	}
	return a
}

func TestAccountsExhaustedState(t *testing.T) {
	t.Setenv("TZ", "Europe/Istanbul") // expectations are in +03; CI runs in UTC
	n := time.Unix(nowEpoch, 0)
	loc := location()
	cases := []struct {
		name string
		a    remote.Account
		want string
	}{
		{"week", exhausted(email1, win(40, lateReset), win(100, friReset), friReset),
			"exhausted (week resets Fri 10:00)"},
		{"session", exhausted(email1, win(100, lateReset), win(70, friReset), lateReset),
			"exhausted (session resets 23:40)"},
		{"both: the later reset", exhausted(email1, win(100, lateReset), win(100, friReset), friReset),
			"exhausted (week resets Fri 10:00)"},
		{"busy", exhausted(email1, win(100, lateReset), win(70, friReset), lateReset, "ali"),
			"exhausted (session resets 23:40), in use by ali"},
		{"busy with several devs", exhausted(email1, win(40, lateReset), win(100, friReset), friReset, "ali", "can"),
			"exhausted (week resets Fri 10:00), in use by ali, can"},
		{"window unknown", exhausted(email1, nil, nil, friReset),
			"exhausted (resets Fri 10:00)"},
		{"no reset known", exhausted(email1, win(100, lateReset), nil, 0),
			"exhausted"},
		{"reset already passed here", exhausted(email1, win(100, nowEpoch-60), nil, nowEpoch-60),
			"exhausted (session has reset)"},
		{"state alone", remote.Account{State: "exhausted", Week: win(100, friReset), ExhaustedUntil: ts(friReset)},
			"exhausted (week resets Fri 10:00)"},
		{"flag alone beats in_use", remote.Account{State: "in_use", Exhausted: true, Session: win(100, lateReset), ExhaustedUntil: ts(lateReset), BusyBy: []string{"ali"}},
			"exhausted (session resets 23:40), in use by ali"},
	}
	for _, tc := range cases {
		if got := state(&tc.a, "murat", n, loc); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The table renders the Worker's order (exhausted last) with the exhausted
// STATE in local time; --json passes the Worker's fields through.
func TestAccountsTableExhausted(t *testing.T) {
	f := setup(t)
	f.addConfig("sixtynine3", "claude3@sixtynine.agency")
	f.save()
	l := listing()
	ex := exhausted("claude3@sixtynine.agency", win(40, lateReset), win(100, friReset), friReset, "ali")
	ex.Rank, ex.Nickname, ex.CollectedAt = 3, "claude3", ts(nowEpoch-300)
	l.Accounts = append(l.Accounts, ex)
	f.fake.Listing = l

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	lines := strings.Split(strings.TrimRight(got.stdout, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %q", lines)
	}
	for _, want := range []string{"3  claude3  claude3@sixtynine.agency", "40% → 23:40", "100% → Fri 10:00",
		"  exhausted (week resets Fri 10:00), in use by ali  ", "~/.claude-sixtynine3", "5m ago"} {
		if !strings.Contains(lines[3], want) {
			t.Errorf("row = %q, missing %q", lines[3], want)
		}
	}

	f.fake.Listing.Raw = json.RawMessage(`{"generated_at":"2026-09-15T14:30:57Z","tz":"Europe/Istanbul","accounts":[` +
		`{"rank":1,"email":"claude3@sixtynine.agency","nickname":"claude3","state":"exhausted","claims":[],"busy_by":["ali"],` +
		`"exhausted":true,"exhausted_until":"2026-09-18T07:00:00Z"}]}`)
	got = f.run("", "accounts", "--json")
	assertCode(t, got, 0)
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	if a := doc.Accounts[0]; a["exhausted"] != true || a["exhausted_until"] != "2026-09-18T07:00:00Z" || a["state"] != "exhausted" {
		t.Errorf("Worker fields not passed through: %v", a)
	}
}

func TestCurrentExhausted(t *testing.T) {
	a := exhausted(email1, win(100, lateReset), win(70, friReset), lateReset, "ali")
	a.Nickname = "alpha"
	f := currentFixture(t, "alpha", &a)
	got := f.run("", "current")
	assertCode(t, got, 0)
	want := "alpha (" + email1 + ") in ~/.claude-sixtynine1\n" +
		"usage: session 100% → 23:40, week 70% → Fri 10:00\n" +
		"state: exhausted (session resets 23:40), in use by ali\n"
	if got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
	// A usable account keeps the two-line form.
	l := listing()
	f.fake.Accounts = map[string]*remote.Account{email1: &l.Accounts[0]}
	if got := f.run("", "current"); strings.Contains(got.stdout, "state:") {
		t.Errorf("usable account got a state line: %q", got.stdout)
	}
}

// --- syncing -----------------------------------------------------------

const email3 = "claude3@sixtynine.agency"

// writeLocalAdd writes shared.json with email shared from this machine at
// the given wall-clock time (LocalAddGrace is measured on the wall clock,
// not on JULIENNING_NOW_EPOCH).
func (f *fixture) writeLocalAdd(email, nick string, at time.Time) {
	f.t.Helper()
	doc := map[string]any{
		"fetched_at": time.Unix(nowEpoch-60, 0).UTC(),
		"emails":     []string{email1, email2, email},
		"nicknames":  map[string]string{email1: "alpha", email: nick},
		"local_adds": map[string]time.Time{email: at.UTC()},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(f.jul, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.jul, sharedcache.File), raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func TestAccountsSyncingRows(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.addConfig("sixtynine3", email3)
	f.save()
	if err := config.SetCurrent(cd); err != nil {
		t.Fatal(err)
	}
	f.shareNicks(map[string]string{email1: "alpha", email2: ""})
	if _, err := sharedcache.AddWithNickname(email3, "gamma"); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	want := "" +
		"#  NICK   ACCOUNT                   SESSION      WEEK                    STATE                     LOCAL                  UPDATED\n" +
		"1  alpha  claude1@sixtynine.agency  12% → 20:00  28% → 2026-11-02 18:00  free                      *~/.claude-sixtynine1  2m ago\n" +
		"2  -      claude2@sixtynine.agency  50% → 20:00  60% → 2026-11-02 18:00  in use by ali, can (12m)  -                      12m ago\n" +
		"-  gamma  claude3@sixtynine.agency  -            -                       syncing (just shared)     ~/.claude-sixtynine3   -\n"
	if got.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", got.stdout, want)
	}
	if c := f.sharedCache(); !c.Contains(email3) {
		t.Errorf("the refresh dropped the local add: %+v", c)
	}

	got = f.run("", "accounts", "--json")
	assertCode(t, got, 0)
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
		Syncing  []map[string]any `json:"syncing"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatalf("json: %v (%q)", err, got.stdout)
	}
	if len(doc.Accounts) != 2 {
		t.Errorf("syncing account leaked into accounts: %v", doc.Accounts)
	}
	if len(doc.Syncing) != 1 {
		t.Fatalf("syncing = %v", doc.Syncing)
	}
	s := doc.Syncing[0]
	if s["email"] != email3 || s["nickname"] != "gamma" {
		t.Errorf("syncing row = %v", s)
	}
	if locals, _ := s["local_configs"].([]any); len(locals) != 1 || locals[0] != "sixtynine3" {
		t.Errorf("syncing local_configs = %#v", s["local_configs"])
	}
	if keys := strings.Join(slices.Sorted(maps.Keys(s)), ","); keys != "email,local_configs,nickname" {
		t.Errorf("syncing keys = %s", keys)
	}
}

// No local dir and no nickname: "-" in the table, [] and null in JSON; an
// empty listing still shows the row instead of "no shared accounts yet".
func TestAccountsSyncingOnly(t *testing.T) {
	f := setup(t)
	f.save()
	f.writeLocalAdd(email3, "", time.Now().Add(-time.Minute))
	f.fake.Listing = &remote.Listing{TZ: "Europe/Istanbul"}

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	want := "" +
		"#  NICK  ACCOUNT                   SESSION  WEEK  STATE                  LOCAL  UPDATED\n" +
		"-  -     claude3@sixtynine.agency  -        -     syncing (just shared)  -      -\n"
	if got.stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", got.stdout, want)
	}

	got = f.run("", "accounts", "--json")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, `"local_configs": []`) || !strings.Contains(got.stdout, `"nickname": null`) {
		t.Errorf("syncing row must have [] and null: %q", got.stdout)
	}
}

func TestAccountsSyncingEnds(t *testing.T) {
	t.Run("the listing has it", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.writeLocalAdd(email3, "gamma", time.Now().Add(-time.Minute))
		l := listing()
		l.Accounts = append(l.Accounts, remote.Account{Rank: 3, Email: "Claude3@Sixtynine.Agency", Nickname: "gamma", State: "free"})
		f.fake.Listing = l
		got := f.run("", "accounts")
		assertCode(t, got, 0)
		// The Worker's spelling of the email still matches the lowercased add.
		if strings.Contains(got.stdout, "syncing") || strings.Count(got.stdout, "Claude3@Sixtynine.Agency") != 1 {
			t.Errorf("listed account still syncing: %q", got.stdout)
		}
		got = f.run("", "accounts", "--json")
		assertCode(t, got, 0)
		if !strings.Contains(got.stdout, `"syncing": []`) {
			t.Errorf("want an empty syncing array: %q", got.stdout)
		}
	})
	t.Run("after the grace", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.writeLocalAdd(email3, "gamma", time.Now().Add(-sharedcache.LocalAddGrace-time.Minute))
		f.fake.Listing = listing()
		got := f.run("", "accounts")
		assertCode(t, got, 0)
		if strings.Contains(got.stdout, "syncing") || strings.Contains(got.stdout, email3) {
			t.Errorf("expired local add still shown: %q", got.stdout)
		}
		if c := f.sharedCache(); c.Contains(email3) || len(c.LocalAdds) != 0 {
			t.Errorf("expired local add kept in the cache: %+v", c)
		}
	})
}
