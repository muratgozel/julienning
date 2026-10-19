package accounts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/usageprobe"
)

func probedListing(used float64) *remote.Listing {
	reset := time.Unix(fiveReset, 0).UTC()
	week := time.Unix(friReset, 0).UTC()
	at := time.Unix(nowEpoch, 0).UTC()
	return &remote.Listing{Accounts: []remote.Account{{
		Rank: 1, Email: email1, Nickname: "alpha", State: "free", Source: "probe",
		Session:     &remote.Window{Used: used, ResetsAt: &reset},
		Week:        &remote.Window{Used: 3, ResetsAt: &week},
		CollectedAt: &at,
	}}}
}

func TestAccountsShowsTheRefreshedListing(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	f.fake.Listing = &remote.Listing{Accounts: []remote.Account{{Rank: 1, Email: email1, Nickname: "alpha", State: "free"}}}
	calls := stubRefresh(t, func(o usageprobe.Options) usageprobe.Report {
		return usageprobe.Report{
			Listing:  probedListing(42),
			Problems: []usageprobe.Problem{{Dir: cd, Email: email1, Err: errors.New("claude did not answer within 10s")}},
		}
	})

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if len(*calls) != 1 {
		t.Fatalf("refreshes: %d", len(*calls))
	}
	o := (*calls)[0]
	if o.Listing != f.fake.Listing || o.StaleAfter != usageprobe.InteractiveStale || o.Only != "" || o.Config.Dev != "murat" {
		t.Fatalf("options: %+v", o)
	}
	if !strings.Contains(got.stdout, "42% → 20:00") || !strings.Contains(got.stdout, "0s ago") {
		t.Errorf("the refreshed numbers must be shown:\n%s", got.stdout)
	}
	if !strings.Contains(got.stderr, "julienning: warning: could not refresh the usage of alpha ("+email1+") in ~/.claude-sixtynine1 (claude did not answer within 10s); its numbers are from 0s ago") {
		t.Errorf("stderr: %q", got.stderr)
	}
}

