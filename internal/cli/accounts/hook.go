package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/usage"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "hook",
		Summary: "Claude Code hooks: SessionStart/SessionEnd keep claims in sync, StopFailure reports a usage-limit refusal",
		Usage:   "hook session-start|session-end|stop-failure",
		Hidden:  true,
		Run:     runHook,
	})
}

// hookStdinWait bounds the stdin read. Claude Code writes the payload at
// once, but a hook run by hand from a terminal would otherwise block forever,
// and hooks must return in well under 100 ms.
var hookStdinWait = 50 * time.Millisecond

// Process ancestry, vars so tests never depend on (or run ps against) the
// real process tree.
var (
	hookParent = os.Getppid
	parentOf   = psParent
)

// psParentWait bounds the one ps call a SessionEnd without a session id may
// make; on timeout the hook falls back to --ending-pid.
const psParentWait = 100 * time.Millisecond

// Hook events, as the argument after `hook`.
const (
	hookSessionStart = "session-start"
	hookSessionEnd   = "session-end"
	hookStopFailure  = "stop-failure"
)

// hookInput is the part of Claude Code's hook payload julienning uses.
type hookInput struct {
	SessionID string
	Source    string // SessionStart: startup | resume | clear | compact
	Reason    string // SessionEnd: clear | logout | prompt_input_exit | other
	// StopFailure: the API error that ended the turn ("rate_limit" for a
	// usage limit) and the refusal text, which names the limit and its
	// reset. Never logged: it is free text.
	Error                string
	ErrorDetails         string
	LastAssistantMessage string
}

// errNoSessionID is readHookInput's error for a payload without a usable
// session_id; the other fields are still filled in.
var errNoSessionID = errors.New("hook input has no valid session_id")

// runHook never fails and never prints: Claude Code shows hook output and
// errors to the user, and a claim is not worth interrupting a session for.
// Every problem is logged (throttled) and the hook still exits 0. It never
// touches the network: the detached claim-sync (or send-exhausted) does that.
func runHook(env cli.Env) error {
	logger, n, err := backgroundLogger()
	if err != nil {
		return nil // julienning home unusable: nowhere to report it
	}
	logf := func(code, message string) { _ = logger.LogThrottled(code, message) }

	if len(env.Args) != 1 || (env.Args[0] != hookSessionStart && env.Args[0] != hookSessionEnd && env.Args[0] != hookStopFailure) {
		logf(usage.CodeHookInput, "hook expects exactly one of session-start, session-end, stop-failure")
		return nil
	}
	event := env.Args[0]

	// Read before gating, so Claude Code never writes into a closed pipe.
	in, err := readHookInput(env.Stdin, hookStdinWait)
	switch {
	case err == nil:
	case event == hookStopFailure && errors.Is(err, errNoSessionID):
		// StopFailure has no use for the session id.
	default:
		logf(usage.CodeHookInput, err.Error()) // keep going: the ids only sharpen the sync
	}
	// The settings.json matcher already limits StopFailure to rate_limit;
	// this keeps a hand-edited matcher from reporting other API errors.
	if event == hookStopFailure && in.Error != "rate_limit" {
		return nil
	}

	// Gate, all local: set up and registered, for every event.
	cfg, err := config.Load()
	if err != nil {
		logf(usage.CodeNotSetup, err.Error())
		return nil
	}
	envDir := os.Getenv(usage.EnvConfigDir)
	dir, err := claudecfg.ActiveDir(envDir)
	if err != nil {
		logf(usage.CodeAccountFile, "cannot resolve the config dir: "+err.Error())
		return nil
	}
	cd, ok := cfg.FindByDir(dir)
	if !ok {
		logf(usage.CodeNotShared, "config dir is not registered with julienning")
		return nil
	}

	if event == hookStopFailure {
		// Same gate as SessionStart, minus the pending share: an account is
		// only reported once it is on the allowlist.
		email, ok := sharedLogin(logf, envDir, false)
		if !ok {
			return nil
		}
		if err := spawner(self(), exhaustedArgs(logger.Dir, email, in, n)); err != nil {
			logf(usage.CodeSpawnFailed, "cannot start send-exhausted: "+err.Error())
		}
		return nil
	}

	args := []string{"claim-sync"}
	if event == hookSessionEnd {
		// /clear ends one session and starts another in the same process: the
		// account stays in use. Syncing here would race the SessionStart run
		// and could release a claim that is still live.
		if in.Reason == "clear" {
			return nil
		}
		// No login or allowlist check here: after /logout the account file
		// has no email, and a stale or unreadable shared.json proves nothing.
		// claim-sync releases only what claims.json says this machine holds.
		if in.SessionID != "" {
			args = append(args, "--ending", in.SessionID)
		} else {
			more, err := endingArgs(dir)
			if err != nil {
				logf(usage.CodeClaimSync, "cannot identify the ending session: "+err.Error())
			}
			args = append(args, more...)
		}
	} else {
		// Same gate as the status line: only a shared login may be claimed,
		// except in a dir new-config marked ShareOnLogin, whose login
		// claim-sync shares first (claims.ResolvePendingShares).
		if _, ok := sharedLogin(logf, envDir, cd.ShareOnLogin != nil); !ok {
			return nil
		}
		if in.SessionID != "" {
			args = append(args, "--starting", in.SessionID)
		}
	}
	if err := spawner(self(), args); err != nil {
		logf(usage.CodeSpawnFailed, "cannot start claim-sync: "+err.Error())
	}
	return nil
}

