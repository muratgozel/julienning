package usageprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

func tp(t time.Time) *time.Time { return &t }

// account is a listed account whose numbers were collected ago before now.
func account(email string, ago time.Duration, now time.Time) remote.Account {
	at := now.Add(-ago)
	reset := now.Add(2 * time.Hour)
	week := now.Add(72 * time.Hour)
	return remote.Account{
		Email:       email,
		Session:     &remote.Window{Used: 10, ResetsAt: &reset},
		Week:        &remote.Window{Used: 20, ResetsAt: &week},
		CollectedAt: &at,
	}
}

func okOut(session, week int) string {
	return usageOut(`[{"kind":"session","percent":` + strconv.Itoa(session) + `,"resets_at":"2026-10-19T13:40:00Z"},` +
		`{"kind":"weekly_all","percent":` + strconv.Itoa(week) + `,"resets_at":"2026-10-24T14:00:00Z"}]`)
}

func probed(log string) bool { return strings.Contains(log, "argv: -p /usage") }

func refreshWith(t *testing.T, fake *remote.Fake, listing *remote.Listing, now time.Time, only string, stale time.Duration) Report {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return Refresh(context.Background(), Options{
		Config: cfg, Client: fake, Listing: listing, Now: now, StaleAfter: stale, Only: only,
	})
}

func TestRefreshProbesOnlyStaleSharedLogins(t *testing.T) {
	h := newHarness(t)
	now := probeNow
	stale := h.dir("j1", "stale@x.io")
	fresh := h.dir("j2", "fresh@x.io")
	reset := h.dir("j3", "reset@x.io")
	never := h.dir("j4", "never@x.io")
	personal := h.dir("j5", "me@personal.io")
	empty := h.dir("j6", "")
	for _, d := range []string{stale, fresh, reset, never, personal} {
		h.write(d, "out.jsonl", okOut(5, 6))
	}
	passed := account("reset@x.io", time.Minute, now)
	passed.Session.ResetPassed = true
	listing := &remote.Listing{Accounts: []remote.Account{
		account("stale@x.io", 20*time.Minute, now),
		account("fresh@x.io", 2*time.Minute, now),
		passed,
		{Email: "never@x.io"},
	}}
	answer := &remote.Listing{Accounts: []remote.Account{{Email: "stale@x.io", Source: "probe"}}}
	fake := &remote.Fake{ProbesListing: answer}

	rep := refreshWith(t, fake, listing, now, "", InteractiveStale)

	for d, want := range map[string]bool{stale: true, fresh: false, reset: true, never: true, personal: false, empty: false} {
		if got := probed(h.log(d)); got != want {
			t.Errorf("%s probed=%v, want %v", filepath.Base(d), got, want)
		}
	}
	if rep.Probed != 3 || len(rep.Problems) != 0 || rep.Listing != answer {
		t.Fatalf("report: %+v", rep)
	}
	calls := fake.CallsFor("probes")
	if len(calls) != 1 || calls[0].Dev != "murat" || calls[0].Identity.MachineID != "3fa9c2d1e07b" {
		t.Fatalf("calls: %+v", calls)
	}
	var emails []string
	for _, r := range calls[0].Probes {
		emails = append(emails, r.Email)
		if r.Session.Used != 5 || r.Week.Used != 6 || !r.CollectedAt.Equal(now.Truncate(time.Second)) {
			t.Errorf("report %+v", r)
		}
	}
	if strings.Join(emails, ",") != "stale@x.io,reset@x.io,never@x.io" {
		t.Fatalf("emails: %v", emails)
	}
	if strings.Contains(strings.Join(emails, ","), "personal") {
		t.Fatal("a personal account must never be reported")
	}
}

