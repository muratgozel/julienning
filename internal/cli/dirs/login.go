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

// Seams: all three run claude, and tests never run the real claude.
var (
	// authLogin runs `claude auth login` against dir in this terminal,
	// pre-filling email on the login page when it is not "".
	authLogin = func(env cli.Env, dir, email string) error {
		args := []string{"auth", "login"}
		if email != "" {
			args = append(args, "--email", email)
		}
		return runAuth(env, dir, args)
	}
	// authLogout runs `claude auth logout` against dir: only ever a dir
	// login or new-config chose for this sign-in moments ago (created, or
	// never used) that the browser signed into the wrong account.
	authLogout = func(env cli.Env, dir string) error {
		return runAuth(env, dir, []string{"auth", "logout"})
	}
	// checkSignedIn asks Claude Code whether dir still has a working login.
	checkSignedIn = usageprobe.SignedIn
)

// runAuth runs `claude <args>` against dir in this terminal.
func runAuth(env cli.Env, dir string, args []string) error {
	path, err := launch.ClaudePath()
	if err != nil {
		return err
	}
	cmd := exec.Command(path, args...)
	// The probe's environment: no inherited Claude Code session
	// variables, no CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY that
	// would sign in something else than the dir's account.
	cmd.Env = usageprobe.Env(os.Environ(), dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
	err = cmd.Run()
	what := "claude " + strings.Join(args, " ")
	if len(args) > 2 {
		what = "claude " + strings.Join(args[:2], " ")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%s did not finish (exit status %d)", what, exit.ExitCode())
	}
	if err != nil {
		return fmt.Errorf("run %s: %w", what, err)
	}
	return nil
}

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
	fresh := false
	switch {
	case errors.As(err, &nl):
		var cd config.ConfigDir
		if cd, fresh, cache, err = dirToSignIn(env, cfg, cache, nl); err != nil {
			return err
		}
		m = resolve.Match{Dir: cd, Email: nl.Email, Nickname: cache.Nickname(nl.Email), Via: resolve.ViaEmail}
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
	return verifyLogin(env, cache, m.Dir, want, before, fresh)
}

// dirToSignIn picks the dir to sign an account into when no registered dir
// is logged into it, in this order:
//
//  1. the first dir by name that held it last and has no login (its
//     sessions are still there). One Claude Code says is still signed in
//     stops login instead: its account file is just stale, and signing in
//     elsewhere would split the account across two dirs;
//  2. for an email the allowlist does not show (refreshed once, in case a
//     teammate shared it since), nothing: new-config decides on sharing;
//  3. a dir julienning does not manage that is logged into it: login stops
//     and says to adopt it, for the same reason as in 1;
//  4. the first unused registered dir (unusedDirs);
//  5. a new dir like new-config's.
//
// A reused dir must be confirmed signed out by Claude Code, so a working
// login is never signed over. fresh is true for 4 and 5, dirs with nothing
// in them yet, so verifyLogin may sign a wrong account out again. cache is
// the allowlist it decided with.
func dirToSignIn(env cli.Env, cfg *config.Config, cache *sharedcache.Cache, nl *resolve.NotLocalError) (_ config.ConfigDir, fresh bool, _ *sharedcache.Cache, _ error) {
	fail := func(err error) (config.ConfigDir, bool, *sharedcache.Cache, error) {
		return config.ConfigDir{}, false, cache, err
	}
	who := accountLabel(nl.Nickname, nl.Email)
	var stale []config.ConfigDir
	for _, cd := range nl.SignedOut {
		in, err := signedIn(cd)
		if err != nil {
			return fail(fmt.Errorf("%v; sign in there yourself with: julienning login %s", err, cd.Name))
		}
		if !in {
			notef(env, "%s held %s last and has no login now; signing in there again (its sessions are still there)", shortenHome(cd.Dir), who)
			return cd, false, cache, nil
		}
		stale = append(stale, cd)
	}
	if len(stale) > 0 {
		return fail(fmt.Errorf("%s held %s last and Claude Code says it is still signed in there, though its account file names nobody; "+
			"start claude there once so it records the account: julienning use %s", shortenHome(stale[0].Dir), who, stale[0].Name))
	}
	if resolve.IsPersonal(cfg, cache, nl.Email) {
		fresh, err := refreshAllowlist(cfg)
		if err != nil || resolve.IsPersonal(cfg, fresh, nl.Email) {
			msg := fmt.Sprintf("%s is not shared with the team and not logged in on this machine; "+
				"sign it into a new dir with `julienning new-config` (shares it; `--no-share` keeps it personal), or into an existing one with `julienning login CONFIG` (see `julienning configs`)", nl.Email)
			if err != nil {
				msg += fmt.Sprintf(" (the team allowlist could not be refreshed: %v)", remoteErr("list accounts", err))
			}
			return fail(errors.New(msg))
		}
		cache = fresh
		who = accountLabel(cache.Nickname(nl.Email), nl.Email)
	}
	held, err := resolve.Unregistered(cfg, nl.Email)
	if err != nil {
		return fail(err)
	}
	if len(held) > 0 {
		return fail(fmt.Errorf("%s is signed in on this machine in %s, which julienning does not manage yet; register it with: julienning adopt %s",
			who, shortenHome(held[0]), shortenHome(held[0])))
	}
	for _, cd := range unusedDirs(cfg) {
		in, err := signedIn(cd)
		if err != nil {
			warnf(env, "%v; not signing in there", err)
			continue
		}
		if in {
			continue
		}
		// The empty mark of an aborted new-config would share whatever
		// account the browser signs in; this dir is for a team account now.
		if cd.ShareOnLogin != nil {
			if _, err := config.UpdateDir(cd.Dir, func(c *config.ConfigDir) { c.ShareOnLogin = nil }); err != nil {
				return fail(fmt.Errorf("clear the pending share of %s: %w", cd.Name, err))
			}
			cd.ShareOnLogin = nil
		}
		notef(env, "nobody has used %s yet; signing %s in there", shortenHome(cd.Dir), who)
		return cd, true, cache, nil
	}
	home, err := homeDir()
	if err != nil {
		return fail(err)
	}
	name, dir, err := newDirName(cfg, home, "")
	if err != nil {
		return fail(err)
	}
	cd, err := createDir(env, name, dir, nil, nil)
	if err != nil {
		return fail(err)
	}
	notef(env, "no dir here holds %s; created %s (config %q) for it", who, shortenHome(cd.Dir), cd.Name)
	return cd, true, cache, nil
}

// refreshAllowlist fetches the team allowlist into shared.json. Without a
// Worker there is nothing to refresh and the cached allowlist stands.
func refreshAllowlist(cfg *config.Config) (*sharedcache.Cache, error) {
	if err := cfg.RequireRemote(); err != nil {
		return sharedcache.Load()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*remoteTimeout)
	defer cancel()
	fresh, _, err := sharedcache.Refresh(ctx, newClient(cfg), cfg.Dev, now())
	return fresh, err
}

// unusedDirs are the registered dirs, by name, that exist, were never
// signed in and hold no sessions: one new-config or login created and whose
// sign-in did not finish, or `new-config --no-login`. A dir marked to share
// its first login under a nickname is left alone; it was made for a new
// account. Another account's sessions are never put under this one: a dir
// with any session, or one whose sessions cannot be counted, is not unused.
func unusedDirs(cfg *config.Config) []config.ConfigDir {
	var out []config.ConfigDir
	for _, cd := range cfg.Configs {
		if cd.LastEmail != "" || (cd.ShareOnLogin != nil && cd.ShareOnLogin.Nickname != "") {
			continue
		}
		// A registered dir deleted by hand would be recreated by the sign-in
		// without julienning's settings.json: no hooks, no status line.
		if fi, err := os.Stat(cd.Dir); err != nil || !fi.IsDir() {
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

// signedIn asks Claude Code whether anybody is signed into cd. The error
// names the dir.
func signedIn(cd config.ConfigDir) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), usageprobe.Timeout)
	defer cancel()
	in, err := checkSignedIn(ctx, cd.Dir)
	if err != nil {
		return false, fmt.Errorf("cannot tell whether %s is still signed in (%v)", shortenHome(cd.Dir), err)
	}
	return in, nil
}

// verifyLogin reads the dir's login after `claude auth login` and says what
// it got. Signing in as another account than asked for is an error: the
// browser was signed into that one. In a fresh dir (one login or new-config
// chose for this sign-in, with nothing in it yet) that account is signed out
// again, so the dir stays unused and the retry signs in there; otherwise the
// login is recorded (config.RecordLogins) and a usage probe's signed-out
// mark is dropped.
func verifyLogin(env cli.Env, cache *sharedcache.Cache, cd config.ConfigDir, want, before string, fresh bool) error {
	shown := shortenHome(cd.Dir)
	got, err := claudecfg.ReadEmail(cd.Dir)
	if errors.Is(err, claudecfg.ErrNotLoggedIn) {
		return fmt.Errorf("%s still has no login after claude auth login; start claude there and sign in with /login", shown)
	}
	if err != nil {
		return err
	}
	label := func(email string) string { return accountLabel(cache.Nickname(email), email) }
	name := func(email string) string { return nickOrEmail(cache.Nickname(email), email) }
	wrong := want != "" && got != strings.ToLower(want)
	if wrong && fresh {
		err := authLogout(env, cd.Dir)
		if err == nil {
			return fmt.Errorf("%s was signed in as %s, not %s: the browser was signed into %s, so julienning signed it out of %s again. "+
				"Sign out of claude.ai there (or use a private window) and run: julienning login %s",
				shown, label(got), label(want), name(got), shown, name(want))
		}
		warnf(env, "could not sign %s out of %s again: %v", label(got), shown, err)
	}
	if _, err := config.RecordLogins(map[string]string{cd.Dir: got}); err != nil {
		warnf(env, "could not record the login of %s (%v)", cd.Name, err)
	}
	if err := usageprobe.ClearSignedOut(cd.Dir); err != nil {
		warnf(env, "%v", err)
	}
	if before != "" && before != got {
		notef(env, "%s was logged into %s; the sessions in it stay there, under %s", shown, label(before), name(got))
	}
	if wrong {
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
