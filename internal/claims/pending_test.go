package claims

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// mark gives a registered dir a pending share and saves config.json, which
// ResolvePendingShares re-reads before clearing a mark.
func (e *env) mark(name, nick string) {
	e.t.Helper()
	for i := range e.cfg.Configs {
		if e.cfg.Configs[i].Name == name {
			e.cfg.Configs[i].ShareOnLogin = &config.ShareOnLogin{Nickname: nick}
		}
	}
	if err := e.cfg.Save(); err != nil {
		e.t.Fatal(err)
	}
}

// pending reports the mark of name in config.json and in e.cfg, which must
// agree.
func (e *env) pending(name string) *config.ShareOnLogin {
	e.t.Helper()
	disk, err := config.Load()
	if err != nil {
		e.t.Fatal(err)
	}
	onDisk, _ := disk.Find(name)
	inMem, _ := e.cfg.Find(name)
	if !reflect.DeepEqual(onDisk.ShareOnLogin, inMem.ShareOnLogin) {
		e.t.Fatalf("%s: config.json has %+v, memory has %+v", name, onDisk.ShareOnLogin, inMem.ShareOnLogin)
	}
	return onDisk.ShareOnLogin
}

func (e *env) resolve() error {
	return ResolvePendingShares(context.Background(), e.cfg, e.fake, now)
}

func cached(t *testing.T) *sharedcache.Cache {
	t.Helper()
	c, err := sharedcache.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResolvePendingSharesSharesTheLogin(t *testing.T) {
	e := newEnv(t)
	e.dir("julienning1", email1, true)
	e.dir("julienning2", "John+Team@x.io", true)
	e.dir("julienning3", email2, true) // not marked: never shared here
	e.mark("julienning1", "delta")
	e.mark("julienning2", "")
	// Another command saves config.json after this process loaded it; the
	// cleared marks must not undo that.
	other, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	other.NamePrefix = "team"
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	if err := e.resolve(); err != nil {
		t.Fatal(err)
	}
	if got := e.ops(); got != "list,share,share" {
		t.Fatalf("Worker calls = %s, want one listing, then the two shares", got)
	}
	shares := e.fake.CallsFor("share")
	if got := []string{shares[0].Email + "=" + shares[0].Nickname, shares[1].Email + "=" + shares[1].Nickname}; !reflect.DeepEqual(got, []string{email1 + "=delta", "john+team@x.io=john-team"}) {
		t.Fatalf("shares = %v", got)
	}
	if id := shares[0].Identity; id == nil || *id != (remote.Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}) {
		t.Fatalf("share identity = %+v", id)
	}
	c := cached(t)
	if c.Nickname(email1) != "delta" || c.Nickname("john+team@x.io") != "john-team" || c.Contains(email2) {
		t.Fatalf("shared.json = %+v", c)
	}
	// The listing refreshed the cache, so claim-sync's own refresh does not
	// replace it with a listing that may lag the shares.
	if !c.FetchedAt.Equal(now) {
		t.Fatalf("fetched_at = %v, want %v", c.FetchedAt, now)
	}
	for _, n := range []string{"julienning1", "julienning2"} {
		if got := e.pending(n); got != nil {
			t.Errorf("%s: mark not cleared: %+v", n, got)
		}
	}
	if disk, _ := config.Load(); disk.NamePrefix != "team" {
		t.Fatalf("a concurrent config change was lost: name_prefix = %q", disk.NamePrefix)
	}

	// Nothing pending: no Worker calls at all.
	e.fake.Reset()
	if err := e.resolve(); err != nil || e.ops() != "" {
		t.Fatalf("second run: err %v, calls %s", err, e.ops())
	}
}

func TestResolvePendingSharesAlreadyShared(t *testing.T) {
	t.Run("cache", func(t *testing.T) {
		e := newEnv(t)
		e.dir("julienning1", email1, true)
		e.mark("julienning1", "delta")
		e.share(email1)
		if err := e.resolve(); err != nil {
			t.Fatal(err)
		}
		if got := e.ops(); got != "" {
			t.Fatalf("Worker calls = %s, want none", got)
		}
		if got := e.pending("julienning1"); got != nil {
			t.Fatalf("mark not cleared: %+v", got)
		}
	})
	t.Run("listing", func(t *testing.T) {
		e := newEnv(t)
		e.dir("julienning1", email1, true)
		e.mark("julienning1", "delta")
		e.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Email: email1, Nickname: "alpha"}}}
		if err := e.resolve(); err != nil {
			t.Fatal(err)
		}
		if got := e.ops(); got != "list" {
			t.Fatalf("Worker calls = %s, want only the listing", got)
		}
		if got := cached(t).Nickname(email1); got != "alpha" {
			t.Fatalf("cached nickname = %q, want the team's alpha", got)
		}
		if got := e.pending("julienning1"); got != nil {
			t.Fatalf("mark not cleared: %+v", got)
		}
	})
}