func TestRefreshCacheWindowResendsInsteadOfProbing(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "out.jsonl", okOut(5, 6))
	listing := &remote.Listing{Accounts: []remote.Account{account("a@x.io", time.Hour, probeNow)}}
	fake := &remote.Fake{}
	refreshWith(t, fake, listing, probeNow, "", InteractiveStale)
	os.Remove(filepath.Join(d, "probe.log"))

	// Four minutes later the listing still lags (it shows the old numbers):
	// no second probe, but the cached result goes out again.
	fake.Reset()
	rep := refreshWith(t, fake, listing, probeNow.Add(4*time.Minute), "", InteractiveStale)
	if probed(h.log(d)) || rep.Probed != 0 {
		t.Fatal("a login probed less than CacheWindow ago must not be probed again")
	}
	calls := fake.CallsFor("probes")
	if len(calls) != 1 || len(calls[0].Probes) != 1 || calls[0].Probes[0].Session.Used != 5 {
		t.Fatalf("the cached result must be re-sent: %+v", calls)
	}

	// Once the listing shows it, nothing is sent at all.
	shown := account("a@x.io", 0, probeNow)
	fake.Reset()
	refreshWith(t, fake, &remote.Listing{Accounts: []remote.Account{shown}}, probeNow.Add(4*time.Minute), "", InteractiveStale)
	if len(fake.CallsFor("probes")) != 0 {
		t.Fatal("nothing newer than the listing: nothing to send")
	}

	// After the window it is probed again.
	refreshWith(t, fake, listing, probeNow.Add(6*time.Minute), "", InteractiveStale)
	if !probed(h.log(d)) {
		t.Fatal("after CacheWindow the login is probed again")
	}
}

func TestRefreshBackgroundIsLessEager(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "out.jsonl", okOut(5, 6))
	listing := &remote.Listing{Accounts: []remote.Account{account("a@x.io", 30*time.Minute, probeNow)}}
	refreshWith(t, &remote.Fake{}, listing, probeNow, "", BackgroundStale)
	if probed(h.log(d)) {
		t.Fatal("30-minute-old numbers are fresh enough for the background round")
	}
}

func TestRefreshOnly(t *testing.T) {
	h := newHarness(t)
	a := h.dir("j1", "a@x.io")
	b := h.dir("j2", "b@x.io")
	for _, d := range []string{a, b} {
		h.write(d, "out.jsonl", okOut(5, 6))
	}
	listing := &remote.Listing{Accounts: []remote.Account{{Email: "a@x.io"}, {Email: "b@x.io"}}}
	refreshWith(t, &remote.Fake{}, listing, probeNow, "b@x.io", InteractiveStale)
	if probed(h.log(a)) || !probed(h.log(b)) {
		t.Fatal("Only limits the refresh to one account")
	}
}

func TestRefreshSignedOutDirs(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j2", "claude2@x.io")
	h.write(d, "out.jsonl", initLine+"\n")
	h.write(d, "auth", "out")
	listing := &remote.Listing{Accounts: []remote.Account{{Email: "claude2@x.io"}}}
	fake := &remote.Fake{}

	rep := refreshWith(t, fake, listing, probeNow, "", InteractiveStale)
	if len(rep.SignedOut) != 1 || rep.SignedOut[0].Email != "claude2@x.io" || len(fake.CallsFor("probes")) != 0 {
		t.Fatalf("report %+v calls %v", rep, fake.Ops())
	}
	cfg, _ := config.Load()
	if !SignedOutDirs(cfg)[d] || !IsSignedOut(d, "claude2@x.io") {
		t.Fatal("the dir must read as signed out")
	}
	if IsSignedOut(d, "someone-else@x.io") {
		t.Fatal("a mark is for the login it was found with")
	}

	// Signed out is re-checked after the cache window even when a teammate's
	// status line keeps the account's numbers fresh: the user may have signed
	// in again with plain claude.
	h.write(d, "out.jsonl", okOut(1, 2))
	os.Remove(filepath.Join(d, "probe.log"))
	freshListing := &remote.Listing{Accounts: []remote.Account{account("claude2@x.io", time.Minute, probeNow)}}
	refreshWith(t, fake, freshListing, probeNow.Add(6*time.Minute), "", InteractiveStale)
	if !probed(h.log(d)) || IsSignedOut(d, "claude2@x.io") {
		t.Fatal("a signed-out dir is probed again, and a working login clears the mark")
	}
}

func TestSignedOutDirsFromLastEmail(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j3", "claude2@x.io")
	broken := h.dir("j4", "x@x.io")
	if _, err := config.RecordLogins(Logins(mustLoad(t))); err != nil {
		t.Fatal(err)
	}
	// Claude Code signs the dir out and clears oauthAccount.
	h.write(d, ".claude.json", `{"numStartups": 3}`)
	h.write(broken, ".claude.json", `{not json`)
	got := SignedOutDirs(mustLoad(t))
	if !got[d] || got[broken] {
		t.Fatalf("signed out: %v", got)
	}
}

