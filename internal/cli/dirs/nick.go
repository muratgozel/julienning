package dirs

import (
	"context"
	"fmt"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "nick",
		Summary: "rename a team account's nickname (and its claude-<nickname> shell function)",
		Usage:   "nick NICKNAME|EMAIL|CONFIG NEW",
		Run:     runNick,
	})
}

// runNick renames the team-wide nickname in the Worker. Teammates pick it up
// when their allowlist cache refreshes; their shell functions follow at the
// next shell start.
func runNick(env cli.Env) error {
	if len(env.Args) != 2 {
		return cli.Usagef("expected the account (nickname, email or config name) and its new nickname")
	}
	newNick, err := normalizeNickname(env.Args[1])
	if err != nil {
		return cli.Usagef("%v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := cfg.RequireRemote(); err != nil {
		return err
	}
	client := newClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*remoteTimeout)
	defer cancel()
	email, old, err := accountByTarget(ctx, env, cfg, client, env.Args[0])
	if err != nil {
		return err
	}
	if old == newNick {
		fmt.Fprintf(env.Stdout, "%s is already called %s.\n", email, newNick)
		return nil
	}
	if err := client.SetNickname(ctx, email, newNick); err != nil {
		switch {
		case remote.IsConflict(err):
			return fmt.Errorf("%w; pick another name", nicknameTaken(newNick, takenBy(ctx, env, cfg, client, newNick, email), err))
		case remote.IsNotShared(err):
			if _, cerr := sharedcache.Remove(email); cerr != nil {
				warnf(env, "could not update the allowlist cache: %v", cerr)
			}
			return fmt.Errorf("%s is not shared with the team (share it with: julienning share %s --nick %s)", email, email, newNick)
		default:
			return remoteErr("rename "+email, err)
		}
	}
	if _, err := sharedcache.AddWithNickname(email, newNick); err != nil {
		warnf(env, "could not update the allowlist cache: %v", err)
	}
	if old != "" {
		fmt.Fprintf(env.Stdout, "Renamed %s to %s (%s).\n", old, newNick, email)
		fmt.Fprintf(env.Stdout, "Shell function claude-%s replaces claude-%s in new terminals (or run: %s).\n", newNick, old, sourceHint(""))
	} else {
		fmt.Fprintf(env.Stdout, "Named %s %s.\n", email, newNick)
		fmt.Fprintf(env.Stdout, "Shell function claude-%s is available in new terminals (or run: %s).\n", newNick, sourceHint(""))
	}
	return nil
}
