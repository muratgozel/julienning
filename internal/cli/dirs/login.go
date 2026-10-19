package dirs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/usageprobe"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "login",
		Host:    cli.HostForward,
		Summary: "sign a config dir into its account (`claude auth login`), then check which account it got",
		Usage:   "login NICKNAME|EMAIL|CONFIG [-- claude args...]",
		Run:     runLogin,
	})
}

// Seams: both run claude, and tests never run the real claude.
var (
	// authLogin runs `claude auth login` against dir in this terminal,
	// pre-filling email on the login page when it is not "".
	authLogin = func(env cli.Env, dir, email string) error {
		path, err := launch.ClaudePath()
		if err != nil {
			return err
		}
		args := []string{"auth", "login"}
		if email != "" {
			args = append(args, "--email", email)
		}
		cmd := exec.Command(path, args...)
		cmd.Env = launch.Env(os.Environ(), dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
		err = cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("claude auth login did not finish (exit status %d)", exit.ExitCode())
		}
		if err != nil {
			return fmt.Errorf("run claude auth login: %w", err)
		}
		return nil
	}
	// checkSignedIn asks Claude Code whether dir still has a working login.
	checkSignedIn = usageprobe.SignedIn
)

// runLogin signs a dir in with `claude auth login` and then reads back which
// account it got. A nickname or email resolves to the dir logged into it;
// when none is (Claude Code signed it out and cleared the login), to the dir
// that held it last (config.ConfigDir.LastEmail), but only when Claude Code
// confirms nothing is signed in there. A config name is that dir (a fresh dir
// nobody is signed into has only its config name). With claude args after
// `--`, plain `claude` starts with them instead, as before, unchecked.
func runLogin(env cli.Env) error {
	target, extra, err := splitLoginArgs(env.Args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cache := loadCache(env)
	m, err := resolveTarget(env, cfg, target)
	var nl *resolve.NotLocalError
	switch {
	case errors.As(err, &nl):
		cd, err := reusableDir(nl)
		if err != nil {
			return err
		}
		m = resolve.Match{Dir: cd, Email: nl.Email, Nickname: nl.Nickname, Via: resolve.ViaEmail}
		notef(env, "%s held %s last and has no login now; signing in there again (its sessions are still there)",
			shortenHome(cd.Dir), accountLabel(nl.Nickname, nl.Email))
	case err != nil:
		return err
	}
	if len(extra) > 0 {
		// launch.Exec unsets CLAUDE_CONFIG_DIR for the default dir.
		return launch.Exec(m.Dir.Dir, extra, "")
	}
	want := ""
	if m.Via != resolve.ViaDir {
		want = m.Email
	}
	before := m.Dir.LastEmail
	if err := authLogin(env, m.Dir.Dir, want); err != nil {
		return err
	}
	return verifyLogin(env, cache, m.Dir, want, before)
}

// reusableDir picks the dir to sign an account back into when no dir is
// logged into it: the first (by name) that held it last, has no login in its
// account file, and that Claude Code itself says nobody is signed into. A
// dir with a working login is never signed over.
func reusableDir(nl *resolve.NotLocalError) (config.ConfigDir, error) {
	for _, cd := range nl.SignedOut {
		ctx, cancel := context.WithTimeout(context.Background(), usageprobe.Timeout)
		in, err := checkSignedIn(ctx, cd.Dir)
		cancel()
		if err != nil {
			return config.ConfigDir{}, fmt.Errorf("cannot tell whether %s is still signed in (%v); sign in there yourself with: julienning login %s", shortenHome(cd.Dir), err, cd.Name)
		}
		if !in {
			return cd, nil
		}
	}
	return config.ConfigDir{}, fmt.Errorf("%s is not logged in on this machine, and no signed-out dir here held it last; "+
		"sign it into a new dir with `julienning new-config`, or into an existing one with `julienning login CONFIG` (see `julienning configs`)",
		accountLabel(nl.Nickname, nl.Email))
}

// verifyLogin reads the dir's login after `claude auth login` and says what
// it got. Signing in as another account than asked for is an error: the
// browser was signed into that one. Either way the login is recorded
// (config.RecordLogins) and a usage probe's signed-out mark is dropped.
func verifyLogin(env cli.Env, cache *sharedcache.Cache, cd config.ConfigDir, want, before string) error {
	shown := shortenHome(cd.Dir)
	got, err := claudecfg.ReadEmail(cd.Dir)
	if errors.Is(err, claudecfg.ErrNotLoggedIn) {
		return fmt.Errorf("%s still has no login after claude auth login; start claude there and sign in with /login", shown)
	}
	if err != nil {
		return err
	}
	if _, err := config.RecordLogins(map[string]string{cd.Dir: got}); err != nil {
		warnf(env, "could not record the login of %s (%v)", cd.Name, err)
	}
	if err := usageprobe.ClearSignedOut(cd.Dir); err != nil {
		warnf(env, "%v", err)
	}
	label := func(email string) string { return accountLabel(cache.Nickname(email), email) }
	name := func(email string) string {
		if n := cache.Nickname(email); n != "" {
			return n
		}
		return email
	}
	if before != "" && before != got {
		notef(env, "%s was logged into %s; the sessions in it stay there, under %s", shown, label(before), name(got))
	}
	if want != "" && got != strings.ToLower(want) {
		return fmt.Errorf("%s is now signed in as %s, not %s: the browser was signed into %s. "+
			"Sign out of claude.ai there (or use a private window) and run: julienning login %s",
			shown, label(got), label(want), name(got), name(want))
	}
	fmt.Fprintf(env.Stdout, "Signed in: %s in %s.\n", label(got), shown)
	return nil
}

// splitLoginArgs takes the target and the args after "--". Flags are not
// accepted so everything is unambiguously forwarded to claude.
func splitLoginArgs(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, cli.Usagef("missing nickname, email or config name")
	}
	target := args[0]
	rest := args[1:]
	if strings.HasPrefix(target, "-") {
		return "", nil, cli.Usagef("expected a nickname, email or config name, got %q", target)
	}
	if len(rest) == 0 {
		return target, nil, nil
	}
	if rest[0] != "--" {
		return "", nil, cli.Usagef("unexpected argument %q (pass claude args after `--`)", rest[0])
	}
	return target, rest[1:], nil
}