func TestRefreshRecordsLoginChanges(t *testing.T) {
	h := newHarness(t)
	d := h.dir("julienning3", "claude2@x.io")
	listing := &remote.Listing{}
	refreshWith(t, &remote.Fake{}, listing, probeNow, "", InteractiveStale)
	if cd, _ := mustLoad(t).Find("julienning3"); cd.LastEmail != "claude2@x.io" {
		t.Fatalf("last_email: %q", cd.LastEmail)
	}
	h.login(d, "claude9@x.io")
	rep := refreshWith(t, &remote.Fake{}, listing, probeNow, "", InteractiveStale)
	if len(rep.Changes) != 1 || rep.Changes[0].From != "claude2@x.io" || rep.Changes[0].To != "claude9@x.io" {
		t.Fatalf("changes: %+v", rep.Changes)
	}
	if rep := refreshWith(t, &remote.Fake{}, listing, probeNow, "", InteractiveStale); len(rep.Changes) != 0 {
		t.Fatal("a change is reported once")
	}
}

func TestRefreshPostFailures(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "out.jsonl", okOut(5, 6))
	listing := &remote.Listing{Accounts: []remote.Account{{Email: "a@x.io"}}}

	old := &remote.Fake{ProbesErr: &remote.Error{Status: 404, Message: "not found"}}
	rep := refreshWith(t, old, listing, probeNow, "", InteractiveStale)
	if !rep.OldWorker || rep.Listing != listing {
		t.Fatalf("an old Worker: %+v", rep)
	}

	os.RemoveAll(filepath.Join(os.Getenv(config.EnvHome), config.ProbesDir))
	down := &remote.Fake{ProbesErr: errors.New("request timed out after 10s")}
	rep = refreshWith(t, down, listing, probeNow, "", InteractiveStale)
	if rep.PostErr == nil || rep.OldWorker || rep.Listing != listing {
		t.Fatalf("a failed post keeps the listing: %+v", rep)
	}
}

func TestRefreshReportsProblems(t *testing.T) {
	h := newHarness(t)
	d := h.dir("j1", "a@x.io")
	h.write(d, "mode", "exit3")
	rep := refreshWith(t, &remote.Fake{}, &remote.Listing{Accounts: []remote.Account{{Email: "a@x.io"}}}, probeNow, "", InteractiveStale)
	if len(rep.Problems) != 1 || rep.Problems[0].Dir.Name != "j1" || !strings.Contains(rep.Problems[0].Err.Error(), "status 3") {
		t.Fatalf("%+v", rep)
	}
}

func TestStateRoundTripAndGuards(t *testing.T) {
	newHarness(t)
	s := State{Dir: "/d/one", Email: "a@x.io", Outcome: OK, ProbedAt: probeNow, Session: &Window{Used: 1, ResetsAt: tp(probeNow)}, Week: &Window{Used: 2}}
	if err := SaveState(s); err != nil {
		t.Fatal(err)
	}
	got := LoadState("/d/one/")
	if got == nil || got.Session.Used != 1 || got.Week.ResetsAt != nil {
		t.Fatalf("%+v", got)
	}
	p, _ := StatePath("/d/one")
	if strings.Contains(filepath.Base(p), "one") {
		t.Fatal("state files are named by a hash")
	}
	os.WriteFile(p, []byte(`{"dir":"/d/other","outcome":"ok"}`), 0o600)
	if LoadState("/d/one") != nil {
		t.Fatal("a file for another dir must not be trusted")
	}
	os.WriteFile(p, []byte(`{nope`), 0o600)
	if LoadState("/d/one") != nil {
		t.Fatal("a corrupt file reads as never probed")
	}
	SaveState(State{Dir: "/d/one", Email: "a@x.io", Outcome: Failed, ProbedAt: probeNow})
	ClearSignedOut("/d/one")
	if LoadState("/d/one") == nil {
		t.Fatal("ClearSignedOut keeps other outcomes")
	}
	SaveState(State{Dir: "/d/one", Email: "a@x.io", Outcome: SignedOut, ProbedAt: probeNow})
	if err := ClearSignedOut("/d/one"); err != nil || LoadState("/d/one") != nil {
		t.Fatal("ClearSignedOut forgets a signed-out mark")
	}
}

func mustLoad(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
