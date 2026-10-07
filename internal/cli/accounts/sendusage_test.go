package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/usage"
)

// sent reads the debounce cache, or nil when there is none.
func (f *fixture) sent(email string) *usage.Sent {
	f.t.Helper()
	raw, err := os.ReadFile(usage.SentPath(f.jul, email))
	if err != nil {
		return nil
	}
	var s usage.Sent
	if err := json.Unmarshal(raw, &s); err != nil {
		f.t.Fatalf("cache json: %v", err)
	}
	return &s
}

func sendArgs(overrides ...string) []string {
	args := []string{
		"send-usage",
		"--email", email1,
		"--session-used", "23.5",
		"--session-resets", strconv.FormatInt(fiveReset, 10),
		"--week-used", "41",
		"--week-resets", strconv.FormatInt(weekReset, 10),
		"--collected-at", strconv.FormatInt(nowEpoch, 10),
	}
	return append(args, overrides...)
}

func TestSendUsageReportsAndCaches(t *testing.T) {
	f := setup(t)
	f.save()
	f.share(email1)

	got := f.run("", sendArgs()...)
	assertCode(t, got, 0)
	if got.stdout != "" || got.stderr != "" {
		t.Errorf("send-usage must stay silent: %q %q", got.stdout, got.stderr)
	}
	if len(f.fake.Calls) != 1 {
		t.Fatalf("calls = %+v", f.fake.Calls)
	}
	c := f.fake.Calls[0]
	if c.Op != "usage" || c.Email != email1 {
		t.Fatalf("call = %+v", c)
	}
	if c.Usage.Session.Used != 23.5 || c.Usage.Session.ResetsAt.Unix() != fiveReset {
		t.Errorf("session = %+v", c.Usage.Session)
	}
	if c.Usage.Week.Used != 41 || c.Usage.Week.ResetsAt.Unix() != weekReset {
		t.Errorf("week = %+v", c.Usage.Week)
	}
	if c.Usage.CollectedAt.Unix() != nowEpoch || c.Usage.CollectedAt.Location().String() != "UTC" {
		t.Errorf("collected_at = %v", c.Usage.CollectedAt)
	}
	if c.Usage.Reporter.Dev != "murat" || c.Usage.Reporter.MachineID != "3fa9c2d1e07b" {
		t.Errorf("reporter = %+v", c.Usage.Reporter)
	}

	raw, err := os.ReadFile(usage.SentPath(f.jul, email1))
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	var cached usage.Sent
	if err := json.Unmarshal(raw, &cached); err != nil {
		t.Fatalf("cache json: %v", err)
	}
	if cached.SessionUsed != 23.5 || cached.WeekResets != weekReset || cached.SentAt.Unix() != nowEpoch {
		t.Errorf("cache = %+v", cached)
	}
	if cached.AttemptedAt.Unix() != nowEpoch {
		t.Errorf("attempted_at = %v", cached.AttemptedAt)
	}
	// The in-flight marker is released on the way out.
	if _, err := os.Stat(usage.InflightPath(f.jul, email1)); !os.IsNotExist(err) {
		t.Errorf("in-flight marker left behind: %v", err)
	}
}

func TestSendUsageDebounce(t *testing.T) {
	t.Run("identical payload is skipped", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1)
		assertCode(t, f.run("", sendArgs()...), 0)
		assertCode(t, f.run("", sendArgs()...), 0)
		if len(f.fake.Calls) != 1 {
			t.Errorf("calls = %d, want 1", len(f.fake.Calls))
		}
	})
	t.Run("identical payload goes out after 30 minutes", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1)
		assertCode(t, f.run("", sendArgs()...), 0)
		t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+31*60))
		assertCode(t, f.run("", sendArgs()...), 0)
		if len(f.fake.Calls) != 2 {
			t.Errorf("calls = %d, want 2", len(f.fake.Calls))
		}
	})
	t.Run("changed usage waits for the minimum interval", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1)
		assertCode(t, f.run("", sendArgs()...), 0)
		t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+60))
		assertCode(t, f.run("", sendArgs("--session-used", "30")...), 0)
		if len(f.fake.Calls) != 1 {
			t.Errorf("calls = %d, want 1", len(f.fake.Calls))
		}
		t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+301))
		assertCode(t, f.run("", sendArgs("--session-used", "30")...), 0)
		if len(f.fake.Calls) != 2 {
			t.Errorf("calls = %d, want 2", len(f.fake.Calls))
		}
	})
	t.Run("a new window is reported immediately", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1)
		assertCode(t, f.run("", sendArgs()...), 0)
		assertCode(t, f.run("", sendArgs("--session-resets", strconv.FormatInt(fiveReset+18000, 10))...), 0)
		if len(f.fake.Calls) != 2 {
			t.Errorf("calls = %d, want 2", len(f.fake.Calls))
		}
	})
	t.Run("--now bypasses the debounce", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1)
		assertCode(t, f.run("", sendArgs()...), 0)
		assertCode(t, f.run("", sendArgs("--now")...), 0)
		if len(f.fake.Calls) != 2 {
			t.Errorf("calls = %d, want 2", len(f.fake.Calls))
		}
	})
}

func TestSendUsageValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing email", []string{"send-usage"}, "--email must be a valid email address"},
		{"bad email", sendArgs("--email", "nope"), "--email must be a valid email address"},
		{"session over 100", sendArgs("--session-used", "101"), "--session-used must be a percentage in [0,100]"},
		{"week negative", sendArgs("--week-used", "-2"), "--week-used must be a percentage in [0,100]"},
		{"zero session reset", sendArgs("--session-resets", "0"), "--session-resets must be positive"},
		{"negative week reset", sendArgs("--week-resets", "-1"), "--week-resets must be positive"},
		{"zero collected at", sendArgs("--collected-at", "0"), "--collected-at must be positive"},
		{"positional argument", sendArgs("extra"), "no positional arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.save()
			f.share(email1)
			got := f.run("", tc.args...)
			assertCode(t, got, cli.ExitUsage)
			if !strings.Contains(got.stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.want)
			}
			if len(f.fake.Calls) != 0 {
				t.Errorf("invalid input reached the Worker: %+v", f.fake.Calls)
			}
		})
	}
}

func TestSendUsageFailureIsLoggedWithoutEmail(t *testing.T) {
	f := setup(t)
	f.save()
	f.share(email1)
	f.fake.PutUsageErr = &remote.Error{Status: 500, Message: "internal server error for " + email1}

	got := f.run("", sendArgs()...)
	assertCode(t, got, 0) // nothing is watching: never fail loudly
	if got.stdout != "" {
		t.Errorf("stdout = %q", got.stdout)
	}
	log := f.errorLog()
	if !strings.Contains(log, "SEND_FAILED") || !strings.Contains(log, "500") {
		t.Errorf("errors.log = %q", log)
	}
	if strings.Contains(log, email1) {
		t.Errorf("email leaked to errors.log: %q", log)
	}
	// Throttle markers and the sent/ cache are keyed by a hash: no file name
	// anywhere under the julienning home carries the address.
	err := filepath.WalkDir(f.jul, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(filepath.Base(p), "@") {
			t.Errorf("%q carries an address", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The attempt is recorded even though it failed; sent_at stays zero.
	cached := f.sent(email1)
	if cached == nil || cached.AttemptedAt.Unix() != nowEpoch {
		t.Fatalf("attempt not recorded: %+v", cached)
	}
	if !cached.SentAt.IsZero() {
		t.Errorf("sent_at = %v, want zero until a 2xx", cached.SentAt)
	}
}

// An unreachable Worker must converge: one attempt per interval, one log line
// per 10 minutes — not one 5 s process and one SEND_FAILED per status line.
func TestSendUsageUnreachableRemoteConverges(t *testing.T) {
	f := setup(t)
	f.save()
	f.share(email1)
	f.fake.PutUsageErr = errors.New("request failed: connection refused")

	assertCode(t, f.run("", sendArgs()...), 0)
	assertCode(t, f.run("", sendArgs()...), 0) // same second: no second attempt
	if len(f.fake.Calls) != 1 {
		t.Fatalf("attempts = %d, want 1", len(f.fake.Calls))
	}
	if n := strings.Count(f.errorLog(), "SEND_FAILED"); n != 1 {
		t.Fatalf("SEND_FAILED lines = %d, want 1", n)
	}

	t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+301))
	assertCode(t, f.run("", sendArgs()...), 0)
	if len(f.fake.Calls) != 2 {
		t.Errorf("attempts after the interval = %d, want 2", len(f.fake.Calls))
	}
	if n := strings.Count(f.errorLog(), "SEND_FAILED"); n != 1 {
		t.Errorf("SEND_FAILED lines = %d, want 1 (throttled to 10 minutes)", n)
	}

	t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+700))
	assertCode(t, f.run("", sendArgs()...), 0)
	if len(f.fake.Calls) != 3 {
		t.Errorf("attempts = %d, want 3", len(f.fake.Calls))
	}
	if n := strings.Count(f.errorLog(), "SEND_FAILED"); n != 2 {
		t.Errorf("SEND_FAILED lines = %d, want 2 after 10 minutes", n)
	}
}

