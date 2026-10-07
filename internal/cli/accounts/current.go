package accounts

import (
	"context"
	"errors"
	"fmt"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "current",
		Summary: "show the selected account (nickname, email, config dir) and its usage",
		Usage:   "current [--json]",
		Run:     runCurrent,
	})
}

const errNotShared = "account is not shared"

func runCurrent(env cli.Env) error {
	fs := cli.NewFlagSet("current", env)
	asJSON := fs.Bool("json", false, "print one JSON document")
	if err := fs.Parse(env.Args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return cli.Usagef("current takes no arguments")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cd, ok, err := cfg.Current()
	if err != nil {
		return err
	}
	if !ok {
		if *asJSON {
			err = printJSON(env, map[string]any{"config": nil, "email": nil, "nickname": nil, "account": nil, "usage_error": nil})
		} else {
			_, err = fmt.Fprintln(env.Stdout, "none")
		}
		if err == nil {
			updateHint(env.Stderr)
		}
		return err
	}

	// ReadEmail applies the default-dir rule: the default dir's login lives
	// in ~/.claude.json, not ~/.claude/.claude.json.
	email, usageErr := "", ""
	switch e, err := claudecfg.ReadEmail(cd.Dir); {
	case errors.Is(err, claudecfg.ErrNotLoggedIn):
		usageErr = "config not logged in"
	case err != nil:
		usageErr = err.Error()
	default:
		email = e
	}

	// Usage is best effort: `current` must work offline.
	var account *remote.Account
	var cache *sharedcache.Cache
	if email != "" {
		cache, account, usageErr = currentUsage(cfg, email)
	}
	nick := ""
	if usageErr != errNotShared {
		// A stale shared.json may still name an account the Worker just
		// called unshared; do not present it as a team account.
		nick = currentNickname(cache, account, email)
	}

	if *asJSON {
		doc := map[string]any{"config": cd.Name, "email": nil, "nickname": nil, "account": nil, "usage_error": nil}
		if email != "" {
			doc["email"] = email
		}
		if nick != "" {
			doc["nickname"] = nick
		}
		if account != nil {
			obj, err := account.Object()
			if err != nil {
				return err
			}
			doc["account"] = obj
		}
		if usageErr != "" {
			doc["usage_error"] = usageErr
		}
		if err := printJSON(env, doc); err != nil {
			return err
		}
		updateHint(env.Stderr)
		return nil
	}

	// Users know accounts by nickname; the config name is only the fallback
	// for a dir whose login has none (personal, legacy, or not logged in).
	label := cd.Name
	if nick != "" {
		label = nick
	}
	if email != "" {
		label += " (" + email + ")"
	}
	fmt.Fprintf(env.Stdout, "%s in %s\n", label, shortenHome(cd.Dir))
	if account == nil {
		fmt.Fprintf(env.Stdout, "usage: unavailable (%s)\n", usageErr)
	} else {
		n, err := now()
		if err != nil {
			return err
		}
		loc := location()
		fmt.Fprintf(env.Stdout, "usage: %s\n", summary(account, n, loc))
		// Only an exhausted account gets a state line: it is the one state
		// that changes what the user can do with the selection.
		if account.IsExhausted() {
			fmt.Fprintf(env.Stdout, "state: %s\n", state(account, cfg.Dev, n, loc))
		}
	}
	updateHint(env.Stderr)
	return nil
}

// currentNickname prefers the Worker's live answer (a rename may be newer
// than shared.json), then the cached allowlist. Only shared accounts have a
// nickname; cache and account may be nil.
func currentNickname(cache *sharedcache.Cache, account *remote.Account, email string) string {
	if account != nil && account.Nickname != "" {
		return account.Nickname
	}
	if cache != nil && email != "" {
		return cache.Nickname(email)
	}
	return ""
}

// currentUsage asks the Worker about email, or says why it did not. Only
// emails on the cached allowlist are sent: a registered dir may be logged
// into a personal account, and that address must not leave the machine. The
// cache is returned (nil when unreadable) so the caller can name the account.
func currentUsage(cfg *config.Config, email string) (*sharedcache.Cache, *remote.Account, string) {
	cache, err := sharedcache.Load()
	switch {
	case err != nil:
		return nil, nil, "cannot tell whether the account is shared (" + err.Error() + ")"
	case !cache.Contains(email):
		return cache, nil, errNotShared
	}
	if err := cfg.RequireRemote(); err != nil {
		return cache, nil, err.Error()
	}
	// dev goes along so the Worker does not report our own activity as "in
	// use by someone else".
	a, err := newClient(cfg, interactiveTimeout).GetAccount(context.Background(), email, cfg.Dev)
	switch {
	case remote.IsNotShared(err):
		return cache, nil, errNotShared
	case err != nil:
		return cache, nil, err.Error()
	}
	return cache, a, ""
}
