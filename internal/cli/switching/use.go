package switching

import (
	"errors"
	"fmt"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "use",
		Summary: "start or resume a session on an account (TARGET: team nickname, email or config name; none: the current selection, or like next when nothing is selected; --clear: back to your default)",
		Usage:   "use [TARGET] [--all] [--limit N] [--no-launch] [--json] | use --clear",
		Run:     runUse,
	})
}

func runUse(env cli.Env) error {
	o, pos, set, err := parseFlags("use", env, true)
	if err != nil {
		return err
	}
	if o.clear {
		if len(pos) != 0 || len(set) != 1 {
			return cli.Usagef("use --clear takes no other arguments")
		}
		if err := config.ClearCurrent(); err != nil {
			return err
		}
		fmt.Fprintln(env.Stdout, "Selection cleared; plain claude uses your default config.")
		selfupdate.Hint(env.Stderr)
		return nil
	}
	if len(pos) > 1 {
		return cli.Usagef("use takes at most one target: a team nickname, an email or a config name (see `julienning accounts`)")
	}
	if len(pos) == 1 && strings.TrimSpace(pos[0]) == "" {
		return cli.Usagef("use needs a non-empty target: a team nickname, an email or a config name")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cur, curOK, err := cfg.Current()
	if len(pos) == 0 {
		// No target: stay on the current selection and go straight to its
		// session picker; with nothing selected, rank like `next`.
		if err != nil {
			return err
		}
		if !curOK {
			return runNext(env)
		}
	} else if err != nil {
		// The selection only breaks ties between dirs of one account.
		warnf(env, "ignoring the current selection (%v)", err)
		curOK = false
	}
	now, err := clock()
	if err != nil {
		return err
	}

	// The Worker is optional for `use`: it refreshes the allowlist (and so
	// the nicknames) and shows usage, but an offline switch must still work.
	// It is asked before resolving the target: a stale cache could let a
	// config name shadow a nickname a teammate just set, and nickname must
	// win (SPEC "Nicknames").
	remoteErr := cfg.RequireRemote()
	var listing *remote.Listing
	if remoteErr == nil {
		listing = fetchListing(env, cfg, now)
	}
	var cache *sharedcache.Cache
	if listing != nil {
		cache = refreshCache(env, listing, now)
	} else {
		cache = loadCache(env)
	}

	cd := cur
	if len(pos) == 1 {
		var curPtr *config.ConfigDir
		if curOK {
			curPtr = &cur
		}
		m, err := resolve.Target(cfg, cache, pos[0], curPtr)
		if err != nil {
			// *resolve.NotLocalError's message already says how to sign in.
			return err
		}
		cd = m.Dir
	}

	sel := selection{cd: cd}
	email, err := claudecfg.ReadEmail(cd.Dir)
	switch {
	case err == nil:
		sel.email = email
	case errors.Is(err, claudecfg.ErrNotLoggedIn):
		warnf(env, "%s is not logged in yet; run: julienning login %s", cd.Name, cd.Name)
	default:
		warnf(env, "cannot read the login of %s (%v); treating it as not logged in", cd.Name, err)
	}
	if remoteErr != nil && sel.email != "" {
		warnf(env, "%v; using the cached shared-account list", remoteErr)
	}

	if sel.email != "" {
		sel.account = findAccount(listing, sel.email)
		sel.shared = sel.account != nil
		if listing == nil {
			sel.shared = cache != nil && cache.Contains(sel.email)
		}
		if sel.shared && cache != nil {
			sel.nickname = cache.Nickname(sel.email)
		}
		if !sel.shared {
			warnf(env, "%s is logged into %s, which is not shared with the team; sessions will not be moved into it (share it with `julienning share %s`)",
				cd.Name, sel.email, sel.email)
		}
	}

	if err := config.SetCurrent(cd); err != nil {
		return err
	}
	return finish(env, cfg, cache, sel, o, now)
}