// A cold cache plus several status line renders used to let every process PUT.
func TestSendUsageInFlightMarkerSerializes(t *testing.T) {
	f := setup(t)
	f.save()
	f.share(email1)
	marker := usage.InflightPath(f.jul, email1)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := time.Unix(nowEpoch, 0)
	if err := os.Chtimes(marker, fresh, fresh); err != nil {
		t.Fatal(err)
	}

	assertCode(t, f.run("", sendArgs()...), 0)
	if len(f.fake.Calls) != 0 {
		t.Errorf("a concurrent report slipped through: %+v", f.fake.Calls)
	}
	if f.errorLog() != "" {
		t.Errorf("losing the race must be quiet: %q", f.errorLog())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the winner's marker was removed: %v", err)
	}

	// A marker left by a killed process must not block reporting forever.
	t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+int(usage.InflightTTL/time.Second)+1))
	assertCode(t, f.run("", sendArgs()...), 0)
	if len(f.fake.Calls) != 1 {
		t.Errorf("stale marker blocked the report: %+v", f.fake.Calls)
	}
}

// A 2xx is what moves sent_at, which is what the 30-minute repeat rule reads.
func TestSendUsageSuccessUpdatesSentAt(t *testing.T) {
	f := setup(t)
	f.save()
	f.share(email1)
	f.fake.PutUsageErr = errors.New("request failed: connection refused")
	assertCode(t, f.run("", sendArgs()...), 0)
	if got := f.sent(email1); got == nil || !got.SentAt.IsZero() {
		t.Fatalf("sent_at = %+v, want zero", got)
	}

	f.fake.PutUsageErr = nil
	t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+301))
	assertCode(t, f.run("", sendArgs()...), 0)
	got := f.sent(email1)
	if got == nil || got.SentAt.Unix() != nowEpoch+301 || got.AttemptedAt.Unix() != nowEpoch+301 {
		t.Fatalf("cache = %+v", got)
	}
	// Now the unchanged payload is debounced for 30 minutes.
	t.Setenv("JULIENNING_NOW_EPOCH", strconv.Itoa(nowEpoch+301+29*60))
	assertCode(t, f.run("", sendArgs()...), 0)
	if len(f.fake.Calls) != 2 {
		t.Errorf("calls = %d, want 2", len(f.fake.Calls))
	}
}

func TestSendUsageWithoutSetup(t *testing.T) {
	f := setup(t) // no config.json
	got := f.run("", sendArgs()...)
	assertCode(t, got, 0)
	if !strings.Contains(f.errorLog(), "NOT_SETUP") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("calls = %+v", f.fake.Calls)
	}
}

func TestSendUsageWithoutRemoteURL(t *testing.T) {
	f := setup(t)
	f.cfg.Remote.URL = ""
	f.save()
	f.share(email1)
	got := f.run("", sendArgs()...)
	assertCode(t, got, 0)
	if !strings.Contains(f.errorLog(), "Worker URL or token not configured") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("calls = %+v", f.fake.Calls)
	}
}

func TestInternalCommandsAreHiddenFromHelp(t *testing.T) {
	f := setup(t)
	got := f.run("", "help")
	assertCode(t, got, 0)
	for _, name := range []string{"send-usage", "hook", "claim-sync"} {
		if strings.Contains(got.stdout, "  "+name+" ") {
			t.Errorf("%s must not be listed: %q", name, got.stdout)
		}
	}
	for _, name := range []string{"current", "accounts", "statusline"} {
		if !strings.Contains(got.stdout, name) {
			t.Errorf("help is missing %q: %q", name, got.stdout)
		}
	}
}

// send-usage is the process that actually sends: it re-checks the allowlist
// instead of trusting its --email argument.
func TestSendUsageRefusesUnsharedAccounts(t *testing.T) {
	f := setup(t)
	f.save()
	f.share(email2)

	got := f.run("", sendArgs()...)
	assertCode(t, got, 0)
	if n := len(f.fake.CallsFor("usage")); n != 0 {
		t.Errorf("usage calls = %d, want 0", n)
	}
	if !strings.Contains(f.errorLog(), "NOT_SHARED") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if f.sent(email1) != nil {
		t.Errorf("debounce cache written for an unsent report")
	}
}

