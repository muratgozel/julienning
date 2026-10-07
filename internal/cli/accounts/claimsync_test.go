package accounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

// claimSyncFixture wires one registered, shared dir with a live session.
func claimSyncFixture(t *testing.T) (*fixture, config.ConfigDir) {
	t.Helper()
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	f.session(cd.Dir, 100, "s1")
	f.useDir(cd.Dir)
	return f, cd
}

func TestClaimSyncClaimsLiveAccounts(t *testing.T) {
	f, _ := claimSyncFixture(t)
	got := f.run("", "claim-sync")
	assertCode(t, got, 0)
	if got.stdout != "" || got.stderr != "" {
		t.Errorf("claim-sync is detached and must stay silent: %q %q", got.stdout, got.stderr)
	}
	if f.ops() != "claim" {
		t.Fatalf("ops = %s", f.ops())
	}
	if c := f.fake.Calls[0]; c.Email != email1 || *c.Identity != (remote.Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}) {
		t.Errorf("claim = %+v", c)
	}
	if _, ok := f.held()[email1]; !ok {
		t.Errorf("claim not recorded")
	}
	if claims.ReconcileDue(time.Unix(nowEpoch, 0)) {
		t.Errorf("reconcile marker not stamped")
	}
	if f.ucheck != 1 {
		t.Errorf("update check ran %d times", f.ucheck)
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	p, _ := config.Path(claims.LockFile)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

func TestClaimSyncEndingReleases(t *testing.T) {
	f, _ := claimSyncFixture(t)
	assertCode(t, f.run("", "claim-sync"), 0)
	f.fake.Reset()

	assertCode(t, f.run("", "claim-sync", "--ending", "s1"), 0)
	if f.ops() != "unclaim" {
		t.Errorf("ops = %s", f.ops())
	}
	if len(f.held()) != 0 {
		t.Errorf("held = %v", f.held())
	}
}

// The SessionEnd hook's fallback when it could not learn the session id.
func TestClaimSyncEndingPIDReleases(t *testing.T) {
	f, _ := claimSyncFixture(t)
	assertCode(t, f.run("", "claim-sync"), 0)
	f.fake.Reset()

	assertCode(t, f.run("", "claim-sync", "--ending-pid", "999"), 0)
	if f.ops() != "" {
		t.Fatalf("an unrelated pid released the claim: %s", f.ops())
	}
	assertCode(t, f.run("", "claim-sync", "--ending-pid", "100"), 0)
	if f.ops() != "unclaim" || len(f.held()) != 0 {
		t.Errorf("ops = %s, held = %v", f.ops(), f.held())
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

// Daemons and spares are registered like sessions but must not hold claims.
func TestClaimSyncIgnoresBackgroundProcesses(t *testing.T) {
	f, cd := claimSyncFixture(t)
	for pid, body := range map[int]string{
		100: `{"pid":100,"sessionId":"s1","kind":"daemon"}`,
		101: `{"pid":101,"sessionId":"s2","kind":"daemon-worker"}`,
		102: `{"pid":102,"sessionId":"s3","kind":"interactive","spare":true}`,
	} {
		p := filepath.Join(cd.Dir, "sessions", fmt.Sprintf("%d.json", pid))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	assertCode(t, f.run("", "claim-sync"), 0)
	if f.ops() != "" || len(f.held()) != 0 {
		t.Errorf("ops = %s, held = %v", f.ops(), f.held())
	}
}

func TestClaimSyncRefreshesStaleSharedCache(t *testing.T) {
	f, _ := claimSyncFixture(t)
	f.shareAt(time.Unix(nowEpoch, 0).Add(-2*time.Hour), email1)
	f.fake.Listing = listing()

	assertCode(t, f.run("", "claim-sync"), 0)
	if f.ops() != "claim,list" {
		t.Fatalf("ops = %s, want reconcile then refresh", f.ops())
	}
	if c := f.sharedCache(); !c.Contains(email2) || c.FetchedAt.Unix() != nowEpoch {
		t.Errorf("cache = %+v", c)
	}
}

func TestClaimSyncLockHeldExitsQuietly(t *testing.T) {
	f, _ := claimSyncFixture(t)
	release, ok, err := claims.AcquireLock(time.Unix(nowEpoch, 0))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer release()

	got := f.run("", "claim-sync")
	assertCode(t, got, 0)
	if len(f.fake.Calls) != 0 || f.errorLog() != "" {
		t.Errorf("calls = %+v, errors.log = %q", f.fake.Calls, f.errorLog())
	}
}

// A SessionEnd right after a SessionStart must not be dropped because the
// start's run still holds the lock.
func TestClaimSyncWaitsForTheLock(t *testing.T) {
	f, _ := claimSyncFixture(t)
	lockWait, pollEvery = 5*time.Second, 10*time.Millisecond
	release, ok, err := claims.AcquireLock(time.Unix(nowEpoch, 0))
	if err != nil || !ok {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
		close(done)
	}()

	assertCode(t, f.run("", "claim-sync"), 0)
	<-done
	if f.ops() != "claim" {
		t.Errorf("ops = %s, want the run to proceed once the lock is free", f.ops())
	}
}

// Whether Claude registers a session before or after its SessionStart hooks
// is undocumented: claim-sync waits briefly for it.
func TestClaimSyncStartingWaitsForTheRegistry(t *testing.T) {
	f, cd := claimSyncFixture(t)
	if err := os.Remove(filepath.Join(cd.Dir, "sessions", "100.json")); err != nil {
		t.Fatal(err)
	}
	startWait, pollEvery = 5*time.Second, 10*time.Millisecond
	done := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.session(cd.Dir, 200, "s2")
		close(done)
	}()

	assertCode(t, f.run("", "claim-sync", "--starting", "s2"), 0)
	<-done
	if f.ops() != "claim" {
		t.Errorf("ops = %s, want the late-registered session claimed", f.ops())
	}
}

// Without a registry there is nothing to wait for.
func TestClaimSyncStartingWithoutRegistryDoesNotWait(t *testing.T) {
	f, cd := claimSyncFixture(t)
	if err := os.RemoveAll(filepath.Join(cd.Dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	startWait = 5 * time.Second
	start := time.Now()
	assertCode(t, f.run("", "claim-sync", "--starting", "s2"), 0)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v on a dir without a registry", d)
	}
}

func TestClaimSyncFailuresAreLoggedThrottledWithoutEmails(t *testing.T) {
	f, _ := claimSyncFixture(t)
	f.fake.PutClaimErr = &remote.Error{Status: 503, Message: "service unavailable"}

	assertCode(t, f.run("", "claim-sync"), 0)
	assertCode(t, f.run("", "claim-sync"), 0)
	log := f.errorLog()
	if n := strings.Count(log, "CLAIM_SYNC_FAILED"); n != 1 {
		t.Errorf("CLAIM_SYNC_FAILED lines = %d: %q", n, log)
	}
	if !strings.Contains(log, "claim sixtynine1: 503 service unavailable") {
		t.Errorf("errors.log = %q", log)
	}
	if strings.Contains(log, "@") {
		t.Errorf("email leaked: %q", log)
	}
}

func TestClaimSyncNotSetUp(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		f := setup(t)
		assertCode(t, f.run("", "claim-sync"), 0)
		if !strings.Contains(f.errorLog(), "NOT_SETUP") || len(f.fake.Calls) != 0 {
			t.Errorf("errors.log = %q, calls = %+v", f.errorLog(), f.fake.Calls)
		}
		p, _ := config.Path(claims.LockFile)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("lock taken without a config: %v", err)
		}
	})
	t.Run("no remote", func(t *testing.T) {
		f := setup(t)
		f.cfg.Remote.Token = ""
		f.save()
		assertCode(t, f.run("", "claim-sync"), 0)
		if !strings.Contains(f.errorLog(), "Worker URL or token not configured") || len(f.fake.Calls) != 0 {
			t.Errorf("errors.log = %q, calls = %+v", f.errorLog(), f.fake.Calls)
		}
	})
}

func TestClaimSyncInvalidArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":     {"claim-sync", "--bogus"},
		"positional":       {"claim-sync", "extra"},
		"hostile id":       {"claim-sync", "--ending", "../x"},
		"missing value":    {"claim-sync", "--ending"},
		"negative pid":     {"claim-sync", "--ending-pid", "-1"},
		"pid not a number": {"claim-sync", "--ending-pid", "abc"},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := claimSyncFixture(t)
			got := f.run("", args...)
			assertCode(t, got, 0)
			if got.stdout != "" || got.stderr != "" {
				t.Errorf("output: %q %q", got.stdout, got.stderr)
			}
			if len(f.fake.Calls) != 0 || !strings.Contains(f.errorLog(), "claim-sync: invalid arguments") {
				t.Errorf("calls = %+v, errors.log = %q", f.fake.Calls, f.errorLog())
			}
		})
	}
}

func TestClaimSyncUpdateCheckFailureIsLogged(t *testing.T) {
	f, _ := claimSyncFixture(t)
	refreshUpdateCheck = func(context.Context) error { return errors.New("github unreachable") }
	assertCode(t, f.run("", "claim-sync"), 0)
	if !strings.Contains(f.errorLog(), "UPDATE_CHECK_FAILED") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}