func TestAccountsShowsSignedOutDirs(t *testing.T) {
	f := setup(t)
	gone := f.addConfig("sixtynine", "")
	stale := f.addConfig("sixtynine2", email2)
	f.addConfig("sixtynine1", email1)
	// Claude Code cleared the first dir's login; julienning saw it last.
	for i := range f.cfg.Configs {
		if f.cfg.Configs[i].Dir == gone.Dir {
			f.cfg.Configs[i].LastEmail = email1
		}
	}
	f.save()
	f.share(email1, email2)
	if err := usageprobe.SaveState(usageprobe.State{Dir: stale.Dir, Email: email2, Outcome: usageprobe.SignedOut, ProbedAt: time.Unix(nowEpoch, 0)}); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = &remote.Listing{Accounts: []remote.Account{
		{Rank: 1, Email: email1, Nickname: "alpha", State: "free"},
		{Rank: 2, Email: email2, Nickname: "beta", State: "free"},
	}}

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	lines := strings.Split(got.stdout, "\n")
	if !strings.Contains(lines[1], "~/.claude-sixtynine1,~/.claude-sixtynine (signed out)") {
		t.Errorf("alpha row: %q", lines[1])
	}
	if !strings.Contains(lines[2], "  ~/.claude-sixtynine2 (signed out)") {
		t.Errorf("beta row: %q", lines[2])
	}

	got = f.run("", "accounts", "--json")
	assertCode(t, got, 0)
	var doc struct {
		Accounts []struct {
			Email            string   `json:"email"`
			LocalConfigs     []string `json:"local_configs"`
			SignedOutConfigs []string `json:"signed_out_configs"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	a, b := doc.Accounts[0], doc.Accounts[1]
	if strings.Join(a.LocalConfigs, ",") != "sixtynine1" || strings.Join(a.SignedOutConfigs, ",") != "sixtynine" {
		t.Errorf("alpha: %+v", a)
	}
	if len(b.LocalConfigs) != 0 || strings.Join(b.SignedOutConfigs, ",") != "sixtynine2" {
		t.Errorf("beta: %+v", b)
	}
}

func TestAccountsPrintsLoginChanges(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine", email2)
	f.save()
	f.share(email1, email2)
	f.fake.Listing = listing()
	stubRefresh(t, func(o usageprobe.Options) usageprobe.Report {
		return usageprobe.Report{Listing: o.Listing, Changes: []config.LoginChange{{Name: cd.Name, Dir: cd.Dir, From: email1, To: email2}}}
	})
	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if !strings.Contains(got.stderr, "julienning: note: ~/.claude-sixtynine was logged into") ||
		!strings.Contains(got.stderr, "the sessions in it stay there") {
		t.Errorf("stderr: %q", got.stderr)
	}
}

func TestSendUsageProbesIdleAccountsOnItsHourlyRefresh(t *testing.T) {
	f := setup(t)
	f.save()
	f.shareAt(time.Unix(nowEpoch, 0).Add(-2*time.Hour), email1)
	if err := claims.MarkReconciled(time.Unix(nowEpoch, 0)); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing()
	calls := stubRefresh(t, func(o usageprobe.Options) usageprobe.Report { return usageprobe.Report{Listing: o.Listing} })

	assertCode(t, f.run("", sendArgs()...), 0)
	if len(*calls) != 1 || (*calls)[0].StaleAfter != usageprobe.BackgroundStale || (*calls)[0].Listing != f.fake.Listing || !(*calls)[0].Background {
		t.Fatalf("the background round must reuse the hourly listing: %+v", *calls)
	}
	if f.ops() != "usage,list" {
		t.Errorf("no extra list for the probes: ops = %s", f.ops())
	}

	// shared.json is fresh now: no listing, no round.
	assertCode(t, f.run("", sendArgs("--now")...), 0)
	if len(*calls) != 1 {
		t.Fatalf("a round needs the hourly listing: %d", len(*calls))
	}
}

func TestClaimSyncProbesIdleAccountsOnItsHourlyRefresh(t *testing.T) {
	f, _ := claimSyncFixture(t)
	f.shareAt(time.Unix(nowEpoch, 0).Add(-2*time.Hour), email1)
	f.fake.Listing = listing()
	calls := stubRefresh(t, func(o usageprobe.Options) usageprobe.Report { return usageprobe.Report{Listing: o.Listing} })
	assertCode(t, f.run("", "claim-sync"), 0)
	if len(*calls) != 1 || (*calls)[0].StaleAfter != usageprobe.BackgroundStale {
		t.Fatalf("calls: %+v", *calls)
	}
}

func TestBackgroundProbeProblemsAreLoggedWithoutEmails(t *testing.T) {
	f, _ := claimSyncFixture(t)
	f.shareAt(time.Unix(nowEpoch, 0).Add(-2*time.Hour), email1)
	f.fake.Listing = listing()
	cd := config.ConfigDir{Name: "sixtynine1", Dir: f.home + "/.claude-sixtynine1"}
	stubRefresh(t, func(o usageprobe.Options) usageprobe.Report {
		return usageprobe.Report{
			Listing:   o.Listing,
			Problems:  []usageprobe.Problem{{Dir: cd, Email: email1, Err: errors.New("could not reach " + email1)}},
			SignedOut: []usageprobe.Problem{{Dir: cd, Email: email1}},
		}
	})
	assertCode(t, f.run("", "claim-sync"), 0)
	log := f.errorLog()
	if !strings.Contains(log, "PROBE_FAILED") || !strings.Contains(log, "usage probe of config sixtynine1 failed") || !strings.Contains(log, "SIGNED_OUT") {
		t.Errorf("errors.log:\n%s", log)
	}
	if strings.Contains(log, "@") {
		t.Errorf("errors.log must stay free of emails:\n%s", log)
	}
}

func TestSendUsageClearsItsDirsSignedOutMark(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	t.Setenv("CLAUDE_CONFIG_DIR", cd.Dir)
	if err := usageprobe.SaveState(usageprobe.State{Dir: cd.Dir, Email: email1, Outcome: usageprobe.SignedOut, ProbedAt: time.Unix(nowEpoch, 0)}); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.run("", sendArgs()...), 0)
	if usageprobe.IsSignedOut(cd.Dir, email1) {
		t.Fatal("a status line report proves the login works")
	}
}

func TestWindowNotRunningShowsJustThePercent(t *testing.T) {
	n := time.Unix(nowEpoch, 0)
	if got := window(&remote.Window{Used: 0}, n, location()); got != "0%" {
		t.Fatalf("a window a probe found not running: %q", got)
	}
	reset := n.Add(time.Hour)
	if got := window(&remote.Window{Used: 5, ResetsAt: &reset}, n, location()); got != "5% → 18:30" {
		t.Fatalf("got %q", got)
	}
}
