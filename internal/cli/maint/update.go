package maint

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/version"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "update",
		Summary: "update julienning to the latest release (or --version vX.Y.Z)",
		Usage:   "update [--version vX.Y.Z]",
		Run:     runUpdate,
	})
}

func runUpdate(env cli.Env) error {
	fs := cli.NewFlagSet("update", env)
	// The UsageError below already reports parse failures; keep the flag
	// package from printing them a second time.
	fs.SetOutput(io.Discard)
	want := fs.String("version", "", "release tag to install instead of the latest")
	if err := fs.Parse(env.Args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return cli.Usagef("%v", err)
	}
	if fs.NArg() != 0 {
		return cli.Usagef("unexpected argument %q", fs.Arg(0))
	}
	var tag string
	if *want != "" {
		t, err := selfupdate.NormalizeTag(*want)
		if err != nil {
			return cli.Usagef("invalid --version: %v", err)
		}
		tag = t
	}

	// Only a managed install (a julienning symlink → VersionsDir/<version>)
	// can be switched safely; anything else was put there by someone else.
	link, _, err := selfupdate.ActiveLink()
	if err != nil {
		var nm *selfupdate.NotManagedError
		if errors.As(err, &nm) {
			return fmt.Errorf("%v, so `julienning update` cannot manage this installation.\nInstall with the installer instead (it sets up the updatable layout and replaces this copy):\n  %s", nm, selfupdate.InstallerCommand())
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cur := version.Version
	latest := ""
	if tag == "" {
		t, err := selfupdate.LatestTag(ctx)
		if err != nil {
			return err
		}
		tag, latest = t, t
	}
	target := selfupdate.DisplayVersion(tag)

	// Dev builds (make install, git describe) always move to the release.
	if !selfupdate.IsDevBuild(cur) {
		cmp, err := selfupdate.CompareVersions(target, cur)
		if err != nil {
			return err
		}
		upToDate := cmp == 0 || (cmp < 0 && *want == "")
		if upToDate {
			recordLatest(ctx, env, latest)
			if cmp == 0 {
				fmt.Fprintf(env.Stdout, "julienning %s is up to date.\n", target)
			} else {
				fmt.Fprintf(env.Stdout, "julienning %s is up to date (newer than the latest release %s).\n", selfupdate.DisplayVersion(cur), target)
			}
			return nil
		}
	}

	fmt.Fprintf(env.Stderr, "Downloading julienning %s (%s/%s)...\n", target, runtime.GOOS, runtime.GOARCH)
	// Install fails (and nothing is activated) unless the new binary runs
	// `version` successfully, like install.sh.
	if _, err := selfupdate.Install(ctx, tag); err != nil {
		return err
	}
	if err := selfupdate.Activate(link, target); err != nil {
		return err
	}
	// The new version is active; a failed cleanup or cache write must not
	// turn a successful update into an error, but is reported.
	if _, err := selfupdate.Prune(selfupdate.KeepVersions, link); err != nil {
		fmt.Fprintf(env.Stderr, "julienning: warning: could not remove old versions: %v\n", err)
	}
	recordLatest(ctx, env, latest)
	fmt.Fprintf(env.Stdout, "Updated %s → %s.\n", selfupdate.DisplayVersion(cur), target)
	return nil
}

// recordLatest refreshes update-check.json so no stale hint follows the
// update. latest is "" when --version was used and the latest tag is unknown.
func recordLatest(ctx context.Context, env cli.Env, latest string) {
	if latest == "" {
		t, err := selfupdate.LatestTag(ctx)
		if err != nil {
			fmt.Fprintf(env.Stderr, "julienning: warning: could not refresh the update check: %v\n", err)
			return
		}
		latest = t
	}
	if err := selfupdate.RecordLatest(latest); err != nil {
		fmt.Fprintf(env.Stderr, "julienning: warning: could not refresh the update check: %v\n", err)
	}
}
