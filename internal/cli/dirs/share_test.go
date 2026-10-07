package dirs

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func TestShareAddsAndRefreshesCache(t *testing.T) {
	h := newHarness(t)
	one := h.loggedIn(".claude-one", "new@team.io")
	cfg := h.initConfig(true, config.ConfigDir{Name: "one", Dir: one})
	cfg.Decline("new@team.io")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	h.allow("old@team.io", "new@team.io")

	out := h.mustRun(runShare, "New@Team.io")
	contains(t, out, "Shared new@team.io with the team as new.\n")
	contains(t, out, "Logged in here as: one.")
	if ops := h.fake.Ops(); !reflect.DeepEqual(ops, []string{"share", "list"}) {
		t.Fatalf("ops = %v", ops)
	}
	if c := h.fake.Calls[0]; c.Email != "new@team.io" || c.Nickname != "new" || c.Identity.MachineID != cfg.MachineID {
		t.Fatalf("share call = %+v", c)
	}
	cache, _ := sharedcache.Load()
	if !reflect.DeepEqual(cache.Emails, []string{"new@team.io", "old@team.io"}) || cache.Nickname("new@team.io") != "new" {
		t.Fatalf("cache = %+v", cache)
	}
	if h.config().Declined("new@team.io") {
		t.Fatal("explicit share left the email declined")
	}
}

func TestShareFallsBackToLocalCacheUpdate(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.cacheEmails("old@team.io")
	h.fake.ListErr = errors.New("request timed out after 10s")
	out := h.mustRun(runShare, "x@team.io")
	contains(t, out, "Shared x@team.io")
	contains(t, h.stderr.String(), "could not refresh the allowlist")
	cache, _ := sharedcache.Load()
	if !cache.Contains("x@team.io") || !cache.Contains("old@team.io") {
		t.Fatalf("cache = %v", cache.Emails)
	}
}

func TestShareErrors(t *testing.T) {
	h := newHarness(t)
	var ue *usageErr
	if !asUsage(h.run(runShare), &ue) || !asUsage(h.run(runShare, "not-an-email"), &ue) || !asUsage(h.run(runShare, "a@b.io", "c@d.io"), &ue) {
		t.Fatal("want usage errors")
	}
	if err := h.run(runShare, "a@b.io"); err == nil || !strings.Contains(err.Error(), "julienning setup") {
		t.Fatalf("not set up: %v", err)
	}
	h.initConfig(false)
	if err := h.run(runShare, "a@b.io"); !errors.Is(err, config.ErrRemoteNotConfigured) {
		t.Fatalf("no remote: %v", err)
	}
	h.initConfig(true)
	h.fake.ShareErr = &remote.Error{Status: 401, Message: "unauthorized"}
	if err := h.run(runShare, "a@b.io"); !errors.Is(err, errTokenRejected) {
		t.Fatalf("401: %v", err)
	}
	h.fake.ShareErr = &remote.Error{Status: 400, Message: "invalid email"}
	if err := h.run(runShare, "a@b.io"); err == nil || err.Error() != "share a@b.io: 400 invalid email" {
		t.Fatalf("400: %v", err)
	}
}

func TestUnshareConfirmation(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.cacheEmails("gone@team.io", "stay@team.io")

	err := h.run(runUnshare, "gone@team.io")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive without --yes: %v", err)
	}
	h.interactive = true
	h.stdin = strings.NewReader("n\n")
	out := h.mustRun(runUnshare, "gone@team.io")
	contains(t, out, "Unshare gone@team.io? This deletes its usage and claims for the whole team. [y/N] ")
	contains(t, out, "Cancelled")
	if len(h.fake.Calls) != 0 {
		t.Fatalf("Worker called after a no: %v", h.fake.Ops())
	}

	h.stdin = strings.NewReader("y\n")
	h.allow("stay@team.io")
	out = h.mustRun(runUnshare, "gone@team.io")
	contains(t, out, "Unshared gone@team.io")
	if ops := h.fake.Ops(); !reflect.DeepEqual(ops, []string{"unshare", "list"}) {
		t.Fatalf("ops = %v", ops)
	}
	cache, _ := sharedcache.Load()
	if !reflect.DeepEqual(cache.Emails, []string{"stay@team.io"}) {
		t.Fatalf("cache = %v", cache.Emails)
	}
}

func TestUnshareYesAndLocalFallback(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.cacheEmails("gone@team.io", "stay@team.io")
	h.fake.ListErr = errors.New("request failed")
	h.mustRun(runUnshare, "--yes", "gone@team.io")
	cache, _ := sharedcache.Load()
	if !reflect.DeepEqual(cache.Emails, []string{"stay@team.io"}) {
		t.Fatalf("cache = %v", cache.Emails)
	}
	h.fake.UnshareErr = &remote.Error{Status: 403, Message: "forbidden"}
	if err := h.run(runUnshare, "gone@team.io", "--yes"); !errors.Is(err, errTokenRejected) {
		t.Fatalf("got %v", err)
	}
}
