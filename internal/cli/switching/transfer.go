package switching

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// Choice is the account a transfer moves a session into (SPEC "transfer"
// step 3), on the machine that chose it.
type Choice struct {
	Config   config.ConfigDir `json:"config"`
	Email    string           `json:"email"`
	Nickname string           `json:"nickname"`
	Account  *remote.Account  `json:"account"`
	// Warnings are the exhausted and busy warnings next would print; the
	// caller prints them (prefixed with the machine for a host).
	Warnings []string `json:"warnings,omitempty"`
}

// ErrNoneLoggedIn is no shared account logged in on this machine.
var ErrNoneLoggedIn = errors.New("none of the shared accounts is logged in")

// PersonalError is a TARGET that is not a team account: transfer never
// moves a session into a personal one.
type PersonalError struct{ What string }

func (e *PersonalError) Error() string {
	return "transfer moves sessions between team accounts only; " + e.What + " is personal"
}

// Choose picks the destination account on this machine and makes it the
// selection, as next does: target resolved like `use TARGET` (it must be a
// shared account logged in here), else the Worker's ranking. loc renders
// the warnings' times (the controller's zone). Nothing is printed: a host's
// agent returns the warnings to the controller.
func Choose(target string, loc *time.Location) (Choice, error) {
	quiet := cli.Env{Stdout: io.Discard, Stderr: io.Discard}
	cfg, err := config.Load()
	if err != nil {
		return Choice{}, err
	}
	if err := cfg.RequireRemote(); err != nil {
		return Choice{}, err
	}
	now, err := clock()
	if err != nil {
		return Choice{}, err
	}
	client := newClient(cfg, listTimeout)
	cur, curOK, err := cfg.Current()
	if err != nil {
		curOK = false
	}
	var c Choice
	if strings.TrimSpace(target) == "" {
		reconcile(quiet, cfg, client, now)
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		listing, err := client.ListAccounts(ctx, cfg.Dev)
		if err != nil {
			return c, fmt.Errorf("cannot rank accounts (%v); name the account to move into", err)
		}
		cache := refreshCache(quiet, listing, now)
		acct, cd, ok := chooseNext(listing, localByEmail(quiet, cfg), cur, curOK)
		if !ok {
			return c, ErrNoneLoggedIn
		}
		email := strings.ToLower(acct.Email)
		c = Choice{Config: cd, Email: email, Nickname: cache.Nickname(email), Account: acct}
		if acct.IsExhausted() {
			c.Warnings = append(c.Warnings, "every usable shared account here is exhausted; "+usableAgain(orEmail(c.Nickname, email), acct.ExhaustedUntil, now, loc))
		}
		if len(acct.BusyBy) > 0 {
			c.Warnings = append(c.Warnings, fmt.Sprintf("%s is also in use by %s", accountName(c.Nickname, email), strings.Join(acct.BusyBy, ", ")))
		}
	} else {
		listing := fetchListing(quiet, cfg, now)
		var cache *sharedcache.Cache
		if listing != nil {
			cache = refreshCache(quiet, listing, now)
		} else {
			cache = loadCache(quiet)
		}
		if strings.Contains(target, "@") {
			if email := strings.ToLower(strings.TrimSpace(target)); resolve.IsPersonal(cfg, cache, email) {
				return c, &PersonalError{What: email}
			}
		}
		var curPtr *config.ConfigDir
		if curOK {
			curPtr = &cur
		}
		m, err := resolve.Target(cfg, cache, target, curPtr)
		if err != nil {
			return c, err
		}
		email, err := claudecfg.ReadEmail(m.Dir.Dir)
		switch {
		case errors.Is(err, claudecfg.ErrNotLoggedIn):
			return c, fmt.Errorf("%s is not logged in; sign in first: julienning login %s", m.Dir.Name, m.Dir.Name)
		case err != nil:
			return c, fmt.Errorf("cannot read the login of %s: %w", m.Dir.Name, err)
		}
		if cache == nil || !cache.Contains(email) {
			return c, &PersonalError{What: email}
		}
		c = Choice{Config: m.Dir, Email: email, Nickname: cache.Nickname(email), Account: findAccount(listing, email)}
	}
	if err := config.SetCurrent(c.Config); err != nil {
		return c, err
	}
	return c, nil
}

// MovingLine heads the transfer's session picker:
//
//	Moving into alpha (claude1@x.io) in ~/.claude-x on box — session 12% → 17:30, week 28% → Tue 21:00.
//
// dir is the account dir as the destination shortens it; machine is "on
// box" or "on this machine".
func MovingLine(c Choice, dir, machine string, now time.Time, loc *time.Location) string {
	name := c.Config.Name
	if c.Nickname != "" {
		name = c.Nickname
	}
	s := "Moving into " + name + " (" + c.Email + ") in " + dir + " " + machine
	if c.Account != nil {
		s += " — " + usageSummary(c.Account, now, loc)
	}
	return s + "."
}

// TransferSessions lists what a transfer may move from this machine: the
// sessions of every registered dir logged into a shared account right now
// (personal sessions never appear), in cwd's project folder or, with all,
// every project. labels maps config names to how picker rows name them.
func TransferSessions(cwd string, all bool, limit int) (list []sessions.Session, labels map[string]string, warnings []string, err error) {
	quiet := cli.Env{Stdout: io.Discard, Stderr: io.Discard}
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	now, err := clock()
	if err != nil {
		return nil, nil, nil, err
	}
	var cache *sharedcache.Cache
	if cfg.RequireRemote() == nil {
		if l := fetchListing(quiet, cfg, now); l != nil {
			cache = refreshCache(quiet, l, now)
		}
	}
	if cache == nil {
		cache = loadCache(quiet)
		warnings = append(warnings, "using the cached shared-account list")
	}
	if cache == nil {
		// Unreadable: nothing is known to be shared, so nothing is listed
		// (a personal session must never be offered).
		return nil, nil, append(warnings, "the shared-account list cannot be read, so no session is offered; run julienning accounts to refresh it"), nil
	}
	logins := loginsByDir(cfg)
	var dirs []config.ConfigDir
	for _, cd := range cfg.Configs {
		if email := logins[cd.Dir]; email != "" && cache.Contains(email) {
			dirs = append(dirs, cd)
		}
	}
	if len(dirs) == 0 {
		return nil, nil, warnings, nil
	}
	list, err = sessions.List(sessions.ListOptions{Dirs: dirs, Cwd: cwd, All: all, Limit: limit, Now: now})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("some sessions could not be read: %v", err))
	}
	return list, dirLabels(cfg, cache, logins), warnings, nil
}

// ScopeLine is scopeLine for transfer: what the picker lists, on which
// machine ("37 sessions in ~/Code/shop on this machine").
func ScopeLine(n, limit int, all bool, cwd, machine string) string {
	noun := "sessions"
	if n == 1 {
		noun = "session"
	}
	s := fmt.Sprintf("%d %s", n, noun)
	if n >= limit {
		s = fmt.Sprintf("%d most recent %s", n, noun)
	}
	if !all && cwd != "" {
		s += " in " + cwd
	}
	return s + " " + machine
}

// Location is the display zone (TZ at call time, else the system's).
func Location() *time.Location { return location() }

// Clock honours JULIENNING_NOW_EPOCH.
func Clock() (time.Time, error) { return clock() }
