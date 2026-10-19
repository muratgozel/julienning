package switching

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/sharedcache"
)

func TestChooseRanksLikeNextAndSelects(t *testing.T) {
	f := setup(t)
	a := f.addConfig("julienning1", email1)
	b := f.addConfig("julienning2", email2)
	f.addConfig("mine", personal)
	f.save()
	busy := acct(email1, 90, 50)
	busy.BusyBy = []string{"ali"}
	f.fake.Listing = listing(acct(email2, 10, 20), busy)
	f.fake.Listing.Accounts[0].Nickname = "bravo"
	f.fake.Listing.Accounts[1].Nickname = "alpha"

	c, err := Choose("", location())
	if err != nil {
		t.Fatal(err)
	}
	if c.Config.Dir != b.Dir || c.Email != email2 || c.Nickname != "bravo" || c.Account == nil || len(c.Warnings) != 0 {
		t.Fatalf("ranked: %+v", c)
	}
	if f.current() != b.Dir {
		t.Error("the chosen account did not become the selection")
	}
	line := MovingLine(c, "~/.claude-julienning2", "on box", nowT, location())
	if line != "Moving into bravo (claude2@x.io) in ~/.claude-julienning2 on box — session 10% → 20:00, week 20% → Fri 20:00." {
		t.Errorf("moving line: %s", line)
	}

	// TARGET picks like use; a busy one is not warned about (use does not).
	c, err = Choose("alpha", location())
	if err != nil || c.Config.Dir != a.Dir {
		t.Fatalf("target: %+v %v", c, err)
	}

	// A personal account, by email or by the config name of its dir, is
	// refused: transfer only moves sessions between team accounts.
	var pe *PersonalError
	if _, err := Choose(personal, location()); !errors.As(err, &pe) || err.Error() != "transfer moves sessions between team accounts only; me@personal.io is personal" {
		t.Errorf("personal email: %v", err)
	}
	if _, err := Choose("mine", location()); !errors.As(err, &pe) {
		t.Errorf("personal dir: %v", err)
	}
}

func TestChooseWarnsAndRefuses(t *testing.T) {
	f := setup(t)
	f.addConfig("julienning1", email1)
	f.save()
	tired := acct(email1, 100, 50)
	tired.Exhausted, tired.ExhaustedUntil = true, ts(fiveReset)
	tired.BusyBy = []string{"ali"}
	f.fake.Listing = listing(tired)
	c, err := Choose("", location())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 2 || !strings.Contains(c.Warnings[0], "exhausted; claude1@x.io becomes usable at 20:00") || c.Warnings[1] != "claude1@x.io is also in use by ali" {
		t.Errorf("warnings: %q", c.Warnings)
	}
	f.fake.Listing = listing(acct(email2, 1, 1))
	if _, err := Choose("", location()); !errors.Is(err, ErrNoneLoggedIn) {
		t.Errorf("none logged in: %v", err)
	}
	f.fake.ListErr = errors.New("connection refused")
	if _, err := Choose("", time.UTC); err == nil || !strings.Contains(err.Error(), "cannot rank accounts") {
		t.Errorf("Worker down: %v", err)
	}
}

func TestTransferSessionsListsSharedAccountsOnly(t *testing.T) {
	f := setup(t)
	a := f.addConfig("julienning1", email1)
	m := f.addConfig("mine", personal)
	f.save()
	f.fake.Listing = listing(acct(email1, 1, 1))
	f.fake.Listing.Accounts[0].Nickname = "alpha"
	if _, err := sharedcache.SaveEntries([]sharedcache.Entry{{Email: email1, Nickname: "alpha"}}, nowT); err != nil {
		t.Fatal(err)
	}
	f.writeSession(a, sidA, "Team task", "fix", time.Hour)
	f.writeSession(m, sidB, "Private task", "mine", time.Hour)
	list, labels, _, err := TransferSessions(f.proj, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != sidA || labels["julienning1"] != "alpha" {
		t.Fatalf("got %+v %v", list, labels)
	}
	if got := ScopeLine(1, 10, false, "~/Code/shop", "on box"); got != "1 session in ~/Code/shop on box" {
		t.Errorf("scope: %s", got)
	}
}

// An unreadable allowlist with the Worker down lists nothing (a personal
// session must never be offered), and does not crash.
func TestTransferSessionsWithAnUnreadableAllowlist(t *testing.T) {
	f := setup(t)
	a := f.addConfig("julienning1", email1)
	f.save()
	f.writeSession(a, sidA, "Team task", "fix", time.Hour)
	f.fake.ListErr = errors.New("connection refused")
	if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, _, warnings, err := TransferSessions(f.proj, false, 10)
	if err != nil || len(list) != 0 || len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], "no session is offered") {
		t.Fatalf("%+v %q %v", list, warnings, err)
	}
}
