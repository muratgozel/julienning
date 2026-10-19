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
	"github.com/muratgozel/julienning/internal/sessions"
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
		// The probe's environment: no inherited Claude Code session
		// variables, no CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY that
		// would sign in something else than the dir's account.
		cmd.Env = usageprobe.Env(os.Environ(), dir)
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
// when none is, to the dir dirToSignIn picks (one that held it last, an
// unused one, or a new one). A config name is that dir (a fresh dir nobody
// is signed into has only its config name). With claude args after `--`,
// plain `claude` starts with them instead, as before, unchecked.
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
	m, err := resolveTargetWith(cfg, cache, target)
	var nl *resolve.NotLocalError
	switch {
	case errors.As(err, &nl):
		cd, err := dirToSignIn(env, cfg, cache, nl)
		if err != nil {
			return err
		}
		m = resolve.Match{Dir: cd, Email: nl.Email, Nickname: nl.Nickname, Via: resolve.ViaEmail}
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

// dirToSignIn picks the dir to sign an account into when no registered dir
// is logged into it: the first (by name) that held it last, else, for a
// team account, the first that was never used (unusedDirs), else a new dir
// like new-config's. A dir with a working login is never signed over:
// every reused dir has no login in its account file, and Claude Code itself
// must say nobody is signed in there. An email that is not a team account
// gets no unused or new dir; new-config asks whether to share it.
func dirToSignIn(env cli.Env, cfg *config.Config, cache *sharedcache.Cache, nl *resolve.NotLocalError) (config.ConfigDir, error) {
	who := accountLabel(nl.Nickname, nl.Email)
	cd, ok, err := firstSignedOut(nl.SignedOut)
	if err != nil {
		return config.ConfigDir{}, err
	}
	if ok {
		notef(env, "%s held %s last and has no login now; signing in there again (its sessions are still there)", shortenHome(cd.Dir), who)
		return cd, nil
	}
	if resolve.IsPersonal(cfg, cache, nl.Email) {
		return config.ConfigDir{}, fmt.Errorf("%s is not shared with the team and not logged in on this machine; "+
			"sign it into a new dir with `julienning new-config` (shares it; `--no-share` keeps it personal), or into an existing one with `julienning login CONFIG` (see `julienning configs`)",
			nl.Email)
	}
	if cd, ok, err = firstSignedOut(unusedDirs(cfg)); err != nil {
		return config.ConfigDir{}, err
	} else if ok {
		notef(env, "nobody has used %s yet; signing %s in there", shortenHome(cd.Dir), who)
		return cd, nil
	}
	home, err := homeDir()
	if err != nil {
		return config.ConfigDir{}, err
	}
	name, dir, err := newDirName(cfg, home, "")
	if err != nil {
		return config.ConfigDir{}, err
	}
	if cd, err = createDir(env, name, dir, nil, nil); err != nil {
		return config.ConfigDir{}, err
	}
	notef(env, "no dir here holds %s; created %s (config %q) for it", who, shortenHome(cd.Dir), cd.Name)
	return cd, nil
}

// unusedDirs are the registered dirs, by name, that were never signed in
// and hold no sessions: one new-config or login created and whose sign-in
// was aborted, or `new-config --no-login`. A dir marked to share its first
// login under a nickname is left alone; it was made for a new account.
// Another account's sessions are never put under this one: a dir with any
// session, or one that cannot be counted, is not unused.
func unusedDirs(cfg *config.Config) []config.ConfigDir {
	var out []config.ConfigDir
	for _, cd := range cfg.Configs {
		if cd.LastEmail != "" || (cd.ShareOnLogin != nil && cd.ShareOnLogin.Nickname != "") {
			continue
		}
		if _, err := claudecfg.ReadEmail(cd.Dir); !errors.Is(err, claudecfg.ErrNotLoggedIn) {
			continue
		}
		if n, err := sessions.Count([]config.ConfigDir{cd}); err != nil || n > 0 {
			continue
		}
		out = append(out, cd)
	}
	return out
}

// firstSignedOut returns the first of dirs that Claude Code says nobody is
// signed into. Failing to ask is an error: guessing could sign over a login.
func firstSignedOut(dirs []config.ConfigDir) (config.ConfigDir, bool, error) {
	for _, cd := range dirs {
		ctx, cancel := context.WithTimeout(context.Background(), usageprobe.Timeout)
		in, err := checkSignedIn(ctx, cd.Dir)
		cancel()
		if err != nil {
			return config.ConfigDir{}, false, fmt.Errorf("cannot tell whether %s is still signed in (%v); sign in there yourself with: julienning login %s", shortenHome(cd.Dir), err, cd.Name)
		}
		if !in {
			return cd, true, nil
		}
	}
	return config.ConfigDir{}, false, nil
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