// sharedLogin is the login gate of SessionStart and StopFailure: it returns
// the (lowercased) login of the account file this Claude process uses when
// it is on the team allowlist, or, with pending (a dir whose share waits for
// its login), whatever it is. The file is not the registered dir's when
// CLAUDE_CONFIG_DIR=~/.claude is set. ok is false when the hook must stop;
// every reason but "not logged in" (a fresh dir: nothing to do) is logged.
func sharedLogin(logf func(code, message string), envDir string, pending bool) (email string, ok bool) {
	accountFile, err := claudecfg.AccountFileForEnv(envDir)
	if err != nil {
		logf(usage.CodeAccountFile, "cannot resolve the account file: "+err.Error())
		return "", false
	}
	email, err = usage.LoginEmail(accountFile)
	if errors.Is(err, claudecfg.ErrNotLoggedIn) {
		return "", false
	}
	if err != nil {
		logf(usage.CodeAccountFile, err.Error())
		return "", false
	}
	cache, err := sharedcache.Load()
	if err != nil && !pending {
		logf(usage.CodeNotShared, "shared account cache is unreadable, treating the account as not shared: "+err.Error())
		return "", false
	}
	if !pending && !cache.Contains(email) {
		logf(usage.CodeNotShared, "account is not on the team allowlist (shared.json)")
		return "", false
	}
	return email, true
}

// exhaustedArgs is the send-exhausted argv for a usage-limit refusal of
// email, decided here from local state only (the hook has no time for the
// network): the window and reset come from the refusal text, else from the
// last report this machine sent for the account (sent/ under home).
func exhaustedArgs(home, email string, in hookInput, now time.Time) []string {
	text := in.ErrorDetails + "\n" + in.LastAssistantMessage
	sent, _ := usage.LoadSent(home, email) // nil when missing or corrupt
	window := usage.RefusalWindow(text, sent, now)
	args := []string{"send-exhausted", "--email", email, "--window", window}
	if reset, ok := usage.RefusalReset(text, window, sent, now); ok {
		args = append(args, "--resets", strconv.FormatInt(reset.Unix(), 10))
	}
	return args
}

// endingArgs names the ending session for claim-sync when the SessionEnd
// input had no usable session id: the registry entry of dir whose process is
// the hook's parent (Claude ran the hook directly) or grandparent (through a
// shell). Without such an entry it falls back to --ending-pid with the
// parent's pid, so claim-sync still excludes it should it be registered.
// Without either, claim-sync would count the ending session as live and keep
// the claim until the next reconcile.
func endingArgs(dir string) ([]string, error) {
	parent := hookParent()
	if parent <= 1 {
		return nil, nil // reparented to init: the session is already gone
	}
	fallback := []string{"--ending-pid", strconv.Itoa(parent)}
	sessions, err := livesess.List(dir)
	if err != nil {
		return fallback, err
	}
	if len(sessions) == 0 {
		return fallback, nil
	}
	byPID := make(map[int]livesess.Session, len(sessions))
	for _, s := range sessions {
		byPID[s.PID] = s
	}
	s, ok := byPID[parent]
	if !ok {
		// A failed lookup is not an error: the fallback is the answer.
		if grand, err := parentOf(parent); err == nil {
			s, ok = byPID[grand]
		}
	}
	switch {
	case !ok:
		return fallback, nil
	case sessionIDRe.MatchString(s.SessionID):
		return []string{"--ending", s.SessionID}, nil
	default:
		return []string{"--ending-pid", strconv.Itoa(s.PID)}, nil
	}
}

// psParent returns pid's parent pid, bounded by psParentWait.
func psParent(pid int) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psParentWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps -o ppid= -p %d: %w", pid, err)
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || ppid <= 0 {
		return 0, fmt.Errorf("ps -o ppid= -p %d: unexpected output", pid)
	}
	return ppid, nil
}

// readHookInput decodes one JSON object from r within wait. Unknown fields
// are ignored and missing ones stay empty; a session id that does not look
// like one is dropped (it is passed on as an argument).
func readHookInput(r io.Reader, wait time.Duration) (hookInput, error) {
	if r == nil {
		return hookInput{}, errors.New("hook input missing: no stdin")
	}
	type result struct {
		doc map[string]any
		err error
	}
	// Decode returns as soon as one value is complete, so a writer that keeps
	// stdin open does not cost the whole wait.
	done := make(chan result, 1)
	go func() {
		var doc map[string]any
		err := json.NewDecoder(io.LimitReader(r, usage.MaxInputBytes)).Decode(&doc)
		done <- result{doc, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(wait):
		return hookInput{}, errors.New("hook input missing: nothing on stdin within " + wait.String())
	}
	if res.err != nil || res.doc == nil {
		return hookInput{}, errors.New("hook input is not a JSON object")
	}
	str := func(k string) string { s, _ := res.doc[k].(string); return s }
	in := hookInput{
		SessionID: str("session_id"), Source: str("source"), Reason: str("reason"),
		Error: str("error"), ErrorDetails: str("error_details"), LastAssistantMessage: str("last_assistant_message"),
	}
	if !sessionIDRe.MatchString(in.SessionID) {
		in.SessionID = ""
		return in, errNoSessionID
	}
	return in, nil
}
