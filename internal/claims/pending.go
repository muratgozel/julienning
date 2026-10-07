package claims

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// ResolvePendingShares implements SPEC "Claims from sessions" step 0: every
// registered dir new-config marked ShareOnLogin has the account it is now
// signed into shared with the team, under the mark's nickname or the email's
// local part, and the mark is cleared. Callers run it right before
// Reconcile, so a freshly shared account is claimed in the same run.
//
// Per marked dir: not logged in → left for a later run; already on the
// allowlist (shared.json, else one fresh listing per run) → mark cleared;
// declined as personal on this machine → mark cleared, not shared; otherwise
// Share. A taken nickname or any other failure keeps the mark, so the next
// run (or `julienning setup`, which offers the dir with that nickname)
// retries. Marks are cleared through config.UpdateDir, never by saving cfg,
// so a long-running caller cannot undo config changes made meanwhile; cfg is
// updated in memory too.
//
// It keeps going past failures and returns them joined. Messages name
// config dirs and nicknames, never emails: callers write them to errors.log.
// Callers must hold the claim-sync lock (Sync and claim-sync do).
func ResolvePendingShares(ctx context.Context, cfg *config.Config, c remote.Client, now time.Time) error {
	var errs []error
	cache, err := sharedcache.Load()
	if err != nil {
		// sharedcache's rule: unreadable means nothing is known to be
		// shared. The listing below decides instead, and replaces the file.
		cache = &sharedcache.Cache{}
	}
	listed := false
	for i := range cfg.Configs {
		cd := cfg.Configs[i]
		if cd.ShareOnLogin == nil {
			continue
		}
		email, err := claudecfg.ReadEmail(cd.Dir)
		if errors.Is(err, claudecfg.ErrNotLoggedIn) {
			continue // signed in later; hooks and commands run this again
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("config %s: %w", cd.Name, err))
			continue
		}
		settle := func() {
			if err := clearShareOnLogin(cfg, cd.Dir); err != nil {
				errs = append(errs, fmt.Errorf("config %s: could not clear its pending share in config.json: %w", cd.Name, err))
			}
		}
		if !cache.Contains(email) && !listed {
			// One listing per run answers "already shared?" for every marked
			// dir, and doubles as the allowlist refresh: a fresh fetched_at
			// keeps claim-sync's own refresh from replacing the cache with a
			// listing that may not show this share yet (KV listings lag).
			listed = true
			if l, lerr := c.ListAccounts(ctx, cfg.Dev); lerr == nil {
				if fresh, serr := sharedcache.FromListing(l, now); serr == nil {
					cache = fresh
				} else {
					errs = append(errs, fmt.Errorf("pending shares: %w", serr))
				}
			}
			// A failed listing is not reported: Share is idempotent and its
			// own outcome (reported below) is what decides.
		}
		if cache.Contains(email) {
			settle()
			continue
		}
		if cfg.Declined(email) {
			// This machine's user said the account is personal; signing it
			// into a team dir by mistake must not publish it.
			settle()
			errs = append(errs, fmt.Errorf("config %s: its account was not shared because it is marked personal on this machine (share it with: julienning share EMAIL)", cd.Name))
			continue
		}
		nick := cd.ShareOnLogin.NicknameFor(email)
		if nick == "" {
			errs = append(errs, fmt.Errorf("config %s: no nickname can be derived from its account's email; share it with: julienning share EMAIL --nick NAME", cd.Name))
			continue
		}
		err = c.Share(ctx, email, nick, Identity(cfg))
		switch {
		case remote.IsConflict(err):
			errs = append(errs, fmt.Errorf("config %s: nickname taken: %q belongs to another team account; pick another with: julienning setup", cd.Name, nick))
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("config %s: share its account as %q: %w", cd.Name, nick, err))
			continue
		}
		if fresh, err := sharedcache.AddWithNickname(email, nick); err == nil {
			cache = fresh
		} else {
			// The share stands; the cache catches up at its next refresh.
			errs = append(errs, fmt.Errorf("config %s: shared as %q, but the allowlist cache could not be updated: %w", cd.Name, nick, err))
			cache.Emails = append(cache.Emails, strings.ToLower(email))
			sort.Strings(cache.Emails)
		}
		settle()
	}
	return errors.Join(errs...)
}

// clearShareOnLogin drops the mark of dir from config.json and from cfg.
func clearShareOnLogin(cfg *config.Config, dir string) error {
	for i := range cfg.Configs {
		if cfg.Configs[i].Dir == dir {
			cfg.Configs[i].ShareOnLogin = nil
		}
	}
	_, err := config.UpdateDir(dir, func(cd *config.ConfigDir) { cd.ShareOnLogin = nil })
	return err
}
