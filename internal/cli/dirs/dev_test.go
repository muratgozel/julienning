package dirs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/remote"
)

// devFixture: a shared account logged into a registered dir with a live
// session, claimed by this machine under the name murat.
func devFixture(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	dir := h.loggedIn(".claude-one", "claude1@team.io")
	h.initConfig(true, config.ConfigDir{Name: "julienning1", Dir: dir})
	h.cacheNicks("claude1@team.io=alpha")
	h.writeFile(filepath.Join(dir, "sessions", "4242.json"), `{"pid":4242,"sessionId":"s1","cwd":"/tmp/p","kind":"interactive"}`)
	prev := livesess.Alive
	livesess.Alive = func(livesess.Session) bool { return true }
	t.Cleanup(func() { livesess.Alive = prev })
	if err := claims.SaveHeld(claims.Held{"claude1@team.io": fixedNow}); err != nil {
		t.Fatal(err)
	}
	return h
}

func claimOps(f *remote.Fake) string {
	var out []string
	for _, c := range f.Calls {
		if c.Op == "claim" || c.Op == "unclaim" {
			out = append(out, fmt.Sprintf("%s %s as %s/%s", c.Op, c.Email, c.Identity.Dev, c.Identity.MachineID))
		}
	}
	return strings.Join(out, "; ")
}

func TestDevPrintsTheName(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	if out := h.mustRun(runDev); out != "murat\n" {
		t.Fatalf("got %q", out)
	}
}

// The claims held under the old name are released and taken again under
// the new one, so teammates never see the old name on an account in use.
func TestDevRenameMovesTheClaims(t *testing.T) {
	h := devFixture(t)
	id := h.config().MachineID
	out := h.mustRun(runDev, "  Ali ")
	if out != "Your dev name is now ali (was murat).\n" {
		t.Fatalf("got %q", out)
	}
	if got, want := claimOps(h.fake), "unclaim claude1@team.io as murat/"+id+"; claim claude1@team.io as ali/"+id; got != want {
		t.Errorf("claims:\n got %s\nwant %s", got, want)
	}
	if got := h.config().Dev; got != "ali" {
		t.Errorf("config.json dev = %q", got)
	}
	if held, err := claims.LoadHeld(); err != nil || len(held) != 1 {
		t.Errorf("claims.json after the rename: %v %v", held, err)
	}
	if h.stderr.Len() != 0 {
		t.Errorf("unexpected warnings: %s", h.stderr.String())
	}
	if out := h.mustRun(runDev, "ali"); out != "Your dev name is already ali.\n" {
		t.Errorf("unchanged: %q", out)
	}
}

// With the Worker down the rename still happens; the old claims expire with
// the Worker's claim TTL, and the next claim sync claims under the new name
// (claims.json no longer says the account is held).
func TestDevRenameWithTheWorkerDown(t *testing.T) {
	h := devFixture(t)
	down := errors.New("dial tcp: connection refused")
	h.fake.DeleteClaimErr, h.fake.PutClaimErr = down, down
	out := h.mustRun(runDev, "ali")
	contains(t, out, "Your dev name is now ali (was murat).")
	contains(t, h.stderr.String(), "warning: could not release 1 claim held as murat (release claims: dial tcp: connection refused); the Worker drops them when its claim TTL runs out (12 h by default)")
	contains(t, h.stderr.String(), "warning: could not claim the accounts in use here as ali yet")
	if got := h.config().Dev; got != "ali" {
		t.Errorf("config.json dev = %q", got)
	}
	if held, err := claims.LoadHeld(); err != nil || len(held) != 0 {
		t.Errorf("claims.json must not keep the old name's claims: %v %v", held, err)
	}
}

func TestDevRenameWaitsForTheClaimLock(t *testing.T) {
	h := devFixture(t)
	release, ok, err := claims.AcquireLock(fixedNow)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer release()
	err = h.run(runDev, "ali")
	if err == nil || err.Error() != "another julienning is syncing claims right now; try again in a moment" {
		t.Fatalf("got %v", err)
	}
	if got := h.config().Dev; got != "murat" {
		t.Errorf("renamed without the lock: %q", got)
	}
	if len(h.fake.Calls) != 0 {
		t.Errorf("Worker called without the lock: %v", h.fake.Ops())
	}
}

func TestDevBadInput(t *testing.T) {
	h := newHarness(t)
	if err := h.run(runDev); !errors.Is(err, config.ErrNotSetup) {
		t.Errorf("not set up: %v", err)
	}
	h.initConfig(false)
	var ue *usageErr
	for _, args := range [][]string{{"a b"}, {"-x"}, {"a", "b"}, {strings.Repeat("a", 33)}} {
		if err := h.run(runDev, args...); !asUsage(err, &ue) {
			t.Errorf("%q: want a usage error, got %v", args, err)
		}
	}
}

// setup --dev on a machine already set up renames through the same steps.
func TestSetupDevRenamesThroughDev(t *testing.T) {
	h := devFixture(t)
	h.allow("claude1@team.io")
	out := h.mustRun(runSetup, "--dev", "ali")
	contains(t, out, "Identity: dev=ali machine="+h.config().MachineID+" (renamed from murat)")
	if got := claimOps(h.fake); !strings.HasPrefix(got, "unclaim claude1@team.io as murat/") || !strings.Contains(got, "claim claude1@team.io as ali/") {
		t.Errorf("claims: %s", got)
	}
	if got := h.config().Dev; got != "ali" {
		t.Errorf("config.json dev = %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.jl, claims.LockFile)); !os.IsNotExist(err) {
		t.Errorf("the claim-sync lock was left behind: %v", err)
	}
}