// Failures keep the mark for the next run, and their messages, which go to
// errors.log, name the config dir and never the email.
func TestResolvePendingSharesFailuresKeepTheMark(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"conflict": {&remote.Error{Status: 409, Message: "nickname is taken"}, `config julienning1: nickname taken: "delta" belongs to another team account; pick another with: julienning setup`},
		"network":  {errors.New("request failed: connection refused"), `config julienning1: share its account as "delta": request failed: connection refused`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.dir("julienning1", email1, true)
			e.mark("julienning1", "delta")
			e.fake.ShareErr = c.err

			err := e.resolve()
			if err == nil || err.Error() != c.want {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), email1) {
				t.Fatalf("error leaks the email: %v", err)
			}
			if got := e.pending("julienning1"); !reflect.DeepEqual(got, &config.ShareOnLogin{Nickname: "delta"}) {
				t.Fatalf("mark = %+v, want it kept", got)
			}
			if cached(t).Contains(email1) {
				t.Fatal("a failed share reached shared.json")
			}

			// The next run retries and settles it.
			e.fake.ShareErr = nil
			if err := e.resolve(); err != nil {
				t.Fatal(err)
			}
			if n := len(e.fake.CallsFor("share")); n != 2 {
				t.Fatalf("%d share calls, want the retry", n)
			}
			if got := e.pending("julienning1"); got != nil {
				t.Fatalf("mark not cleared after the retry: %+v", got)
			}
		})
	}
}

func TestResolvePendingSharesNotLoggedInWaits(t *testing.T) {
	e := newEnv(t)
	e.dir("julienning1", "", true)
	e.mark("julienning1", "")
	if err := e.resolve(); err != nil {
		t.Fatal(err)
	}
	if got := e.ops(); got != "" {
		t.Fatalf("Worker calls = %s, want none", got)
	}
	if got := e.pending("julienning1"); got == nil {
		t.Fatal("mark dropped before the login")
	}
}

func TestResolvePendingSharesUnreadableAccountKeepsTheMark(t *testing.T) {
	e := newEnv(t)
	d := e.dir("julienning1", "", true)
	if err := os.WriteFile(filepath.Join(d, ".claude.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.mark("julienning1", "")
	err := e.resolve()
	if err == nil || !strings.HasPrefix(err.Error(), "config julienning1: parse ") {
		t.Fatalf("err = %v", err)
	}
	if got := e.pending("julienning1"); got == nil {
		t.Fatal("mark dropped")
	}
}

// An account this machine's user declined as personal is never published by
// a team dir it was signed into by mistake.
func TestResolvePendingSharesNeverSharesDeclined(t *testing.T) {
	e := newEnv(t)
	e.dir("julienning1", email1, true)
	e.cfg.Decline(email1)
	e.mark("julienning1", "")
	err := e.resolve()
	if err == nil || !strings.Contains(err.Error(), "config julienning1: its account was not shared because it is marked personal") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), email1) {
		t.Fatalf("error leaks the email: %v", err)
	}
	if n := len(e.fake.CallsFor("share")); n != 0 {
		t.Fatalf("%d share calls for a declined account", n)
	}
	if got := e.pending("julienning1"); got != nil {
		t.Fatalf("mark kept: %+v", got)
	}
}

// Sync shares first, so the account is claimed in the same run.
func TestSyncSharesPendingBeforeReconciling(t *testing.T) {
	e := newEnv(t)
	d := e.dir("julienning1", email1, true)
	e.session(d, 100, "s1")
	e.mark("julienning1", "delta")

	ran, err := Sync(context.Background(), e.cfg, e.fake, now)
	if !ran || err != nil {
		t.Fatalf("ran %v, err %v", ran, err)
	}
	if got := e.ops(); got != "list,share,claim" {
		t.Fatalf("Worker calls = %s, want list,share,claim", got)
	}
	if _, ok := e.held()[email1]; !ok {
		t.Fatal("not recorded as held")
	}
}
