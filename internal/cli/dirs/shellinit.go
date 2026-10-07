package dirs

import (
	"errors"
	"fmt"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/shell"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "shell-init",
		Summary: "print the claude wrapper and claude-<nickname> functions to eval from your rc file",
		Usage:   "shell-init zsh|bash",
		Run:     runShellInit,
	})
}

// runShellInit emits one claude-<nick> function per team nickname in the
// cached allowlist, logged in here or not: the function resolves the dir
// when called, and says how to sign in when no local dir has the account.
// It never touches the network; the cache is refreshed elsewhere.
func runShellInit(env cli.Env) error {
	if len(env.Args) != 1 {
		return cli.Usagef("expected exactly one shell name (zsh or bash)")
	}
	name := env.Args[0]
	if !shell.Supported(name) {
		return cli.Usagef("unsupported shell %q (want zsh or bash)", name)
	}
	_, err := config.Load()
	if errors.Is(err, config.ErrNotSetup) {
		// The rc line evals this output on every shell start: a machine that
		// has not run setup must get empty output and exit 0, never an error.
		return nil
	}
	if err != nil {
		return err
	}
	defaultDir, err := claudecfg.DefaultDir()
	if err != nil {
		return err
	}
	// Nicknames come from the Worker and become function names. One bad
	// record must not break every shell start, so it is skipped and named.
	var nicks []string
	for _, en := range loadCache(env).Entries() {
		switch {
		case en.Nickname == "":
		case !shell.ValidNickname(en.Nickname):
			warnf(env, "skipping invalid nickname %q of %s from the allowlist cache (rename it: julienning nick %s NAME)", en.Nickname, en.Email, en.Email)
		default:
			nicks = append(nicks, en.Nickname)
		}
	}
	code, err := shell.Init(name, nicks, defaultDir)
	if err != nil {
		return err
	}
	fmt.Fprint(env.Stdout, code)
	return nil
}
