package accounts

import (
	"context"
	"flag"
	"os"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/usage"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "send-exhausted",
		Summary: "report that Claude refused a request for a usage limit (spawned by the StopFailure hook)",
		Usage:   "send-exhausted --email E --window session|week [--resets EPOCH]",
		Hidden:  true,
		Run:     runSendExhausted,
	})
}

// runSendExhausted marks the account exhausted on the Worker. It runs
// detached: bad arguments are usage errors (nobody sees them, but nothing
// invalid reaches the Worker), everything else is logged and exits 0.
func runSendExhausted(env cli.Env) error {
	fs := cli.NewFlagSet("send-exhausted", env)
	email := fs.String("email", "", "account email")
	window := fs.String("window", "", "the exhausted window: session or week")
	resets := fs.Int64("resets", 0, "when the window resets, unix epoch seconds (omit when unknown)")
	if err := fs.Parse(env.Args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return cli.Usagef("send-exhausted takes no positional arguments")
	}
	if !claudecfg.ValidEmail(*email) {
		return cli.Usagef("--email must be a valid email address")
	}
	if *window != remote.WindowSession && *window != remote.WindowWeek {
		return cli.Usagef("--window must be session or week")
	}
	var resetsAt *time.Time
	if flagSet(fs, "resets") {
		if err := checkEpoch("--resets", *resets); err != nil {
			return err
		}
		t := time.Unix(*resets, 0).UTC()
		resetsAt = &t
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
		_ = logger.LogThrottled(usage.CodeNotSetup, err.Error())
		return nil
	}
	if err := cfg.RequireRemote(); err != nil {
		_ = logger.LogThrottled(usage.CodeNotSetup, err.Error())
		return nil
	}
	// The hook gated on this already; check again because this is the
	// process that sends, and --email is just an argument.
	addr := strings.ToLower(*email)
	cache, err := sharedcache.Load()
	if err != nil {
		_ = logger.LogThrottled(usage.CodeNotShared, "shared account cache is unreadable, treating the account as not shared: "+err.Error())
		return nil
	}
	if !cache.Contains(addr) {
		_ = logger.LogThrottled(usage.CodeNotShared, "account is not on the team allowlist (shared.json)")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), backgroundBudget)
	defer cancel()
	err = newClient(cfg, reportTimeout).PutExhausted(ctx, addr, remote.ExhaustedReport{
		Window:   *window,
		ResetsAt: resetsAt,
		Reporter: remote.Identity{Dev: cfg.Dev, MachineID: cfg.MachineID},
	})
	switch {
	case err == nil:
	case remote.IsNotShared(err):
		// A teammate unshared it since shared.json was fetched: drop it, as
		// send-usage does, so its hooks and status line stop reporting it.
		msg := "the Worker says this account is not shared; removed it from shared.json"
		if _, ferr := sharedcache.Remove(addr); ferr != nil {
			msg = "the Worker says this account is not shared, but shared.json could not be updated: " + ferr.Error()
		}
		_ = logger.Log(usage.CodeNotShared, scrub(msg, addr))
	case remote.IsNotFound(err):
		exhaustedFailed(logger, addr, "the Worker has no exhausted report route ("+err.Error()+"); it is older than this julienning and needs a redeploy")
	default:
		exhaustedFailed(logger, addr, err.Error())
	}
	return nil
}

// exhaustedFailed records one EXHAUSTED_FAILED line, at most once per
// usage.BackgroundFailedEvery per account (keyed by hash, like SEND_FAILED).
func exhaustedFailed(logger usage.Logger, email, message string) {
	_ = logger.LogThrottledEvery(usage.CodeExhaustedFailed, email, scrub(message, email), usage.BackgroundFailedEvery)
}

// flagSet reports whether name was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}
