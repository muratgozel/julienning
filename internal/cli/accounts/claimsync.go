package accounts

import (
	"context"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/usage"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "claim-sync",
		Summary: "reconcile this machine's claims with its live sessions (spawned by hooks)",
		Usage:   "claim-sync [--ending SESSION_ID] [--starting SESSION_ID]",
		Hidden:  true,
		Run:     runClaimSync,
	})
}

// Timing knobs, vars so tests do not sleep.
var (
	// lockWait is how long claim-sync waits for another run to finish. A
	// SessionEnd right after a SessionStart must not be dropped just because
	// the start's run still holds the lock: that would leave a claim behind.
	lockWait = 30 * time.Second
	// startWait bounds how long a SessionStart run waits for the new session
	// to show up in Claude's registry. Whether Claude writes it before or
	// after running hooks is undocumented; without the wait a fresh session
	// could go unclaimed until the next reconcile.
	startWait = 2 * time.Second
	pollEvery = 100 * time.Millisecond
)

// codeShareFailed is the errors.log code for a pending share on login
// (new-config) that failed and stays pending: a taken nickname, an
// unreachable Worker, an unreadable account file. Throttled like the other
// background codes (usage.BackgroundFailedEvery).
const codeShareFailed = "SHARE_FAILED"

// sessionIDRe bounds what a hook may pass on to claim-sync. Claude's session
// ids are UUIDs.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// runClaimSync is detached: nothing reads its output or exit code, so every
// problem goes to errors.log (throttled) and it always exits 0.
func runClaimSync(env cli.Env) error {
	logger, _, err := backgroundLogger()
	if err != nil {
		return nil // julienning home unusable: nowhere to report it
	}
	fs := cli.NewFlagSet("claim-sync", cli.Env{Stderr: io.Discard}) // detached: no stderr
	ending := fs.String("ending", "", "session id that is ending (excluded from the live count)")
	// --ending-pid is the hook's fallback when it could not learn the ending
	// session's id: the pid of the registry entry to exclude (hidden: only
	// the SessionEnd hook passes it).
	endingPID := fs.Int("ending-pid", 0, "pid of the registry entry that is ending (excluded from the live count)")
	starting := fs.String("starting", "", "session id that is starting (waited for in the registry)")
	if err := fs.Parse(env.Args); err != nil || fs.NArg() != 0 ||
		!validSessionArg(*ending) || !validSessionArg(*starting) || *endingPID < 0 {
		_ = logger.LogThrottledEvery(usage.CodeClaimSync, "", "claim-sync: invalid arguments", usage.BackgroundFailedEvery)
		return nil
	}

	// Config before the lock: a machine that is not set up has nothing to
	// sync and should not accumulate lock files.
	cfg, err := config.Load()
	if err != nil {
		_ = logger.LogThrottled(usage.CodeNotSetup, err.Error())
		return nil
	}
	if err := cfg.RequireRemote(); err != nil {
		_ = logger.LogThrottled(usage.CodeNotSetup, err.Error())
		return nil
	}

	if *starting != "" {
		waitForSession(*starting)
	}

	release, ok, err := acquireClaimLock(lockWait)
	if err != nil {
		_ = logger.LogThrottledEvery(usage.CodeClaimSync, "", "cannot take the claim-sync lock: "+err.Error(), usage.BackgroundFailedEvery)
		return nil
	}
	if !ok {
		return nil // another run is still busy after lockWait: it will do
	}

	u := upkeep{cfg: cfg, client: newClient(cfg, reportTimeout), logger: logger}
	func() {
		defer release()
		// Read the clock after waiting: the reconcile and cache stamps must
		// not predate the wait.
		n, err := now()
		if err != nil {
			n = time.Now()
		}
		u.now, u.logger.Now = n, n
		ctx, cancel := context.WithTimeout(context.Background(), backgroundBudget)
		defer cancel()
		u.reconcile(ctx, claims.Ending{SessionID: *ending, PID: *endingPID})
		u.refreshShared(ctx)
	}()
	u.updateIfDue()
	return nil
}

func validSessionArg(id string) bool { return id == "" || sessionIDRe.MatchString(id) }

// acquireClaimLock polls the per-machine lock for up to wait.
func acquireClaimLock(wait time.Duration) (release func(), ok bool, err error) {
	deadline := time.Now().Add(wait)
	for {
		n, clockErr := now()
		if clockErr != nil {
			n = time.Now()
		}
		release, ok, err := claims.AcquireLock(n)
		if err != nil || ok || !time.Now().Before(deadline) {
			return release, ok, err
		}
		time.Sleep(pollEvery)
	}
}

// waitForSession returns once id appears in the active config dir's live
// session registry, or after startWait. A dir without a registry is not
// waited on: it will never show up there.
func waitForSession(id string) {
	dir, err := claudecfg.ActiveDir(os.Getenv(usage.EnvConfigDir))
	if err != nil || !livesess.RegistryExists(dir) {
		return
	}
	deadline := time.Now().Add(startWait)
	for {
		sessions, _ := livesess.List(dir) // best effort: an error just means "not yet"
		for _, s := range sessions {
			if s.SessionID == id {
				return
			}
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(pollEvery)
	}
}

// upkeep is the background maintenance shared by claim-sync and the
// send-usage child: claim reconciliation, the allowlist snapshot and the
// daily update check (auto-update). Failures are logged (throttled), never
// returned: nothing watches these processes.
type upkeep struct {
	cfg    *config.Config
	client remote.Client
	logger usage.Logger
	now    time.Time
}

// reconcile shares the logins new-config marked ShareOnLogin (SPEC "Claims
// from sessions" step 0), then reconciles claims, so an account shared here
// is claimed in the same run.
func (u upkeep) reconcile(ctx context.Context, ending claims.Ending) {
	if err := claims.ResolvePendingShares(ctx, u.cfg, u.client, u.now); err != nil {
		u.fail(codeShareFailed, err)
	}
	if err := claims.Reconcile(ctx, u.cfg, u.client, ending, u.now); err != nil {
		u.fail(usage.CodeClaimSync, err)
	}
	if err := claims.MarkReconciled(u.now); err != nil {
		u.fail(usage.CodeClaimSync, err)
	}
}

// refreshShared refetches shared.json when it is stale or unreadable (a
// corrupt file is replaced rather than trusted).
func (u upkeep) refreshShared(ctx context.Context) {
	if !sharedStale(u.now) {
		return
	}
	if _, _, err := sharedcache.Refresh(ctx, u.client, u.cfg.Dev, u.now); err != nil {
		u.fail(usage.CodeSharedRefresh, err)
	}
}

// updateIfDue runs the daily update check, which installs a newer release
// unless auto-update is off (selfupdate.AutoUpdateIfDue). Callers run it
// after releasing the claim-sync lock: a slow download may outlast
// claims.LockStale, and selfupdate has its own lock and time budget, so it
// gets a context without the claim work's short deadline.
func (u upkeep) updateIfDue() {
	if err := autoUpdateIfDue(context.Background(), u.cfg); err != nil {
		u.fail(usage.CodeUpdateCheck, err)
	}
}

// sharedStale reports whether shared.json needs a refresh.
func sharedStale(now time.Time) bool {
	c, err := sharedcache.Load()
	return err != nil || c.Stale(now, sharedcache.MaxAge)
}

func (u upkeep) fail(code string, err error) {
	_ = u.logger.LogThrottledEvery(code, "", err.Error(), usage.BackgroundFailedEvery)
}
