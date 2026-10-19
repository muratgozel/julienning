package usageprobe

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

const (
	// InteractiveStale: `accounts`, `next` and `use` probe an account whose
	// numbers are older than this.
	InteractiveStale = 15 * time.Minute
	// BackgroundStale: the background round probes less eagerly; it only
	// keeps idle accounts from going stale for the whole team.
	BackgroundStale = time.Hour
	// CacheWindow: a login is not probed twice within this, whatever the
	// outcome. Its last result is re-sent instead, because the Worker's
	// listing can lag the write by up to a minute.
	CacheWindow = 5 * time.Minute
)

// Options is one refresh.
type Options struct {
	Config *config.Config
	Client remote.Client
	// Listing is the listing just fetched: the allowlist (only accounts on
	// it are probed: personal usage never leaves the machine), how fresh
	// each account's numbers are, and whether the Worker takes probes.
	Listing *remote.Listing
	Now     time.Time
	// StaleAfter is InteractiveStale or BackgroundStale.
	StaleAfter time.Duration
	// Only limits the refresh to one account (lowercased email; `use`).
	Only string
	// Background is the round run by detached processes: it records no
	// logins (a change must be told to someone at a terminal, see
	// Report.Changes) and asks the Worker for no listing (it ranks nothing,
	// and every listing costs a KV list operation).
	Background bool
}

// Problem is a probe that failed for one dir.
type Problem struct {
	Dir   config.ConfigDir
	Email string
	Err   error
	Shape string // see Result.Shape
}

// Report is what a refresh did. The caller prints or logs it.
type Report struct {
	// Listing is what to rank on: the Worker's answer with the probes merged
	// in, or Options.Listing when nothing was sent or sending failed.
	Listing *remote.Listing
	// Problems are probes that failed (or did not run in time).
	Problems []Problem
	// SignedOut are dirs a probe found signed out (just now, or within
	// CacheWindow).
	SignedOut []Problem
	// Changes are registered dirs whose login moved to another account.
	Changes []config.LoginChange
	// RecordErr: the dirs' logins could not be recorded in config.json.
	RecordErr error
	// OldWorker: the Worker predates POST /probes and needs a redeploy;
	// nothing was probed.
	OldWorker bool
	// PostErr: sending the probes failed otherwise.
	PostErr error
}

// target is one dir to probe.
type target struct {
	cd    config.ConfigDir
	email string
}

// Refresh probes the stale shared accounts logged in on this machine and
// sends what it read to the Worker in one batch. It never fails: problems
// come back in the Report, and the listing to rank on is always set.
func Refresh(ctx context.Context, o Options) Report {
	rep := Report{Listing: o.Listing}
	if o.Listing == nil {
		return rep
	}
	logins := Logins(o.Config)
	if !o.Background {
		rep.Changes, rep.RecordErr = config.RecordLogins(logins)
	}
	if !o.Listing.Probes {
		// A Worker that predates probes could store nothing: no claude runs.
		rep.OldWorker = true
		return rep
	}
	listed := make(map[string]*remote.Account, len(o.Listing.Accounts))
	for i := range o.Listing.Accounts {
		a := &o.Listing.Accounts[i]
		listed[strings.ToLower(a.Email)] = a
	}

	var probe []target
	var cached []State
	covered := map[string]bool{} // accounts already probed or cached in this refresh
	for _, cd := range o.Config.Configs {
		email := logins[filepath.Clean(cd.Dir)]
		acct, shared := listed[email]
		if email == "" || !shared || (o.Only != "" && email != o.Only) {
			continue
		}
		st := LoadState(cd.Dir)
		if st != nil && st.Email == email && within(o.Now, st.ProbedAt, CacheWindow) {
			switch st.Outcome {
			case OK:
				cached = append(cached, *st)
				covered[email] = true
			case SignedOut:
				// Said again on every run: next skips the dir, so it must
				// not go quiet just because the mark is recent.
				rep.SignedOut = append(rep.SignedOut, Problem{Dir: cd, Email: email})
			default:
				covered[email] = true
			}
			continue
		}
		// One probe per account: another dir logged into it says the
		// same. A dir marked signed out is asked again all the same, to
		// see whether someone signed in there.
		signedOut := st != nil && st.Outcome == SignedOut && st.Email == email
		if signedOut || (!covered[email] && stale(acct, o.Now, o.StaleAfter)) {
			probe = append(probe, target{cd: cd, email: email})
			covered[email] = true
		}
	}

	dirs := make([]string, len(probe))
	for i, t := range probe {
		dirs[i] = t.cd.Dir
	}
	fresh := make([]State, 0, len(probe))
	for i, r := range RunAll(ctx, dirs, o.Now) {
		t := probe[i]
		if errors.Is(r.Err, ErrNoTime) {
			// It did not run (or was cut short): nothing to cache.
			rep.Problems = append(rep.Problems, Problem{Dir: t.cd, Email: t.email, Err: r.Err})
			continue
		}
		st := stateOf(r, t.email)
		if err := SaveState(st); err != nil {
			rep.Problems = append(rep.Problems, Problem{Dir: t.cd, Email: t.email, Err: err})
		}
		switch r.Outcome {
		case OK:
			fresh = append(fresh, st)
		case SignedOut:
			rep.SignedOut = append(rep.SignedOut, Problem{Dir: t.cd, Email: t.email})
		default:
			rep.Problems = append(rep.Problems, Problem{Dir: t.cd, Email: t.email, Err: r.Err, Shape: r.Shape})
		}
	}

	reports := batch(fresh, cached, listed)
	if len(reports) == 0 {
		return rep
	}
	res, err := o.Client.PostProbes(ctx, remote.Identity{Dev: o.Config.Dev, MachineID: o.Config.MachineID}, o.Config.Dev, reports, !o.Background)
	switch {
	case remote.IsUnknownRoute(err):
		rep.OldWorker = true
	case err != nil:
		rep.PostErr = err
	case res.Listing != nil:
		rep.Listing = res.Listing
	}
	return rep
}

