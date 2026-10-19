package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/usage"
	"github.com/muratgozel/julienning/internal/usageprobe"
)

// Timeouts: interactive commands can afford to wait a little, detached
// processes cannot (spec: 5 s per request).
const (
	interactiveTimeout = 10 * time.Second
	reportTimeout      = 5 * time.Second
	// reconcileTimeout bounds the claim sync `accounts` runs before listing:
	// it must not make a slow Worker feel twice as slow.
	reconcileTimeout = 5 * time.Second
	// backgroundBudget bounds all network work of one detached process. It
	// must stay well below claims.LockStale, or a slow run's lock would be
	// broken while it still runs.
	backgroundBudget = 25 * time.Second
	// probeBudget bounds a refresh through usage probes: two rounds of
	// usageprobe.Parallel probes (each bounded by usageprobe.Timeout) plus
	// the POST. A probe still running at the end is killed, so no result
	// can arrive after the command decided.
	probeBudget = 45 * time.Second
)

// refreshUsage probes stale accounts through Claude Code. A var because it
// runs claude: tests replace it (TestMain stubs it out), and tests never run
// the real claude.
var refreshUsage = usageprobe.Refresh

// probeAccounts refreshes the stale usage of shared accounts logged in here
// before anything is shown or ranked (SPEC "Usage probes"), waiting for every
// probe, and prints what went wrong. The listing it returns is the one to
// show: the Worker's answer with the probes merged in, else l.
func probeAccounts(env cli.Env, cfg *config.Config, l *remote.Listing, n time.Time) *remote.Listing {
	ctx, cancel := context.WithTimeout(context.Background(), probeBudget)
	defer cancel()
	rep := refreshUsage(ctx, usageprobe.Options{
		Config: cfg, Client: newClient(cfg, interactiveTimeout), Listing: l, Now: n,
		StaleAfter: usageprobe.InteractiveStale,
	})
	rep.Print(env.Stderr, n)
	if rep.Listing == nil {
		return l
	}
	return rep.Listing
}

// newClient builds the Worker client. Tests replace it with one returning a
// *remote.Fake so no command ever touches the network.
var newClient = func(cfg *config.Config, timeout time.Duration) remote.Client {
	return remote.NewHTTP(cfg.Remote.URL, cfg.Remote.Token, timeout)
}

// Update hooks, vars so tests never reach GitHub.
var (
	updateHint      = selfupdate.Hint
	autoUpdateIfDue = selfupdate.AutoUpdateIfDue
)

// spawner starts a detached julienning child. Tests replace it to capture the
// argv instead of forking.
var spawner = spawnDetached

// now reads the clock, honouring JULIENNING_NOW_EPOCH so tests and bug
// reproductions can freeze it.
func now() (time.Time, error) {
	return usage.Now(os.Getenv)
}

// location resolves the display timezone from $TZ at call time. time.Local is
// resolved once per process, which would make a t.Setenv("TZ", ...) in a test a
// silent no-op.
func location() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// notef prints something worth knowing that is not a problem.
func notef(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: note: %s\n", fmt.Sprintf(format, a...))
}

// warnf prints a non-fatal problem; the command still succeeds.
func warnf(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: warning: %s\n", fmt.Sprintf(format, a...))
}

// localConfigs maps each lowercased email to the registered dirs logged into
// it, in config order (sorted by name), and to the dirs signed out of it: a
// dir Claude Code signed out (no login, but its last_email names the
// account) or one a usage probe found signed out. Other dirs without a login
// are skipped; dirs whose account file cannot be read are returned in
// unreadable so the caller can say so instead of silently hiding them.
func localConfigs(cfg *config.Config) (byEmail, signedOut map[string][]config.ConfigDir, unreadable map[string]error) {
	byEmail = make(map[string][]config.ConfigDir, len(cfg.Configs))
	signedOut = map[string][]config.ConfigDir{}
	unreadable = map[string]error{}
	out := usageprobe.SignedOutDirs(cfg)
	for _, cd := range cfg.Configs {
		email, err := claudecfg.ReadEmail(cd.Dir)
		switch {
		case errors.Is(err, claudecfg.ErrNotLoggedIn):
			if out[filepath.Clean(cd.Dir)] {
				signedOut[cd.LastEmail] = append(signedOut[cd.LastEmail], cd)
			}
		case err != nil:
			unreadable[cd.Name] = err
		case out[filepath.Clean(cd.Dir)]:
			signedOut[email] = append(signedOut[email], cd)
		default:
			byEmail[email] = append(byEmail[email], cd)
		}
	}
	return byEmail, signedOut, unreadable
}

// summary renders "session 12% → 17:30, week 28% → Tue 21:00" for one account.
func summary(a *remote.Account, now time.Time, loc *time.Location) string {
	if a == nil || (a.Session == nil && a.Week == nil) {
		return "no usage reported yet"
	}
	return "session " + window(a.Session, now, loc) + ", week " + window(a.Week, now, loc)
}

func window(w *remote.Window, now time.Time, loc *time.Location) string {
	if w == nil {
		return "unknown"
	}
	if w.ResetsAt == nil && !w.ResetPassed {
		// A probe found the window not running: nothing used, nothing to reset.
		return usage.FormatPercent(w.Percent()) + "%"
	}
	reset := "?"
	if w.ResetsAt != nil {
		reset = usage.FormatReset(*w.ResetsAt, now, loc)
	}
	return usage.FormatPercent(w.Percent()) + "% → " + reset
}

// shortenHome replaces the home prefix with "~" for display only. Paths that
// are not under $HOME (or an unknown home) are shown as they are.
func shortenHome(path string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return path
	}
	h = filepath.Clean(h)
	if path == h {
		return "~"
	}
	if strings.HasPrefix(path, h+string(filepath.Separator)) {
		return "~" + path[len(h):]
	}
	return path
}

// printJSON writes one document with the 2-space indentation the spec asks for.
func printJSON(env cli.Env, doc any) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}
	_, err = fmt.Fprintf(env.Stdout, "%s\n", raw)
	return err
}

// self is the path detached children are started from.
func self() string {
	p, err := os.Executable()
	if err != nil || p == "" {
		return os.Args[0]
	}
	return p
}

// spawnDetached starts a child in its own session with /dev/null stdio and
// never waits for it: the status line and hooks must return immediately, and
// the child must survive Claude Code cancelling them.
func spawnDetached(self string, args []string) error {
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// backgroundLogger is the errors.log writer for detached processes. A broken
// JULIENNING_NOW_EPOCH must not stop them from logging, so the wall clock is
// the fallback.
func backgroundLogger() (usage.Logger, time.Time, error) {
	n, clockErr := now()
	if clockErr != nil {
		n = time.Now()
	}
	l, err := usage.NewLogger(n, location(), os.Getenv(usage.EnvConfigDir))
	if err != nil {
		return usage.Logger{}, n, err
	}
	if clockErr != nil {
		_ = l.LogThrottled(usage.CodeClockInvalid, clockErr.Error())
	}
	return l, n, nil
}
