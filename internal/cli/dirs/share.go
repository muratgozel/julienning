package dirs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/shell"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "share",
		Summary: "add an account to the team allowlist under a team-wide nickname",
		Usage:   "share EMAIL [--nick NAME]",
		Run:     runShare,
	})
	cli.Register(&cli.Command{
		Name:    "unshare",
		Summary: "remove an account from the team allowlist (deletes its usage and claims)",
		Usage:   "unshare NICKNAME|EMAIL|CONFIG [--yes]",
		Run:     runUnshare,
	})
}

func runShare(env cli.Env) error {
	fs := cli.NewFlagSet("share", env)
	nickFlag := fs.String("nick", "", "team-wide nickname (default: the email's local part)")
	args, err := parseFlagsPermute(fs, env.Args)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return cli.Usagef("expected exactly one email")
	}
	email, err := normalizeEmail(args[0])
	if err != nil {
		return err
	}
	nick := defaultNickname(email)
	if *nickFlag != "" {
		if nick, err = normalizeNickname(*nickFlag); err != nil {
			return cli.Usagef("%v", err)
		}
	} else if nick == "" {
		return cli.Usagef("cannot derive a nickname from %s; pass --nick NAME (%s)", email, shell.NicknameRule)
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
	if err := client.Share(ctx, email, nick, remote.Identity{Dev: cfg.Dev, MachineID: cfg.MachineID}); err != nil {
		if remote.IsConflict(err) {
			return fmt.Errorf("%w; pick another: julienning share %s --nick NAME", nicknameTaken(nick, takenBy(ctx, env, cfg, client, nick, email), err), email)
		}
		return remoteErr("share "+email, err)
	}
	if cfg.Declined(email) {
		cfg.Undecline(email)
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	// The Worker keeps an existing record as it is (sharing is idempotent),
	// so the fresh listing says which nickname the account really has. A
	// record from before nicknames has none: name it explicitly. A missing
	// email means the listing lags this write, so ours stands.
	fresh := refreshCache(ctx, env, cfg, client)
	final, already := nick, false
	var nameErr error
	if fresh != nil && fresh.Contains(email) {
		switch cur := fresh.Nickname(email); {
		case cur == "":
			err := client.SetNickname(ctx, email, nick)
			switch {
			case remote.IsConflict(err):
				nameErr = nicknameTaken(nick, holderOf(fresh, nick, email), err)
			case err != nil:
				nameErr = remoteErr("set its nickname", err)
			}
			if nameErr != nil {
				final = ""
			}
		case cur != nick:
			final, already = cur, true
		}
	}
	if _, err := sharedcache.AddWithNickname(email, final); err != nil {
		warnf(env, "could not update the allowlist cache: %v", err)
	}
	switch {
	case nameErr != nil:
		fmt.Fprintf(env.Stdout, "Shared %s with the team.\n", email)
	case already:
		fmt.Fprintf(env.Stdout, "%s was already shared as %s (rename with: julienning nick %s NEW).\n", email, final, final)
	default:
		fmt.Fprintf(env.Stdout, "Shared %s with the team as %s.\n", email, final)
	}
	if names := localNames(cfg, email); len(names) > 0 {
		fmt.Fprintf(env.Stdout, "Logged in here as: %s.\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintln(env.Stdout, "No registered dir here is logged in as it; `julienning setup` registers one once it is.")
	}
	if nameErr != nil {
		return fmt.Errorf("%s has no nickname yet: %w; name it with: julienning nick %s NAME", email, nameErr, email)
	}
	return nil
}

func runUnshare(env cli.Env) error {
	fs := cli.NewFlagSet("unshare", env)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	args, err := parseFlagsPermute(fs, env.Args)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return cli.Usagef("expected exactly one nickname, email or config name")
	}
	if strings.Contains(args[0], "@") {
		if _, err := normalizeEmail(args[0]); err != nil {
			return err
		}
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
	email, nick, err := accountByTarget(ctx, env, cfg, client, args[0])
	if err != nil {
		return err
	}
	label := accountLabel(nick, email)
	if !*yes {
		if !isInteractive(env) {
			return errors.New("refusing to unshare without confirmation: run it in a terminal, or add --yes")
		}
		q := fmt.Sprintf("Unshare %s? This deletes its usage and claims for the whole team. [y/N] ", label)
		if !confirm(env.Stdout, bufio.NewReader(env.Stdin), q) {
			fmt.Fprintln(env.Stdout, "Cancelled; nothing was changed.")
			return nil
		}
	}
	if err := client.Unshare(ctx, email); err != nil {
		return remoteErr("unshare "+email, err)
	}
	fmt.Fprintf(env.Stdout, "Unshared %s; its usage and claims were removed from the Worker.\n", label)
	refreshCache(ctx, env, cfg, client)
	if _, err := sharedcache.Remove(email); err != nil {
		warnf(env, "could not update the allowlist cache: %v", err)
	}
	return nil
}

// refreshCache re-fetches shared.json after a change; nil (with a warning)
// when the Worker cannot list. Callers then apply their own change on top:
// KV listings lag writes by up to a minute, so a fresh listing may still
// show the old state. sharedcache.AddWithNickname/Remove keep fetched_at,
// so a failed refresh does not make the cache look fresh.
func refreshCache(ctx context.Context, env cli.Env, cfg *config.Config, client remote.Client) *sharedcache.Cache {
	cache, _, err := sharedcache.Refresh(ctx, client, cfg.Dev, now())
	if err != nil {
		warnf(env, "could not refresh the allowlist (%v); updating the cached copy", remoteErr("list accounts", err))
		return nil
	}
	return cache
}

// localNames lists registered dirs currently logged in as email.
func localNames(cfg *config.Config, email string) []string {
	var out []string
	for _, cd := range cfg.Configs {
		if e, ok, _ := dirEmail(cd.Dir); ok && e == email {
			out = append(out, cd.Name)
		}
	}
	return out
}
