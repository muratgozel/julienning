package switching

import (
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/usageprobe"
)

func TestNextRanksOnTheRefreshedListing(t *testing.T) {
	f := setup(t)
	f.addConfig("julienning1", email1)
	cd2 := f.addConfig("julienning2", email2)
	f.save()
	// The Worker's stale numbers make email1 look emptiest; the probes found
	// a teammate filled it outside julienning.
	f.fake.Listing = listing(acct(email1, 5, 5), acct(email2, 50, 60))
	calls := stubRefresh(t, func(o usageprobe.Options) usageprobe.Report {
		return usageprobe.Report{Listing: listing(acct(email2, 50, 60), acct(email1, 95, 99))}
	})

	got := f.run("next")
	assertCode(t, got, 0)
	if len(*calls) != 1 || (*calls)[0].Only != "" || (*calls)[0].StaleAfter != usageprobe.InteractiveStale {
		t.Fatalf("refresh calls: %+v", *calls)
	}
	if f.current() != cd2.Dir || !strings.Contains(got.stdout, "Now using julienning2 (claude2@x.io) — session 50%") {
		t.Errorf("next must rank on the probes: current %q\n%s", f.current(), got.stdout)
	}
}

func TestNextSkipsSignedOutDirs(t *testing.T) {
	f := setup(t)
	cd1 := f.addConfig("julienning1", email1)
	cd2 := f.addConfig("julienning2", email2)
	f.save()
	if err := usageprobe.SaveState(usageprobe.State{Dir: cd1.Dir, Email: email1, Outcome: usageprobe.SignedOut, ProbedAt: nowT}); err != nil {
		t.Fatal(err)
	}
	f.fake.Listing = listing(acct(email1, 5, 5), acct(email2, 50, 60))
	got := f.run("next")
	assertCode(t, got, 0)
	if f.current() != cd2.Dir {
		t.Errorf("a signed-out dir must not be picked: current %q", f.current())
	}
}

func TestUseRefreshesOnlyTheTarget(t *testing.T) {
	f := setup(t)
	f.addConfig("julienning1", email1)
	f.addConfig("julienning2", email2)
	f.save()
	f.fake.Listing = listing(acct(email1, 5, 5), acct(email2, 50, 60))
	calls := stubRefresh(t, func(o usageprobe.Options) usageprobe.Report {
		return usageprobe.Report{Listing: listing(acct(email1, 5, 5), acct(email2, 77, 60))}
	})
	got := f.run("use", "julienning2")
	assertCode(t, got, 0)
	if len(*calls) != 1 || (*calls)[0].Only != email2 {
		t.Fatalf("refresh calls: %+v", *calls)
	}
	if !strings.Contains(got.stdout, "session 77%") {
		t.Errorf("the summary must show the refreshed numbers: %q", got.stdout)
	}
}

func TestUseDoesNotProbePersonalOrUnsharedAccounts(t *testing.T) {
	f := setup(t)
	f.addConfig("personal", personal)
	f.addConfig("julienning1", email1)
	f.save()
	f.fake.Listing = listing(acct(email1, 5, 5))
	calls := stubRefresh(t, func(o usageprobe.Options) usageprobe.Report { return usageprobe.Report{Listing: o.Listing} })
	assertCode(t, f.run("use", personal), 0)
	assertCode(t, f.run("use", "personal"), 0)
	if len(*calls) != 0 {
		t.Fatalf("an account off the allowlist must never be probed: %+v", *calls)
	}

	f.fake.ListErr = &remote.Error{Status: 500, Message: "internal error"}
	assertCode(t, f.run("use", "julienning1"), 0)
	if len(*calls) != 0 {
		t.Fatal("offline (no listing), nothing is probed")
	}
}
