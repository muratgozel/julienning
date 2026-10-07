package accounts

import (
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
)

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

// warnf prints a non-fatal problem; the command still succeeds.
func warnf(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: warning: %s\n", fmt.Sprintf(format, a...))
}

// localConfigs maps each lowercased email to the registered dirs logged into
// it, in config order (sorted by name). Dirs without a login are skipped;
// dirs whose account file cannot be read are returned in unreadable so the
// caller can say so instead of silently hiding them.
func localConfigs(cfg *config.Config) (byEmail map[string][]config.ConfigDir, unreadable map[string]error) {
	byEmail = make(map[string][]config.ConfigDir, len(cfg.Configs))
	unreadable = map[string]error{}
	for _, cd := range cfg.Configs {
		email, err := claudecfg.ReadEmail(cd.Dir)
		switch {
		case errors.Is(err, claudecfg.ErrNotLoggedIn):
		case err != nil:
			unreadable[cd.Name] = err
		default:
			byEmail[email] = append(byEmail[email], cd)
		}
	}
	return byEmail, unreadable
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
