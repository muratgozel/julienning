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
	defer release()

	// Read the clock after waiting: the reconcile and cache stamps must not
	// predate the wait.
	n, err := now()
	if err != nil {
		n = time.Now()
	}
	logger.Now = n
	ctx, cancel := context.WithTimeout(context.Background(), backgroundBudget)
	defer cancel()
	u := upkeep{cfg: cfg, client: newClient(cfg, reportTimeout), logger: logger, now: n}
	u.reconcile(ctx, claims.Ending{SessionID: *ending, PID: *endingPID})
	u.refreshShared(ctx)
	u.refreshUpdates(ctx)
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
// update check. Failures are logged (throttled), never returned: nothing
// watches these processes.
type upkeep struct {
	cfg    *config.Config
	client remote.Client
	logger usage.Logger
	now    time.Time
}

func (u upkeep) reconcile(ctx context.Context, ending claims.Ending) {
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

func (u upkeep) refreshUpdates(ctx context.Context) {
	if err := refreshUpdateCheck(ctx); err != nil {
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
