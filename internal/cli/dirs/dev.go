package dirs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "dev",
		Host:    cli.HostForward,
		Summary: "show or change your dev name, the name teammates see on your claims and reports",
		Usage:   "dev [NEW]",
		Run:     runDev,
	})
}

// devLockWait bounds how long a rename waits for a running claim-sync; a
// var so tests do not wait.
var devLockWait = 5 * time.Second

func runDev(env cli.Env) error {
	fs := cli.NewFlagSet("dev", env)
	args, err := parseFlagsPermute(fs, env.Args)
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return cli.Usagef("expected at most one new dev name")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(env.Stdout, cfg.Dev)
		return nil
	}
	name, err := normalizeDev(args[0])
	if err != nil {
		return cli.Usagef("%v", err)
	}
	if name == cfg.Dev {
		fmt.Fprintf(env.Stdout, "Your dev name is already %s.\n", name)
		return nil
	}
	old := cfg.Dev
	if err := renameDev(env, cfg, name); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Your dev name is now %s (was %s).\n", name, old)
	return nil
}

// renameDev changes the dev name (SPEC "dev"), for julienning only: the
// claims this machine holds are released under the old name, config.json
// is saved with the new one, and the accounts in use are claimed again
// under it. Everything runs under the claim-sync lock, so a claim-sync
// spawned by a hook can neither re-claim under the old name in between nor
// lose a record of claims.json. Worker failures are warnings: claims the
// Worker never heard released expire with its claim TTL. cfg.Dev is set on
// success.
func renameDev(env cli.Env, cfg *config.Config, name string) error {
	release, err := takeClaimLock()
	if err != nil {
		return err
	}
	defer release()

	old := cfg.Dev
	var client remote.Client
	if cfg.RequireRemote() == nil {
		client = newClient(cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*remoteTimeout)
	defer cancel()

	held, err := claims.LoadHeld()
	switch {
	case err != nil:
		warnf(env, "could not read the claims held as %s (%v); the Worker drops them when its claim TTL runs out (12 h by default)", old, err)
	case len(held) > 0 && client == nil:
		warnf(env, "could not release %s held as %s (no Worker configured); the Worker drops them when its claim TTL runs out (12 h by default)", plural(len(held), "claim", "claims"), old)
	case len(held) > 0:
		// ReleaseAll claims as cfg, which still carries the old name.
		relErr := claims.ReleaseAll(ctx, cfg, client)
		if relErr != nil {
			left := len(held)
			if after, err := claims.LoadHeld(); err == nil {
				left = len(after)
			}
			warnf(env, "could not release %s held as %s (%v); the Worker drops them when its claim TTL runs out (12 h by default)", plural(left, "claim", "claims"), old, remoteErr("release claims", relErr))
		}
	}
	// What is left in claims.json was claimed under the old name; the new
	// name claims afresh below, which a fresh record would otherwise skip.
	if err == nil && len(held) > 0 {
		if err := claims.SaveHeld(claims.Held{}); err != nil {
			return err
		}
	}

	fresh, err := config.Load()
	if err != nil {
		return err
	}
	fresh.Dev = name
	if err := fresh.Save(); err != nil {
		return err
	}
	cfg.Dev = name

	if client != nil {
		n := now()
		if err := claims.Reconcile(ctx, fresh, client, claims.Ending{}, n); err != nil {
			warnf(env, "could not claim the accounts in use here as %s yet (%v); the next claim sync does it", name, remoteErr("claim", err))
		}
		if err := claims.MarkReconciled(n); err != nil {
			warnf(env, "%v", err)
		}
	}
	return nil
}

// takeClaimLock takes the per-machine claim-sync lock, retrying for up to
// devLockWait while a background claim-sync holds it.
func takeClaimLock() (func(), error) {
	deadline := time.Now().Add(devLockWait)
	for {
		release, ok, err := claims.AcquireLock(now())
		if err != nil {
			return nil, err
		}
		if ok {
			return release, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("another julienning is syncing claims right now; try again in a moment")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
