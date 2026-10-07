package claims

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

const (
	email1 = "claude1@sixtynine.agency"
	email2 = "claude2@sixtynine.agency"
	nowSec = 1789482657 // 2026-09-15T14:30:57Z
)

var now = time.Unix(nowSec, 0).UTC()

type env struct {
	t    *testing.T
	home string
	jul  string
	cfg  *config.Config
	fake *remote.Fake
	dead map[int]bool // pids livesess.Alive reports as gone
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{
		t:    t,
		home: filepath.Join(root, "home"),
		jul:  filepath.Join(root, "julienning"),
		fake: &remote.Fake{},
		dead: map[int]bool{},
		cfg:  &config.Config{Version: config.SchemaVersion, Dev: "murat", MachineID: "3fa9c2d1e07b"},
	}
	if err := os.MkdirAll(e.home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv(config.EnvHome, e.jul)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	prev := livesess.Alive
	livesess.Alive = func(s livesess.Session) bool { return !e.dead[s.PID] }
	t.Cleanup(func() { livesess.Alive = prev })
	return e
}

// dir registers a config dir, logs email into it ("" = not logged in) and
// gives it an empty session registry unless registry is false.
func (e *env) dir(name, email string, registry bool) string {
	e.t.Helper()
	d := filepath.Join(e.home, ".claude-"+name)
	if name == "default" {
		d = filepath.Join(e.home, ".claude")
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if registry {
		if err := os.MkdirAll(filepath.Join(d, "sessions"), 0o700); err != nil {
			e.t.Fatal(err)
		}
	}
	if email != "" {
		e.login(d, email)
	}
	if err := e.cfg.Add(config.ConfigDir{Name: name, Dir: d}); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) login(dir, email string) {
	e.t.Helper()
	p := filepath.Join(dir, ".claude.json")
	if filepath.Base(dir) == ".claude" {
		p = filepath.Join(filepath.Dir(dir), ".claude.json") // default-dir rule
	}
	body := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) session(dir string, pid int, id string) {
	e.t.Helper()
	e.entry(dir, pid, fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":"/tmp/p","kind":"interactive"}`, pid, id))
}

// entry writes a raw registry entry for pid.
func (e *env) entry(dir string, pid int, body string) {
	e.t.Helper()
	reg := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(reg, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reg, fmt.Sprintf("%d.json", pid)), []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) share(emails ...string) {
	e.t.Helper()
	if _, err := sharedcache.Save(emails, now); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) held() Held {
	e.t.Helper()
	h, err := LoadHeld()
	if err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *env) hold(emails ...string) {
	e.t.Helper()
	h := Held{}
	for _, m := range emails {
		h[m] = now
	}
	if err := SaveHeld(h); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reconcile(ending string, at time.Time) error {
	e.t.Helper()
	return Reconcile(context.Background(), e.cfg, e.fake, Ending{SessionID: ending}, at)
}

func (e *env) ops() string {
	return strings.Join(e.fake.Ops(), ",")
}

func TestReconcileClaimsAccountWithLiveSession(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")
	e.share(email1)

	if err := e.reconcile("", now); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if e.ops() != "claim" {
		t.Fatalf("ops = %s", e.ops())
	}
	c := e.fake.Calls[0]
	if c.Email != email1 || *c.Identity != (remote.Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}) {
		t.Errorf("claim = %+v", c)
	}
	if got := e.held()[email1]; !got.Equal(now) {
		t.Errorf("held = %v, want %v", got, now)
	}
}

// Every PUT is a KV write: a fresh record is trusted, a stale one re-sent.
func TestReconcileRefreshesOnlyStaleClaims(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")
	e.share(email1)
	e.hold(email1)

	if err := e.reconcile("", now.Add(ClaimRefresh-time.Second)); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "" {
		t.Fatalf("fresh claim re-sent: %s", e.ops())
	}
	later := now.Add(ClaimRefresh)
	if err := e.reconcile("", later); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "claim" {
		t.Fatalf("stale claim not refreshed: %s", e.ops())
	}
	if got := e.held()[email1]; !got.Equal(later) {
		t.Errorf("held = %v, want %v", got, later)
	}
	// A record from the future (clock moved back) is refreshed, not trusted.
	e.fake.Reset()
	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "claim" {
		t.Errorf("future record trusted: %s", e.ops())
	}
}

func TestReconcileReleasesWhenTheLastSessionEnds(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")
	e.share(email1)
	e.hold(email1)

	// SessionEnd: the process is still alive, so the id must be excluded.
	if err := e.reconcile("s1", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "unclaim" {
		t.Fatalf("ops = %s", e.ops())
	}
	if c := e.fake.Calls[0]; c.Identity.MachineID != "3fa9c2d1e07b" || c.Dev != "murat" {
		t.Errorf("unclaim identity = %+v", c.Identity)
	}
	if _, ok := e.held()[email1]; ok {
		t.Errorf("record kept after release")
	}
	// Nothing held and nothing live: no calls at all.
	e.dead[100] = true
	e.fake.Reset()
	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "" {
		t.Errorf("idle, unheld account touched the Worker: %s", e.ops())
	}
}

// A crashed session (SessionEnd never ran) is cleaned up once its process is
// gone.
func TestReconcileReleasesAfterACrash(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")
	e.share(email1)
	e.hold(email1)
	e.dead[100] = true

	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "unclaim" {
		t.Errorf("ops = %s", e.ops())
	}
}

func TestReconcileCountsEveryDirOfTheAccount(t *testing.T) {
	e := newEnv(t)
	a := e.dir("sixtynine1", email1, true)
	b := e.dir("sixtynine9", email1, true)
	e.session(a, 100, "s1")
	e.session(b, 200, "s2")
	e.share(email1)
	e.hold(email1)

	// One of two sessions ends: still in use.
	if err := e.reconcile("s1", now.Add(ClaimRefresh)); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "claim" {
		t.Fatalf("ops = %s, want the claim kept (refreshed)", e.ops())
	}
	e.dead[200] = true
	e.fake.Reset()
	if err := e.reconcile("s1", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "unclaim" {
		t.Errorf("ops = %s", e.ops())
	}
}

// Personal usage must never leave the machine: an unshared email is never
// sent, however many sessions it has.
func TestReconcileNeverClaimsUnsharedEmails(t *testing.T) {
	e := newEnv(t)
	d := e.dir("personal", "me@personal.com", true)
	e.session(d, 100, "s1")
	e.share(email1)

	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Calls) != 0 {
		t.Errorf("unshared email reached the Worker: %+v", e.fake.Calls)
	}
	if len(e.held()) != 0 {
		t.Errorf("held = %v", e.held())
	}
}

// A missing cache is an empty allowlist: nothing is claimed.
func TestReconcileWithoutSharedCache(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")

	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Calls) != 0 {
		t.Errorf("claimed without an allowlist: %+v", e.fake.Calls)
	}
}

func TestReconcileReleasesHeldEmailsThatLeft(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")
	e.dir("sixtynine2", "", true)
	e.share(email2) // email1 was unshared by a teammate
	e.hold(email1, email2)

	// email1: no longer shared; email2: no longer logged in anywhere.
	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "unclaim,unclaim" {
		t.Fatalf("ops = %s", e.ops())
	}
	if e.fake.Calls[0].Email != email1 || e.fake.Calls[1].Email != email2 {
		t.Errorf("calls = %+v", e.fake.Calls)
	}
	if len(e.held()) != 0 {
		t.Errorf("held = %v", e.held())
	}
}

// No registry means "unknown", never "idle": the claim survives.
func TestReconcileKeepsClaimWhenADirHasNoRegistry(t *testing.T) {
	e := newEnv(t)
	e.dir("sixtynine1", email1, true)
	e.dir("legacy", email1, false)
	e.share(email1)
	e.hold(email1)

	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "" {
		t.Errorf("ops = %s, want nothing", e.ops())
	}
	if _, ok := e.held()[email1]; !ok {
		t.Errorf("held claim dropped")
	}
	// A live session elsewhere still claims.
	e.hold()
	e.session(filepath.Join(e.home, ".claude-sixtynine1"), 100, "s1")
	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "claim" {
		t.Errorf("ops = %s", e.ops())
	}
}

// An account file that cannot be read may hide the email a held claim
// belongs to: keep claims rather than release them on a guess.
func TestReconcileUnreadableAccountFileBlocksReleases(t *testing.T) {
	e := newEnv(t)
	e.dir("sixtynine1", email1, true)
	broken := e.dir("broken", "", true)
	if err := os.WriteFile(filepath.Join(broken, ".claude.json"), []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.share(email1, email2)
	e.hold(email1, email2)

	err := e.reconcile("", now)
	if err == nil || !strings.Contains(err.Error(), "config broken") {
		t.Fatalf("err = %v, want the broken config reported", err)
	}
	if e.ops() != "" {
		t.Errorf("ops = %s, want no releases", e.ops())
	}
	if len(e.held()) != 2 {
		t.Errorf("held = %v", e.held())
	}
}

func TestReconcileNotSharedAnswers(t *testing.T) {
	e := newEnv(t)
	a := e.dir("sixtynine1", email1, true)
	e.session(a, 100, "s1")
	e.dir("sixtynine2", email2, true)
	// Nicknamed cache: pruning one email must keep the other's nickname.
	if _, err := sharedcache.SaveEntries([]sharedcache.Entry{{Email: email1, Nickname: "alpha"}, {Email: email2, Nickname: "beta"}}, now); err != nil {
		t.Fatal(err)
	}
	e.hold(email2)
	e.fake.ErrFor = func(op, email string) error { return remote.NotSharedError() }

	if err := e.reconcile("", now); err != nil {
		t.Fatalf("not-shared answers are not failures: %v", err)
	}
	if e.ops() != "claim,unclaim" {
		t.Fatalf("ops = %s", e.ops())
	}
	if len(e.held()) != 0 {
		t.Errorf("held = %v, want both records dropped", e.held())
	}
	// The allowlist moved: the rejected claim's email leaves shared.json, and
	// the refresh schedule is kept.
	c, err := sharedcache.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Contains(email1) || !c.Contains(email2) || !c.FetchedAt.Equal(now) {
		t.Errorf("cache = %+v", c)
	}
	if c.Nickname(email1) != "" || c.Nickname(email2) != "beta" {
		t.Errorf("nicknames = %v, want only beta kept", c.Nicknames)
	}
}

func TestReconcileContinuesPastErrorsWithoutLeakingEmails(t *testing.T) {
	e := newEnv(t)
	a := e.dir("sixtynine1", email1, true)
	b := e.dir("sixtynine2", email2, true)
	e.session(a, 100, "s1")
	e.session(b, 200, "s2")
	e.share(email1, email2)
	e.fake.ErrFor = func(op, email string) error {
		if email == email1 {
			return &remote.Error{Status: 503, Message: "service unavailable"}
		}
		return nil
	}

	err := e.reconcile("", now)
	if err == nil || !strings.Contains(err.Error(), "claim sixtynine1: 503 service unavailable") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "@") {
		t.Errorf("error carries an email (it goes to errors.log): %v", err)
	}
	if e.ops() != "claim,claim" {
		t.Errorf("ops = %s", e.ops())
	}
	h := e.held()
	if _, ok := h[email1]; ok {
		t.Errorf("failed claim recorded")
	}
	if _, ok := h[email2]; !ok {
		t.Errorf("successful claim not recorded")
	}

	// A failed release keeps the record so the next run retries.
	e.hold(email1)
	e.dead[100] = true
	e.fake.Reset()
	e.fake.ErrFor = func(op, email string) error { return errors.New("request failed: connection refused") }
	if err := e.reconcile("", now); err == nil || !strings.Contains(err.Error(), "release claim sixtynine1") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.held()[email1]; !ok {
		t.Errorf("record dropped after a failed release")
	}
}

func TestReconcileCorruptState(t *testing.T) {
	t.Run("shared cache", func(t *testing.T) {
		e := newEnv(t)
		d := e.dir("sixtynine1", email1, true)
		e.session(d, 100, "s1")
		e.hold(email2)
		p, _ := config.Path(sharedcache.File)
		if err := os.WriteFile(p, []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.reconcile("", now); err == nil {
			t.Fatal("want an error")
		}
		if len(e.fake.Calls) != 0 {
			t.Errorf("acted on an unknown allowlist: %+v", e.fake.Calls)
		}
		if _, ok := e.held()[email2]; !ok {
			t.Errorf("claims.json changed")
		}
	})
	t.Run("claims file", func(t *testing.T) {
		e := newEnv(t)
		d := e.dir("sixtynine1", email1, true)
		e.session(d, 100, "s1")
		e.share(email1)
		p, _ := config.Path(File)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := e.reconcile("", now)
		if err == nil || !strings.Contains(err.Error(), "starting over") {
			t.Fatalf("err = %v", err)
		}
		if e.ops() != "claim" {
			t.Errorf("ops = %s", e.ops())
		}
		if _, ok := e.held()[email1]; !ok {
			t.Errorf("claims.json not rewritten")
		}
	})
}

// The default dir's login lives in ~/.claude.json, not ~/.claude/.claude.json.
func TestReconcileDefaultDir(t *testing.T) {
	e := newEnv(t)
	d := e.dir("default", email1, true)
	e.session(d, 100, "s1")
	e.share(email1)
	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "claim" {
		t.Errorf("ops = %s", e.ops())
	}
}

func TestReleaseAll(t *testing.T) {
	e := newEnv(t)
	e.hold(email1, email2)
	e.fake.ErrFor = func(op, email string) error {
		if email == email2 {
			return errors.New("request failed")
		}
		return nil
	}
	if err := ReleaseAll(context.Background(), e.cfg, e.fake); err == nil {
		t.Fatal("want the failure reported")
	}
	h := e.held()
	if _, ok := h[email1]; ok {
		t.Errorf("released claim kept")
	}
	if _, ok := h[email2]; !ok {
		t.Errorf("failed release dropped")
	}
}

func TestHeldRoundTripAndMissingFile(t *testing.T) {
	newEnv(t)
	h, err := LoadHeld()
	if err != nil || len(h) != 0 {
		t.Fatalf("missing file: %v %v", h, err)
	}
	if err := SaveHeld(Held{email1: now}); err != nil {
		t.Fatal(err)
	}
	h, err = LoadHeld()
	if err != nil || !h[email1].Equal(now) {
		t.Fatalf("round trip: %v %v", h, err)
	}
	p, _ := config.Path(File)
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi, err)
	}
}

func TestAcquireLock(t *testing.T) {
	newEnv(t)
	release, ok, err := AcquireLock(now)
	if err != nil || !ok {
		t.Fatalf("first acquire: %v %v", ok, err)
	}
	if _, ok, err := AcquireLock(now.Add(LockStale - time.Second)); err != nil || ok {
		t.Fatalf("second acquire while held: %v %v", ok, err)
	}
	// A crashed holder's lock is broken after LockStale.
	release2, ok, err := AcquireLock(now.Add(LockStale))
	if err != nil || !ok {
		t.Fatalf("stale lock not broken: %v %v", ok, err)
	}
	release2()
	release() // the first holder's late release must not panic or error
	p, _ := config.Path(LockFile)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
	if _, ok, _ := AcquireLock(now); !ok {
		t.Errorf("released lock cannot be taken again")
	}
}

func TestAcquireLockUnwritableHome(t *testing.T) {
	e := newEnv(t)
	ro := filepath.Join(e.home, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvHome, filepath.Join(ro, "julienning"))
	if _, ok, err := AcquireLock(now); err == nil || ok {
		t.Errorf("want an error, got ok=%v err=%v", ok, err)
	}
}

func TestReconcileMarker(t *testing.T) {
	newEnv(t)
	if !ReconcileDue(now) {
		t.Fatal("never reconciled must be due")
	}
	if err := MarkReconciled(now); err != nil {
		t.Fatal(err)
	}
	if ReconcileDue(now.Add(ReconcileEvery - time.Second)) {
		t.Errorf("due too early")
	}
	if !ReconcileDue(now.Add(ReconcileEvery)) {
		t.Errorf("not due after ReconcileEvery")
	}
	if !ReconcileDue(now.Add(-time.Minute)) {
		t.Errorf("a marker from the future must not suppress reconciliation")
	}
}

// Claude registers daemons, daemon workers and pre-spawned spares next to
// real sessions; none of them may hold (or keep) a claim.
func TestReconcileCountsOnlyUserSessions(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.entry(d, 101, `{"pid":101,"sessionId":"d1","kind":"daemon"}`)
	e.entry(d, 102, `{"pid":102,"sessionId":"d2","kind":"daemon-worker"}`)
	e.entry(d, 103, `{"pid":103,"sessionId":"d3","kind":"interactive","spare":true}`)
	e.share(email1)

	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "" {
		t.Fatalf("background processes claimed the account: %s", e.ops())
	}
	e.hold(email1)
	if err := e.reconcile("", now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "unclaim" {
		t.Fatalf("background processes kept the claim: ops = %s", e.ops())
	}

	// bg sessions and entries without a kind (older Claude versions) count.
	for pid, body := range map[int]string{
		104: `{"pid":104,"sessionId":"b1","kind":"bg"}`,
		105: `{"pid":105,"sessionId":"o1"}`,
	} {
		t.Run(fmt.Sprint(pid), func(t *testing.T) {
			e.fake.Reset()
			e.hold()
			e.entry(d, pid, body)
			defer os.Remove(filepath.Join(d, "sessions", fmt.Sprintf("%d.json", pid)))
			if err := e.reconcile("", now); err != nil {
				t.Fatal(err)
			}
			if e.ops() != "claim" {
				t.Errorf("ops = %s, want claim", e.ops())
			}
		})
	}
}

// A SessionEnd hook that could not learn its session id names the ending
// registry entry by pid instead.
func TestReconcileExcludesTheEndingPID(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")
	e.share(email1)
	e.hold(email1)

	err := Reconcile(context.Background(), e.cfg, e.fake, Ending{PID: 999}, now)
	if err != nil || e.ops() != "" {
		t.Fatalf("an unrelated pid released the claim: err=%v ops=%s", err, e.ops())
	}
	if err := Reconcile(context.Background(), e.cfg, e.fake, Ending{PID: 100}, now); err != nil {
		t.Fatal(err)
	}
	if e.ops() != "unclaim" {
		t.Errorf("ops = %s, want unclaim", e.ops())
	}
}

func TestSyncTakesTheLock(t *testing.T) {
	e := newEnv(t)
	d := e.dir("sixtynine1", email1, true)
	e.session(d, 100, "s1")
	e.share(email1)

	release, ok, err := AcquireLock(now)
	if err != nil || !ok {
		t.Fatal(err)
	}
	ran, err := Sync(context.Background(), e.cfg, e.fake, now)
	if ran || err != nil || len(e.fake.Calls) != 0 {
		t.Fatalf("ran while locked: ran=%v err=%v calls=%v", ran, err, e.fake.Calls)
	}
	release()

	ran, err = Sync(context.Background(), e.cfg, e.fake, now)
	if !ran || err != nil || e.ops() != "claim" {
		t.Fatalf("ran=%v err=%v ops=%s", ran, err, e.ops())
	}
	if ReconcileDue(now) {
		t.Errorf("Sync did not stamp the reconcile marker")
	}
	p, _ := config.Path(LockFile)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("lock not released: %v", err)
	}
}
