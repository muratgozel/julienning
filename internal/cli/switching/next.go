package switching

import (
	"context"
	"fmt"
	"strings"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "next",
		Summary: "switch to the best shared account on this machine, then start or resume a session",
		Usage:   "next [--all] [--limit N] [--no-launch] [--json]",
		Run:     runNext,
	})
}

func runNext(env cli.Env) error {
	o, pos, _, err := parseFlags("next", env, false)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return cli.Usagef("next takes no arguments")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireRemote(); err != nil {
		return err
	}
	now, err := clock()
	if err != nil {
		return err
	}
	client := newClient(cfg, listTimeout)
	reconcile(env, cfg, client, now)

	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	listing, err := client.ListAccounts(ctx, cfg.Dev)
	if err != nil {
		return fmt.Errorf("cannot rank accounts (%v); pick manually with `julienning use NICKNAME`", err)
	}
	cache := refreshCache(env, listing, now)
	if len(listing.Accounts) == 0 {
		return fmt.Errorf("the team allowlist is empty; share an account with `julienning share EMAIL`")
	}

	cur, curOK, err := cfg.Current()
	if err != nil {
		return err
	}
	acct, cd, ok := chooseNext(listing, localByEmail(env, cfg), cur, curOK)
	if !ok {
		best := listing.Accounts[0]
		return fmt.Errorf("none of the %d shared accounts is logged in on this machine (best ranked: %s); "+
			"sign one in with `julienning new-config --login` or `julienning login NICKNAME`",
			len(listing.Accounts), accountName(cache.Nickname(best.Email), strings.ToLower(best.Email)))
	}
	if err := config.SetCurrent(cd); err != nil {
		return err
	}
	email := strings.ToLower(acct.Email)
	if len(acct.BusyBy) > 0 {
		warnf(env, "%s is also in use by %s", accountName(cache.Nickname(email), email), strings.Join(acct.BusyBy, ", "))
	}
	sel := selection{
		cd:       cd,
		email:    email,
		nickname: cache.Nickname(email),
		account:  acct,
		shared:   true,
		stay:     curOK && cur.Dir == cd.Dir,
	}
	return finish(env, cfg, cache, sel, o, now)
}

// chooseNext walks the Worker's ranking and returns the first account with a
// local dir. Among several dirs logged into that account, the current dir
// wins, else the first by name. When the current dir's account ranks level
// with the winner on every ranking key (the Worker then orders by email
// only), staying put avoids a pointless switch.
func chooseNext(l *remote.Listing, local map[string][]config.ConfigDir, cur config.ConfigDir, curOK bool) (*remote.Account, config.ConfigDir, bool) {
	var best *remote.Account
	for i := range l.Accounts {
		if len(local[strings.ToLower(l.Accounts[i].Email)]) > 0 {
			best = &l.Accounts[i]
			break
		}
	}
	if best == nil {
		return nil, config.ConfigDir{}, false
	}
	if curOK {
		for i := range l.Accounts {
			a := &l.Accounts[i]
			if a != best && holds(local[strings.ToLower(a.Email)], cur) && sameRank(a, best) {
				return a, cur, true
			}
		}
	}
	dirs := local[strings.ToLower(best.Email)]
	if curOK && holds(dirs, cur) {
		return best, cur, true
	}
	return best, dirs[0], true
}

func holds(dirs []config.ConfigDir, cd config.ConfigDir) bool {
	for _, d := range dirs {
		if d.Dir == cd.Dir {
			return true
		}
	}
	return false
}

// sameRank compares the Worker's sort keys except the final email
// tie-breaker (SPEC "Ranking and state").
func sameRank(a, b *remote.Account) bool {
	known := func(x *remote.Account) bool { return x.Session != nil || x.Week != nil }
	reset := func(w *remote.Window) int64 {
		if w == nil || w.ResetPassed || w.ResetsAt == nil {
			return 0
		}
		return w.ResetsAt.Unix()
	}
	return (len(a.BusyBy) > 0) == (len(b.BusyBy) > 0) &&
		known(a) == known(b) &&
		a.Session.Percent() == b.Session.Percent() &&
		a.Week.Percent() == b.Week.Percent() &&
		reset(a.Session) == reset(b.Session)
}
