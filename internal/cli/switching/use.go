package switching

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/pins"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "use",
		Summary: "start or resume a session on an account (TARGET: nickname, email or config name; none: this directory's pin, else the selection, else like next; --clear: back to your default)",
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
	// Without a target, a pin on the working directory (or a parent) names
	// the account, exactly as if its email had been typed.
	var pin *pins.Pin
	if len(pos) == 0 {
		p, ok, err := pinHere()
		if err != nil {
			return err
		}
		if ok {
			pin, pos = &p, []string{p.Email}
		}
	}
	cur, curOK, err := cfg.Current()
	if len(pos) == 0 {
		// No target: stay on the current selection and go straight to its
		// session picker; with nothing selected, rank like `next`.
		if err != nil {
			return err
		}
		if !curOK {
			return runNext(env) // warns about setup itself
		}
	} else if err != nil {
		// The selection only breaks ties between dirs of one account.
		warnf(env, "ignoring the current selection (%v)", err)
		curOK = false
	}
	warnSetup(env, cfg)
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

	// An email that is not a team account is personal: it is opened where
	// it is logged in, registered or not, and never becomes the selection.
	if len(pos) == 1 && strings.Contains(pos[0], "@") {
		email := strings.ToLower(strings.TrimSpace(pos[0]))
		if resolve.IsPersonal(cfg, cache, email) {
			if !claudecfg.ValidEmail(email) {
				return fmt.Errorf("%q is not a valid email address", pos[0])
			}
			return usePersonal(env, cfg, cache, email, pin, o, now)
		}
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
			if pin != nil {
				return pins.UseError(*pin, err)
			}
			return err
		}
		cd = m.Dir
	}

	sel := selection{cd: cd}
	if pin != nil {
		sel.pinDir = pin.Dir
	}
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

// usePersonal opens a personal account: the dir it is logged in (remembered
// in config.json, searched again when the login moved), its own sessions
// only, and never a move, a claim, a report or a change of the selection.
func usePersonal(env cli.Env, cfg *config.Config, cache *sharedcache.Cache, email string, pin *pins.Pin, o options, now time.Time) error {
	pm, err := resolve.Personal(cfg, email)
	if err != nil {
		if pin != nil {
			return pins.UseError(*pin, err)
		}
		return err
	}
	for _, n := range pm.Notes() {
		notef(env, "%s", n)
	}
	if err := pm.Remember(); err != nil {
		// Only a shortcut for next time: the account was found anyway.
		warnf(env, "%v", err)
	}
	sel := selection{cd: config.ConfigDir{Name: pm.Name, Dir: pm.Dir}, email: email, personal: true}
	if pin != nil {
		sel.pinDir = pin.Dir
	}
	return finish(env, cfg, cache, sel, o, now)
}

// pinHere returns the pin that applies in the working directory.
func pinHere() (pins.Pin, bool, error) {
	list, err := pins.Load()
	if err != nil || len(list) == 0 {
		return pins.Pin{}, false, err
	}
	cwd, err := getwd()
	if err != nil {
		return pins.Pin{}, false, fmt.Errorf("cannot determine the current directory to look up its pin: %w", err)
	}
	p, ok := pins.Match(list, cwd)
	return p, ok, nil
}
