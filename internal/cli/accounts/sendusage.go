package accounts

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/usage"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "send-usage",
		Summary: "report one usage snapshot to the Worker, then background upkeep (spawned by statusline)",
		Usage: "send-usage --email E --session-used P --session-resets EPOCH " +
			"--week-used P --week-resets EPOCH --collected-at EPOCH [--now]",
		Hidden: true,
		Run:    runSendUsage,
	})
}

func runSendUsage(env cli.Env) error {
	fs := cli.NewFlagSet("send-usage", env)
	email := fs.String("email", "", "account email")
	sessionUsed := fs.Float64("session-used", -1, "five-hour window usage percentage")
	sessionResets := fs.Int64("session-resets", 0, "five-hour window reset, unix epoch seconds")
	weekUsed := fs.Float64("week-used", -1, "seven-day window usage percentage")
	weekResets := fs.Int64("week-resets", 0, "seven-day window reset, unix epoch seconds")
	collectedAt := fs.Int64("collected-at", 0, "when the numbers were read, unix epoch seconds")
	force := fs.Bool("now", false, "bypass the debounce (manual testing)")
	if err := fs.Parse(env.Args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return cli.Usagef("send-usage takes no positional arguments")
	}
	// Validate at the boundary: this runs detached, so a bad value must fail
	// here rather than reach the Worker.
	if !claudecfg.ValidEmail(*email) {
		return cli.Usagef("--email must be a valid email address")
	}
	if err := checkPercent("--session-used", *sessionUsed); err != nil {
		return err
	}
	if err := checkPercent("--week-used", *weekUsed); err != nil {
		return err
	}
	if err := checkEpoch("--session-resets", *sessionResets); err != nil {
		return err
	}
	if err := checkEpoch("--week-resets", *weekResets); err != nil {
		return err
	}
	if err := checkEpoch("--collected-at", *collectedAt); err != nil {
		return err
	}

	n, err := now()
	if err != nil {
		return cli.Usagef("%s", err)
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	logger := usage.Logger{Dir: dir, ConfigDir: os.Getenv(usage.EnvConfigDir), Now: n, Loc: location()}

	cfg, err := config.Load()
	if err != nil {
		// Nothing is watching this process; leave a breadcrumb and stop.
		_ = logger.LogThrottled(usage.CodeNotSetup, err.Error())
		return nil
	}
	if err := cfg.RequireRemote(); err != nil {
		_ = logger.LogThrottled(usage.CodeNotSetup, err.Error())
		return nil
	}

	// One budget for everything this detached process does on the network.
	ctx, cancel := context.WithTimeout(context.Background(), backgroundBudget)
	defer cancel()
	client := newClient(cfg, reportTimeout)
	addr := strings.ToLower(*email)
	payload := usage.Payload{
		SessionUsed:   *sessionUsed,
		SessionResets: *sessionResets,
		WeekUsed:      *weekUsed,
		WeekResets:    *weekResets,
	}
	report := remote.UsageReport{
		Session:     remote.UsageWindow{Used: *sessionUsed, ResetsAt: time.Unix(*sessionResets, 0).UTC()},
		Week:        remote.UsageWindow{Used: *weekUsed, ResetsAt: time.Unix(*weekResets, 0).UTC()},
		CollectedAt: time.Unix(*collectedAt, 0).UTC(),
		Reporter:    remote.Identity{Dev: cfg.Dev, MachineID: cfg.MachineID},
	}
	sendReport(ctx, client, logger, dir, addr, payload, report, *force, time.Duration(cfg.SendMinIntervalSec)*time.Second)

	// Upkeep rides on the reporter because it runs while Claude is in use,
	// which is exactly when crashed sessions (no SessionEnd) need cleaning up.
	// It runs even when the report was debounced; its own markers keep it
	// rare, and the claim-sync lock keeps it to one process per machine.
	backgroundUpkeep(ctx, upkeep{cfg: cfg, client: client, logger: logger, now: n})
	return nil
}

// sendReport PUTs one usage snapshot, applying the privacy gate, debounce and
// in-flight lock. Failures are logged, never returned.
func sendReport(ctx context.Context, client remote.Client, logger usage.Logger, dir, addr string,
	payload usage.Payload, report remote.UsageReport, force bool, minInterval time.Duration) {
	// The status line gated on this already; check again because this is
	// the process that actually sends, and --email is just an argument.
	cache, err := sharedcache.Load()
	if err != nil {
		_ = logger.LogThrottled(usage.CodeNotShared, "shared account cache is unreadable, treating the account as not shared: "+err.Error())
		return
	}
	if !cache.Contains(addr) {
		_ = logger.LogThrottled(usage.CodeNotShared, "account is not on the team allowlist (shared.json)")
		return
	}

	n := logger.Now
	prev, _ := usage.LoadSent(dir, addr)
	if !force && !usage.ShouldSend(prev, payload, n, minInterval) {
		return
	}

	// One report per account at a time: several status line renders can spawn
	// concurrent processes with a cold cache, and they would all PUT.
	release, ok, err := usage.AcquireInflight(dir, addr, n)
	if err != nil {
		sendFailed(logger, addr, "cannot take the send lock: "+err.Error())
		return
	}
	if !ok {
		return
	}
	defer release()

	// Record the attempt *before* the request: an unreachable Worker must not
	// fan out one 5 s process (and one log line) per status line refresh.
	attempt := usage.Sent{Payload: payload, AttemptedAt: n.UTC()}
	if prev != nil {
		attempt.SentAt = prev.SentAt
	}
	if err := usage.SaveSent(dir, addr, attempt); err != nil {
		sendFailed(logger, addr, "the debounce cache could not be written: "+err.Error())
	}

	err = client.PutUsage(ctx, addr, report)
	if remote.IsNotShared(err) {
		// A teammate unshared it since shared.json was fetched. Dropping it
		// from the cache stops the status line from spawning reports for it,
		// so this line is written once, not per render.
		msg := "the Worker says this account is not shared; removed it from shared.json"
		if _, ferr := sharedcache.Remove(addr); ferr != nil {
			msg = "the Worker says this account is not shared, but shared.json could not be updated: " + ferr.Error()
		}
		_ = logger.Log(usage.CodeNotShared, scrub(msg, addr))
		return
	}
	if err != nil {
		sendFailed(logger, addr, err.Error())
		return
	}
	attempt.SentAt = n.UTC()
	if err := usage.SaveSent(dir, addr, attempt); err != nil {
		sendFailed(logger, addr, "sent, but the debounce cache could not be written: "+err.Error())
	}
}

// backgroundUpkeep reconciles claims at most every claims.ReconcileEvery and
// refreshes shared.json when stale. Nothing due means no lock and no I/O
// beyond two stats, so running it on every render is cheap.
func backgroundUpkeep(ctx context.Context, u upkeep) {
	reconcile := claims.ReconcileDue(u.now)
	stale := sharedStale(u.now)
	if !reconcile && !stale {
		return
	}
	release, ok, err := claims.AcquireLock(u.now)
	if err != nil {
		u.fail(usage.CodeClaimSync, err)
		return
	}
	if !ok {
		return // claim-sync (or another reporter) is doing this right now
	}
	defer release()
	if reconcile {
		u.reconcile(ctx, claims.Ending{})
	}
	u.refreshShared(ctx)
	u.refreshUpdates(ctx)
}

// sendFailed records one SEND_FAILED line, at most once per 10 minutes per
// account, so an outage leaves a breadcrumb instead of a flood. The throttle
// marker is keyed by usage.KeyHash, like the sent/ cache files: neither the
// log nor any file name may carry an address.
func sendFailed(logger usage.Logger, email, message string) {
	_ = logger.LogThrottledEvery(usage.CodeSendFailed, email, scrub(message, email), usage.SendFailedEvery)
}

func checkPercent(flag string, v float64) error {
	if v < 0 || v > 100 {
		return cli.Usagef("%s must be a percentage in [0,100]", flag)
	}
	return nil
}

func checkEpoch(flag string, v int64) error {
	if v <= 0 {
		return cli.Usagef("%s must be positive unix epoch seconds", flag)
	}
	return nil
}

// scrub is a last line of defence: errors.log must never contain an email,
// whatever a future transport error decides to include.
func scrub(msg, email string) string {
	if email == "" {
		return msg
	}
	return strings.ReplaceAll(msg, email, "<email>")
}