// A 404 "account is not shared" means a teammate unshared it: drop it from
// shared.json so the status line stops reporting it, and say so once.
func TestSendUsageNotSharedAnswerPrunesTheCache(t *testing.T) {
	f := setup(t)
	f.save()
	f.shareNicks(map[string]string{email1: "alpha", email2: "beta"})
	f.fake.PutUsageErr = remote.NotSharedError()

	assertCode(t, f.run("", sendArgs()...), 0)
	c := f.sharedCache()
	if c.Contains(email1) || !c.Contains(email2) {
		t.Errorf("cache = %+v", c)
	}
	if c.Nickname(email1) != "" || c.Nickname(email2) != "beta" {
		t.Errorf("nicknames = %v, want only beta kept", c.Nicknames)
	}
	if _, ok := c.ByNickname("alpha"); ok {
		t.Errorf("the unshared account's nickname still resolves")
	}
	if c.FetchedAt.Unix() != nowEpoch {
		t.Errorf("fetched_at moved: %v", c.FetchedAt)
	}
	log := f.errorLog()
	if strings.Count(log, "NOT_SHARED") != 1 || !strings.Contains(log, "removed it from shared.json") {
		t.Errorf("errors.log = %q", log)
	}
	if strings.Contains(log, "SEND_FAILED") || strings.Contains(log, email1) {
		t.Errorf("errors.log = %q", log)
	}
}

// The reporter reconciles claims at most every 10 minutes, cleaning up after
// sessions whose SessionEnd hook never ran.
func TestSendUsageReconcilesClaimsEveryTenMinutes(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	f.session(cd.Dir, 100, "s1")

	assertCode(t, f.run("", sendArgs()...), 0)
	if f.ops() != "usage,claim" {
		t.Fatalf("ops = %s", f.ops())
	}
	if _, ok := f.held()[email1]; !ok {
		t.Fatalf("claim not recorded")
	}
	if f.ucheck != 1 {
		t.Errorf("update check ran %d times, want 1", f.ucheck)
	}

	f.dead[100] = true // crashed: no SessionEnd
	f.fake.Reset()
	t.Setenv("JULIENNING_NOW_EPOCH", fmt.Sprint(nowEpoch+9*60))
	assertCode(t, f.run("", sendArgs("--now")...), 0)
	if f.ops() != "usage" {
		t.Errorf("ops = %s, want no reconcile before 10 minutes", f.ops())
	}

	f.fake.Reset()
	t.Setenv("JULIENNING_NOW_EPOCH", fmt.Sprint(nowEpoch+10*60))
	assertCode(t, f.run("", sendArgs()...), 0) // debounced report, upkeep still due
	if f.ops() != "unclaim" {
		t.Errorf("ops = %s, want the stale claim released", f.ops())
	}
	if len(f.held()) != 0 {
		t.Errorf("held = %v", f.held())
	}
}

func TestSendUsageRefreshesStaleSharedCache(t *testing.T) {
	f := setup(t)
	f.save()
	f.shareAt(time.Unix(nowEpoch, 0).Add(-2*time.Hour), email1)
	if err := claims.MarkReconciled(time.Unix(nowEpoch, 0)); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()

	assertCode(t, f.run("", sendArgs()...), 0)
	if f.ops() != "usage,list" {
		t.Fatalf("ops = %s", f.ops())
	}
	c := f.sharedCache()
	if !c.Contains(email2) || c.FetchedAt.Unix() != nowEpoch {
		t.Errorf("cache = %+v", c)
	}

	// Fresh now: no second fetch.
	f.fake.Reset()
	assertCode(t, f.run("", sendArgs("--now")...), 0)
	if f.ops() != "usage" {
		t.Errorf("ops = %s", f.ops())
	}
}

func TestSendUsageUpkeepFailuresAreThrottled(t *testing.T) {
	f := setup(t)
	f.save()
	f.shareAt(time.Unix(nowEpoch, 0).Add(-2*time.Hour), email1)
	f.fake.ListErr = errors.New("request failed: connection refused")
	refreshUpdateCheck = func(context.Context) error { return errors.New("github unreachable") }

	assertCode(t, f.run("", sendArgs()...), 0)
	assertCode(t, f.run("", sendArgs("--now")...), 0)
	log := f.errorLog()
	if n := strings.Count(log, "SHARED_REFRESH_FAILED"); n != 1 {
		t.Errorf("SHARED_REFRESH_FAILED lines = %d: %q", n, log)
	}
	if n := strings.Count(log, "UPDATE_CHECK_FAILED"); n != 1 {
		t.Errorf("UPDATE_CHECK_FAILED lines = %d: %q", n, log)
	}
}

// One maintenance run per machine: a held claim-sync lock means someone else
// is doing it right now.
func TestSendUsageSkipsUpkeepWhileLocked(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.shareAt(time.Unix(nowEpoch, 0).Add(-2*time.Hour), email1)
	f.session(cd.Dir, 100, "s1")
	release, ok, err := claims.AcquireLock(time.Unix(nowEpoch, 0))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer release()

	assertCode(t, f.run("", sendArgs()...), 0)
	if f.ops() != "usage" {
		t.Errorf("ops = %s", f.ops())
	}
	if f.ucheck != 0 {
		t.Errorf("update check ran while locked")
	}
}
