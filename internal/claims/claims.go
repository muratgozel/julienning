// Package claims keeps this machine's claims in sync with its live Claude
// sessions. A claim exists in the Worker for (email, dev, machine) while at
// least one Claude session logged into that email runs on this machine.
package claims

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// File is the local record of emails this machine holds a claim on.
const File = "claims.json"

// Held maps lowercased email → when this machine last claimed it
// successfully (the last PutClaim), in UTC.
type Held map[string]time.Time

type fileFormat struct {
	Held Held `json:"held"`
}

// LoadHeld reads claims.json; missing means nothing held.
func LoadHeld() (Held, error) {
	p, err := config.Path(File)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(raw) == 0) {
		return Held{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if f.Held == nil {
		f.Held = Held{}
	}
	return f.Held, nil
}

// SaveHeld writes claims.json atomically.
func SaveHeld(h Held) error {
	p, err := config.Path(File)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	raw, err := json.MarshalIndent(fileFormat{Held: h}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode claims: %w", err)
	}
	return config.WriteFileAtomic(p, append(raw, '\n'), 0o600)
}

// Identity is this machine's claim holder identity.
func Identity(cfg *config.Config) remote.Identity {
	return remote.Identity{Dev: cfg.Dev, MachineID: cfg.MachineID}
}

// ReleaseAll deletes every claim this machine holds (uninstall). It keeps
// going on errors and returns the first one.
func ReleaseAll(ctx context.Context, cfg *config.Config, c remote.Client) error {
	h, err := LoadHeld()
	if err != nil {
		return err
	}
	emails := make([]string, 0, len(h))
	for e := range h {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	var first error
	for _, e := range emails {
		if err := c.DeleteClaim(ctx, e, Identity(cfg)); err != nil && !remote.IsNotShared(err) {
			if first == nil {
				first = err
			}
			continue
		}
		delete(h, e)
	}
	if err := SaveHeld(h); err != nil && first == nil {
		first = err
	}
	return first
}

// ClaimRefresh is how long a recorded claim is trusted before Reconcile
// PUTs it again. Every PUT is a KV write on the Worker's small free-tier
// budget, and usage reports already refresh the holder's `at`, so an email
// held and claimed within this window is not re-sent. Re-PUTting after it
// heals a claim the Worker lost (unshare + re-share, TTL expiry).
const ClaimRefresh = time.Hour

// account is one email logged into this machine's registered dirs.
type account struct {
	names []string // registered config names logged into it, sorted
	live  int      // live sessions across those dirs, minus the ending one
	// unknown means at least one of its dirs could not be counted (no
	// registry, unreadable registry), so "no live sessions" is not proven.
	unknown bool
}

// Ending identifies the session that is closing, if any. At SessionEnd time
// its process and registry entry still exist, so it must be excluded
// explicitly. SessionID is preferred; PID (the registry entry's pid) is the
// fallback when the hook could not learn the id. Zero values exclude nothing.
type Ending struct {
	SessionID string
	PID       int
}

func (e Ending) excludes(s livesess.Session) bool {
	return (e.SessionID != "" && s.SessionID == e.SessionID) || (e.PID > 0 && s.PID == e.PID)
}

// Reconcile implements SPEC "Claims from sessions" steps 1-2: for every
// shared email logged into a registered dir, claim it while this machine has
// a live session on it and release it once none is left.
//
// It keeps going past failures and returns them joined. Error messages name
// config dirs, never emails: callers write them to errors.log.
//
// Callers must hold the claim-sync lock (AcquireLock), or use Sync: two
// concurrent runs would each rewrite claims.json and one could lose a record,
// leaving a claim in the Worker that this machine no longer knows to release.
func Reconcile(ctx context.Context, cfg *config.Config, c remote.Client, ending Ending, now time.Time) error {
	var errs []error
	cache, err := sharedcache.Load()
	if err != nil {
		// Membership is unknown: claiming could send a personal email to the
		// Worker and releasing could drop a live claim, so change nothing.
		// Refreshing the allowlist overwrites the broken file.
		return fmt.Errorf("claims: %w", err)
	}
	held, err := LoadHeld()
	if err != nil {
		// Self-healing: live accounts are re-claimed and recorded below; only
		// a stale claim on an idle account survives until the Worker's TTL.
		errs = append(errs, fmt.Errorf("claims: %w (starting over)", err))
		held = Held{}
	}

	accounts, readFailed := localAccounts(cfg, &errs)
	for email, a := range accounts {
		if cache.Contains(email) {
			countLive(a, cfg, email, ending, &errs)
		}
	}

	changed := false
	id := Identity(cfg)
	release := func(email, label string) {
		err := c.DeleteClaim(ctx, email, id)
		if err != nil && !remote.IsNotShared(err) {
			errs = append(errs, fmt.Errorf("release claim %s: %w", label, err))
			return
		}
		delete(held, email)
		changed = true
	}

	for _, email := range union(accounts, held) {
		a, logged := accounts[email]
		_, isHeld := held[email]
		switch {
		case !cache.Contains(email):
			// Never claim an unshared email (it must not reach the Worker);
			// an existing claim was made while it was shared, so releasing it
			// discloses nothing new.
			if isHeld {
				release(email, "on an account that is no longer shared")
			}
		case !logged:
			// A dir whose account file could not be read might still be
			// logged into this email; keep the claim until that is known.
			if isHeld && !readFailed {
				release(email, "on an account no longer logged in here")
			}
		case a.live > 0:
			if isHeld && fresh(held[email], now) {
				continue
			}
			err := c.PutClaim(ctx, email, id)
			switch {
			case err == nil:
				held[email] = now.UTC()
				changed = true
			case remote.IsNotShared(err):
				// The allowlist moved under us: drop the record and the stale
				// cache entry so hooks and the status line stop acting on it.
				if isHeld {
					delete(held, email)
					changed = true
				}
				if _, ferr := sharedcache.Remove(email); ferr != nil {
					errs = append(errs, fmt.Errorf("claims: %w", ferr))
				}
			default:
				errs = append(errs, fmt.Errorf("claim %s: %w", label(a), err))
			}
		case a.unknown || readFailed:
			// Zero counted sessions is not proof of zero sessions: a dir
			// without a registry (Claude versions that do not write one, or a
			// dir Claude never ran in) or an unreadable dir may hide one.
			// Keep whatever is held; the next reconcile decides.
		case isHeld:
			release(email, label(a))
		}
	}

	if changed {
		if err := SaveHeld(held); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Sync is ResolvePendingShares plus Reconcile for interactive commands
// (accounts, next, use): it takes the claim-sync lock without waiting and
// stamps MarkReconciled. ran=false with a nil error means another process
// holds the lock and is reconciling right now, which is not a failure.
func Sync(ctx context.Context, cfg *config.Config, c remote.Client, now time.Time) (ran bool, err error) {
	release, ok, err := AcquireLock(now)
	if err != nil || !ok {
		return false, err
	}
	defer release()
	err = errors.Join(ResolvePendingShares(ctx, cfg, c, now), Reconcile(ctx, cfg, c, Ending{}, now))
	if merr := MarkReconciled(now); merr != nil {
		err = errors.Join(err, merr)
	}
	return true, err
}

// localAccounts groups the registered dirs by the email logged into them.
// readFailed reports a dir whose account file exists but could not be read.
func localAccounts(cfg *config.Config, errs *[]error) (map[string]*account, bool) {
	out := map[string]*account{}
	readFailed := false
	for _, cd := range cfg.Configs {
		email, err := claudecfg.ReadEmail(cd.Dir)
		if errors.Is(err, claudecfg.ErrNotLoggedIn) {
			continue
		}
		if err != nil {
			readFailed = true
			*errs = append(*errs, fmt.Errorf("config %s: %w", cd.Name, err))
			continue
		}
		a := out[email]
		if a == nil {
			a = &account{}
			out[email] = a
		}
		a.names = append(a.names, cd.Name)
	}
	for _, a := range out {
		sort.Strings(a.names)
	}
	return out, readFailed
}

// countLive fills a.live and a.unknown from the live-session registry of every
// registered dir logged into email.
func countLive(a *account, cfg *config.Config, email string, ending Ending, errs *[]error) {
	for _, cd := range cfg.Configs {
		if !contains(a.names, cd.Name) {
			continue
		}
		// CRITICAL: the registry is an undocumented Claude Code internal. A
		// dir without one says nothing about its sessions, so it can keep a
		// claim alive but never prove it idle.
		if !livesess.RegistryExists(cd.Dir) {
			a.unknown = true
			continue
		}
		sessions, err := livesess.List(cd.Dir)
		if err != nil {
			a.unknown = true
			*errs = append(*errs, fmt.Errorf("config %s: %w", cd.Name, err))
			continue
		}
		for _, s := range sessions {
			// Daemons, daemon workers and pre-spawned spares are registered
			// too but serve nobody; counting them would hold claims forever.
			if livesess.CountsAsSession(s) && !ending.excludes(s) {
				a.live++
			}
		}
	}
}

// fresh reports whether a claim recorded at t needs no refresh at now. A
// record from the future (clock moved back) is treated as stale.
func fresh(t, now time.Time) bool {
	d := now.Sub(t)
	return d >= 0 && d < ClaimRefresh
}

func label(a *account) string { return strings.Join(a.names, ",") }

func union(accounts map[string]*account, held Held) []string {
	out := make([]string, 0, len(accounts)+len(held))
	for e := range accounts {
		out = append(out, e)
	}
	for e := range held {
		if _, dup := accounts[e]; !dup {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
