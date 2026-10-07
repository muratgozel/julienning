package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/muratgozel/julienning/internal/cli/dirs" // new-config, for the end-to-end flow
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

// markShareOnLogin gives a registered dir of f.cfg new-config's pending share.
func (f *fixture) markShareOnLogin(name, nick string) {
	f.t.Helper()
	for i := range f.cfg.Configs {
		if f.cfg.Configs[i].Name == name {
			f.cfg.Configs[i].ShareOnLogin = &config.ShareOnLogin{Nickname: nick}
			return
		}
	}
	f.t.Fatalf("config %q not registered", name)
}

// pendingOnDisk reads the dir's mark back from config.json.
func (f *fixture) pendingOnDisk(name string) *config.ShareOnLogin {
	f.t.Helper()
	cfg, err := config.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	cd, ok := cfg.Find(name)
	if !ok {
		f.t.Fatalf("config %q not registered", name)
	}
	return cd.ShareOnLogin
}

// The SessionStart gate lets a dir with a pending share through although
// its email is not in shared.json: claim-sync shares it first.
func TestHookSessionStartSpawnsForPendingShare(t *testing.T) {
	t.Run("not shared yet", func(t *testing.T) {
		f := setup(t)
		cd := f.addConfig("julienning1", email2)
		f.markShareOnLogin("julienning1", "")
		f.save()
		f.share(email1) // the allowlist knows other accounts only
		f.useDir(cd.Dir)
		spawn := f.captureSpawn()
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if want := "claim-sync --starting 5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11"; spawn.calls != 1 || strings.Join(spawn.args, " ") != want {
			t.Fatalf("spawn = %+v, want %s", spawn, want)
		}
		if f.errorLog() != "" {
			t.Errorf("errors.log = %q", f.errorLog())
		}
	})
	t.Run("unreadable shared cache", func(t *testing.T) {
		f := setup(t)
		cd := f.addConfig("julienning1", email2)
		f.markShareOnLogin("julienning1", "")
		f.save()
		if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.useDir(cd.Dir)
		spawn := f.captureSpawn()
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 1 {
			t.Fatalf("spawn calls = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("not logged in", func(t *testing.T) {
		f := setup(t)
		cd := f.addConfig("julienning1", "")
		f.markShareOnLogin("julienning1", "")
		f.save()
		f.useDir(cd.Dir)
		spawn := f.captureSpawn()
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || f.errorLog() != "" {
			t.Fatalf("spawn calls = %d, errors.log = %q; nothing to share before the login", spawn.calls, f.errorLog())
		}
	})
	t.Run("other dirs stay gated", func(t *testing.T) {
		f := setup(t)
		f.addConfig("julienning1", "")
		f.markShareOnLogin("julienning1", "")
		cd := f.addConfig("julienning2", email2) // unmarked, not shared
		f.save()
		f.useDir(cd.Dir)
		spawn := f.captureSpawn()
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Fatalf("spawn calls = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
}

// claim-sync shares a pending login before reconciling, so the new account
// is claimed by the same run.
func TestClaimSyncSharesPendingLogin(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("julienning1", email2)
	f.markShareOnLogin("julienning1", "delta")
	f.save()
	f.share(email1)
	f.session(cd.Dir, 100, "s1")
	f.useDir(cd.Dir)

	got := f.run("", "claim-sync", "--starting", "s1")
	assertCode(t, got, 0)
	if got.stdout != "" || got.stderr != "" {
		t.Errorf("claim-sync must stay silent: %q %q", got.stdout, got.stderr)
	}
	if f.ops() != "list,share,claim" {
		t.Fatalf("ops = %s, want list,share,claim", f.ops())
	}
	if c := f.fake.CallsFor("share")[0]; c.Email != email2 || c.Nickname != "delta" {
		t.Fatalf("share = %+v", c)
	}
	if got := f.sharedCache().Nickname(email2); got != "delta" {
		t.Fatalf("cached nickname = %q", got)
	}
	if got := f.pendingOnDisk("julienning1"); got != nil {
		t.Fatalf("mark not cleared: %+v", got)
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

// A taken nickname keeps the mark and leaves one throttled, email-free
// SHARE_FAILED line; the claim reconciliation still runs.
func TestClaimSyncPendingShareFailureIsLogged(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("julienning1", email2)
	f.markShareOnLogin("julienning1", "delta")
	f.save()
	f.useDir(cd.Dir)
	f.fake.ShareErr = &remote.Error{Status: 409, Message: "nickname is taken"}

	assertCode(t, f.run("", "claim-sync"), 0)
	assertCode(t, f.run("", "claim-sync"), 0)
	log := f.errorLog()
	if n := strings.Count(log, "SHARE_FAILED"); n != 1 {
		t.Fatalf("SHARE_FAILED lines = %d: %q", n, log)
	}
	if !strings.Contains(log, `config julienning1: nickname taken: "delta" belongs to another team account; pick another with: julienning setup`) {
		t.Errorf("errors.log = %q", log)
	}
	if strings.Contains(log, "@") {
		t.Errorf("email leaked: %q", log)
	}
	if n := len(f.fake.CallsFor("share")); n != 2 {
		t.Errorf("share attempts = %d, want one per run", n)
	}
	if got := f.pendingOnDisk("julienning1"); got == nil || got.Nickname != "delta" {
		t.Fatalf("mark = %+v, want it kept", got)
	}
}

// `accounts` reconciles through claims.Sync, which settles pending shares.
func TestAccountsSharesPendingLogin(t *testing.T) {
	const fresh = "claude9@sixtynine.agency"
	f := setup(t)
	f.addConfig("julienning1", fresh)
	f.markShareOnLogin("julienning1", "")
	f.save()
	f.fake.Listing = listing()

	assertCode(t, f.run("", "accounts"), 0)
	shares := f.fake.CallsFor("share")
	if len(shares) != 1 || shares[0].Email != fresh || shares[0].Nickname != "claude9" {
		t.Fatalf("shares = %+v", shares)
	}
	if got := f.pendingOnDisk("julienning1"); got != nil {
		t.Fatalf("mark not cleared: %+v", got)
	}
}

// End to end through cli.Main: new-config marks the dir, the user signs in,
// the SessionStart hook spawns claim-sync, which shares the account under
// the chosen nickname; the status line then reports it.
func TestNewConfigSharesOnFirstSession(t *testing.T) {
	f := setup(t)
	f.save()
	spawn := f.captureSpawn()

	got := f.run("", "new-config", "--nick", "delta", "--no-login")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, "The account you sign in with will be shared with the team as delta;") {
		t.Fatalf("new-config stdout = %q", got.stdout)
	}
	dir := filepath.Join(f.home, ".claude-julienning1")
	if p := f.pendingOnDisk("julienning1"); p == nil || p.Nickname != "delta" {
		t.Fatalf("mark = %+v", p)
	}

	// Signing in writes the account file; Claude then runs the hook.
	f.writeAccount(dir, email2)
	f.useDir(dir)
	assertCode(t, f.run(payload(""), "statusline"), 0)
	if spawn.calls != 0 {
		t.Fatalf("the status line reported an account that is not shared yet: %v", spawn.args)
	}
	assertSilent(t, f, f.run(startInput, "hook", "session-start"))
	if spawn.calls != 1 || spawn.args[0] != "claim-sync" {
		t.Fatalf("hook spawn = %+v", spawn)
	}

	// The detached child, run in-process.
	assertCode(t, f.run("", spawn.args...), 0)
	shares := f.fake.CallsFor("share")
	if len(shares) != 1 || shares[0].Email != email2 || shares[0].Nickname != "delta" {
		t.Fatalf("shares = %+v", shares)
	}
	if p := f.pendingOnDisk("julienning1"); p != nil {
		t.Fatalf("mark not cleared: %+v", p)
	}
	if got := f.sharedCache().Nickname(email2); got != "delta" {
		t.Fatalf("cached nickname = %q", got)
	}
	if log := f.errorLog(); strings.Contains(log, "SHARE_FAILED") || strings.Contains(log, "CLAIM_SYNC_FAILED") {
		t.Fatalf("errors.log = %q", log)
	}

	// Now shared: the status line starts reporting usage.
	assertCode(t, f.run(payload(""), "statusline"), 0)
	if spawn.calls != 2 || spawn.args[0] != "send-usage" {
		t.Fatalf("status line spawn = %+v", spawn)
	}
}
