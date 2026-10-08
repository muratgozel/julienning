package accounts

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/usage"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "statusline",
		Summary: "render Claude Code's status line and report usage (statusLine hook)",
		Usage:   "statusline",
		Run:     runStatusline,
	})
}

// runStatusline always returns nil: a non-zero exit blanks Claude Code's status
// line, so every problem is reported on stdout and in errors.log instead.
func runStatusline(env cli.Env) error {
	s := &statusline{env: env, now: time.Now(), loc: location(), configDir: os.Getenv(usage.EnvConfigDir)}
	s.run()
	return nil
}

type statusline struct {
	env       cli.Env
	line      string // the status line so far; printed before any error row
	now       time.Time
	loc       *time.Location
	configDir string
}

func (s *statusline) run() {
	n, err := usage.Now(os.Getenv)
	if err != nil {
		s.report(usage.CodeClockInvalid, err.Error(), "")
		return
	}
	s.now = n

	in, err := usage.Parse(s.env.Stdin)
	if err != nil {
		s.report(usage.CodeInvalidStdin, err.Error(), "")
		return
	}
	s.line = usage.StatusLine(in)

	if in.Invalid() {
		s.report(usage.CodeUsageInvalid, "rate_limits in status line input are malformed", in.RateLimitsSignature())
		return
	}
	// An absent window is normal before the first reply and right after a
	// reset, so there is no error row; the throttled log line (shape only,
	// never numbers) is what explains a "usage pending" that never clears.
	if !in.Complete() {
		s.logThrottled(usage.CodeUsagePending, "status line input lacks a rate limit window; usage is not reported until both are there "+in.RateLimitsSignature())
		s.printf("%s · %s\n", s.line, usage.Pending)
		return
	}

	// Gate, cheapest first and without any network: julienning set up, dir
	// registered, account shared. Registration is checked before the account
	// file is read: a personal or brand new dir has no .claude.json, and
	// reading it first would put an error row on every render of a dir
	// julienning does not manage. Every code here is throttled because this
	// runs on every render.
	cfg, err := config.Load()
	if err != nil {
		s.logThrottled(usage.CodeNotSetup, err.Error())
		s.printf("%s\n", s.line)
		return
	}
	// ActiveDir, not CLAUDE_CONFIG_DIR itself: unset means the default dir,
	// whose login lives in ~/.claude.json (claudecfg.AccountFilePath).
	dir, err := claudecfg.ActiveDir(s.configDir)
	if err != nil {
		s.logThrottled(usage.CodeAccountFile, "cannot resolve the config dir: "+err.Error())
		s.printf("%s\n", s.line)
		return
	}
	if _, ok := cfg.FindByDir(dir); !ok {
		s.logThrottled(usage.CodeNotShared, "config dir is not registered with julienning")
		s.printf("%s\n", s.line)
		return
	}

	// The login comes from the file this Claude process reads, which differs
	// from the registered dir's file when CLAUDE_CONFIG_DIR=~/.claude is
	// exported explicitly (see claudecfg.DefaultDir).
	accountFile, err := claudecfg.AccountFileForEnv(s.configDir)
	if err != nil {
		s.logThrottled(usage.CodeAccountFile, "cannot resolve the account file: "+err.Error())
		s.printf("%s\n", s.line)
		return
	}
	email, err := usage.AccountEmail(accountFile)
	if err != nil {
		// A registered dir without a login is a steady state (a fresh
		// new-config waiting for `julienning login`), not a problem the user
		// can act on from here: no error row, one throttled log line.
		code, message := usage.CodeAccountFile, err.Error()
		var ce *usage.CodedError
		if errors.As(err, &ce) {
			code, message = ce.Code, ce.Message
		}
		s.logThrottled(code, message)
		s.printf("%s\n", s.line)
		return
	}

	// A registered dir reports only while its current login is on the team
	// allowlist: logins move between dirs, and personal usage must never
	// leave the machine. No readable cache means nothing is shared.
	cache, err := sharedcache.Load()
	if err != nil {
		s.logThrottled(usage.CodeNotShared, "shared account cache is unreadable, treating the account as not shared: "+err.Error())
		s.printf("%s\n", s.line)
		return
	}
	if !cache.Contains(email) {
		s.logThrottled(usage.CodeNotShared, "account is not on the team allowlist (shared.json)")
		s.printf("%s\n", s.line)
		return
	}

	if err := s.spawn(strings.ToLower(email), in); err != nil {
		s.report(usage.CodeSpawnFailed, "cannot start the usage reporter: "+err.Error(), "")
		return
	}
	s.printf("%s\n", s.line)
}

func (s *statusline) spawn(email string, in usage.Input) error {
	return spawner(self(), []string{
		"send-usage",
		"--email", email,
		"--session-used", usage.FormatPercent(in.Session.Used),
		"--session-resets", strconv.FormatInt(in.Session.ResetsAt, 10),
		"--week-used", usage.FormatPercent(in.Week.Used),
		"--week-resets", strconv.FormatInt(in.Week.ResetsAt, 10),
		"--collected-at", strconv.FormatInt(s.now.Unix(), 10),
	})
}

func (s *statusline) printf(format string, a ...any) {
	fmt.Fprintf(s.env.Stdout, format, a...)
}

// report prints the status line (when there is one) plus an error row, and
// records the failure. extra carries debugging detail for the log only. The
// row repeats on every render; the log line is throttled like every other
// status line code, because this runs several times a second.
func (s *statusline) report(code, message, extra string) {
	if s.line != "" {
		s.printf("%s\n", s.line)
	}
	s.printf("julienning: %s\n", message)
	logged := message
	if extra != "" {
		logged += " " + extra
	}
	if err := s.throttled(code, logged); err != nil {
		s.printf("julienning: %s %v\n", usage.CodeLogFailed, err)
	}
}

// logThrottled is for gate outcomes, which print no error row: a log failure
// there has nowhere visible to go.
func (s *statusline) logThrottled(code, message string) {
	_ = s.throttled(code, message)
}

func (s *statusline) throttled(code, message string) error {
	l, err := usage.NewLogger(s.now, s.loc, s.configDir)
	if err != nil {
		return err
	}
	return l.LogThrottled(code, message)
}
