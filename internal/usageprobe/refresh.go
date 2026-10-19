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
	// it are probed: personal usage never leaves the machine) and how fresh
	// each account's numbers are.
	Listing *remote.Listing
	Now     time.Time
	// StaleAfter is InteractiveStale or BackgroundStale.
	StaleAfter time.Duration
	// Only limits the refresh to one account (lowercased email; `use`).
	Only string
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
	// Probed counts the probes that ran.
	Probed int
	// Problems are probes that failed.
	Problems []Problem
	// SignedOut are dirs a probe just found signed out.
	SignedOut []Problem
	// Changes are registered dirs whose login moved to another account.
	Changes []config.LoginChange
	// RecordErr: the dirs' logins could not be recorded in config.json.
	RecordErr error
	// OldWorker: the Worker predates POST /probes and needs a redeploy.
	OldWorker bool
	// PostErr: sending the probes failed otherwise.
	PostErr error
	// NotShared are probed emails the Worker says are not shared.
	NotShared []string
}

// target is one dir to probe, or whose cached result to re-send.
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
	listed := make(map[string]*remote.Account, len(o.Listing.Accounts))
	for i := range o.Listing.Accounts {
		a := &o.Listing.Accounts[i]
		listed[strings.ToLower(a.Email)] = a
	}

	logins := Logins(o.Config)
	rep.Changes, rep.RecordErr = config.RecordLogins(logins)

	var probe []target
	var cached []State
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
			case SignedOut:
				// Said again on every run: next skips the dir, so it must
				// not go quiet just because the mark is recent.
				rep.SignedOut = append(rep.SignedOut, Problem{Dir: cd, Email: email})
			}
			continue
		}
		signedOut := st != nil && st.Outcome == SignedOut && st.Email == email
		if signedOut || stale(acct, o.Now, o.StaleAfter) {
			probe = append(probe, target{cd: cd, email: email})
		}
	}

	dirs := make([]string, len(probe))
	for i, t := range probe {
		dirs[i] = t.cd.Dir
	}
	results := RunAll(ctx, dirs, o.Now)
	rep.Probed = len(results)
	fresh := make([]State, 0, len(results))
	for i, r := range results {
		t := probe[i]
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

	reports := batch(append(fresh, cached...), listed)
	if len(reports) == 0 {
		return rep
	}
	res, err := o.Client.PostProbes(ctx, remote.Identity{Dev: o.Config.Dev, MachineID: o.Config.MachineID}, o.Config.Dev, reports)
	switch {
	case remote.IsUnknownRoute(err):
		rep.OldWorker = true
	case err != nil:
		rep.PostErr = err
	case res.Listing == nil:
		rep.PostErr = errors.New("the Worker answered without a listing")
	default:
		rep.Listing, rep.NotShared = res.Listing, res.NotShared
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
	if a.Session == nil || a.Week == nil || a.CollectedAt == nil {
		return true
	}
	if a.Session.ResetPassed || a.Week.ResetPassed {
		return true
	}
	return !within(now, *a.CollectedAt, after)
}

// within reports whether t is less than d before now. A t in the future
// (clock skew, a frozen test clock) counts as within.
func within(now, t time.Time, d time.Duration) bool {
	return now.Sub(t) < d
}

// batch picks, per account, the newest OK state, and keeps only those newer
// than the listing's numbers: anything older would be dropped by the Worker
// anyway.
func batch(states []State, listed map[string]*remote.Account) []remote.ProbeReport {
	best := map[string]State{}
	var order []string
	for _, s := range states {
		cur, ok := best[s.Email]
		if !ok {
			order = append(order, s.Email)
		}
		if !ok || s.ProbedAt.After(cur.ProbedAt) {
			best[s.Email] = s
		}
	}
	var out []remote.ProbeReport
	for _, email := range order {
		s := best[email]
		if a := listed[email]; a != nil && a.CollectedAt != nil && !s.ProbedAt.After(*a.CollectedAt) {
			continue
		}
		out = append(out, remote.ProbeReport{
			Email:       email,
			Session:     remote.ProbeWindow{Used: s.Session.Used, ResetsAt: s.Session.ResetsAt},
			Week:        remote.ProbeWindow{Used: s.Week.Used, ResetsAt: s.Week.ResetsAt},
			CollectedAt: s.ProbedAt,
		})
	}
	return out
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