// Logins maps every registered dir (cleaned path) to its current login,
// lowercased; "" when it has none or its account file cannot be read.
func Logins(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Configs))
	for _, cd := range cfg.Configs {
		email, err := claudecfg.ReadEmail(cd.Dir)
		if err != nil {
			email = ""
		}
		out[filepath.Clean(cd.Dir)] = strings.ToLower(email)
	}
	return out
}

// stale reports whether an account's numbers need a probe: none yet, older
// than after, or a window whose reset has passed (its numbers say nothing
// about the window running now).
func stale(a *remote.Account, now time.Time, after time.Duration) bool {
	at := a.UsageAt()
	if a.Session == nil || a.Week == nil || at == nil {
		return true
	}
	if a.Session.ResetPassed || a.Week.ResetPassed {
		return true
	}
	return !within(now, *at, after)
}

// within reports whether t is less than d before now. A t in the future
// (clock skew, a frozen test clock) counts as within.
func within(now, t time.Time, d time.Duration) bool {
	return now.Sub(t) < d
}

// batch picks, per account, the newest OK state, fresh probes before cached
// ones on a tie, and keeps those the listing does not show yet. A fresh
// probe goes when it is newer than the listing's numbers. A cached one is a
// resend for a listing that lagged the first write: it goes only when the
// listing neither shows those very numbers from a probe nor has numbers at
// least as new. Comparing the numbers keeps a machine whose clock runs
// ahead from resending on every run (the Worker clamps each resend to its
// own clock, which would make old numbers look new again).
func batch(fresh, cached []State, listed map[string]*remote.Account) []remote.ProbeReport {
	type pick struct {
		s     State
		fresh bool
	}
	best := map[string]pick{}
	var order []string
	consider := func(s State, isFresh bool) {
		cur, ok := best[s.Email]
		if !ok {
			order = append(order, s.Email)
		}
		if !ok || s.ProbedAt.After(cur.s.ProbedAt) || (isFresh && !cur.fresh && s.ProbedAt.Equal(cur.s.ProbedAt)) {
			best[s.Email] = pick{s, isFresh}
		}
	}
	for _, s := range fresh {
		consider(s, true)
	}
	for _, s := range cached {
		consider(s, false)
	}
	var out []remote.ProbeReport
	for _, email := range order {
		p := best[email]
		a := listed[email]
		if p.fresh {
			if a != nil && a.UsageAt() != nil && !p.s.ProbedAt.After(*a.UsageAt()) {
				continue
			}
		} else if a != nil && (shows(a, p.s) || (a.UsageAt() != nil && !p.s.ProbedAt.After(*a.UsageAt()))) {
			continue
		}
		out = append(out, remote.ProbeReport{
			Email:       email,
			Session:     remote.ProbeWindow{Used: p.s.Session.Used, ResetsAt: p.s.Session.ResetsAt},
			Week:        remote.ProbeWindow{Used: p.s.Week.Used, ResetsAt: p.s.Week.ResetsAt},
			CollectedAt: p.s.ProbedAt,
		})
	}
	return out
}

// shows reports whether a listed account already shows s's numbers from a
// probe.
func shows(a *remote.Account, s State) bool {
	same := func(w *remote.Window, p *Window) bool {
		if w == nil || p == nil || w.Used != p.Used {
			return false
		}
		if w.ResetsAt == nil || p.ResetsAt == nil {
			return w.ResetsAt == nil && p.ResetsAt == nil
		}
		return w.ResetsAt.Equal(*p.ResetsAt)
	}
	return a.Source == "probe" && same(a.Session, s.Session) && same(a.Week, s.Week)
}

// SignedOutDirs reports, per registered dir (cleaned path), whether it is
// signed out: it has no login but LastEmail names one (Claude Code cleared
// it on a sign-out), or a probe found its current login signed out. A dir
// whose account file cannot be read is not called signed out: that is a
// different problem, reported where the file is read.
func SignedOutDirs(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, cd := range cfg.Configs {
		dir := filepath.Clean(cd.Dir)
		email, err := claudecfg.ReadEmail(cd.Dir)
		switch {
		case errors.Is(err, claudecfg.ErrNotLoggedIn) && cd.LastEmail != "":
			out[dir] = true
		case err == nil && IsSignedOut(dir, email):
			out[dir] = true
		}
	}
	return out
}
