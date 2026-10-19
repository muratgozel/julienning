package dirs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/tui"
)

// dirPrefix is the prefix of every non-default Claude config dir julienning
// creates ("~/.claude-foo").
const dirPrefix = ".claude-"

// maxAutoNumber bounds new-config's search for a free <prefix><N>.
const maxAutoNumber = 999

func init() {
	cli.Register(&cli.Command{
		Name:    "new-config",
		Host:    cli.HostForward,
		Summary: "create and register a config dir for a team account and start claude in it to sign in; the account is shared automatically",
		Usage:   "new-config [--name NAME] [--copy-settings-from NAME] [--nick NICK] [--no-share] [--no-login]",
		Run:     runNewConfig,
	})
}

func runNewConfig(env cli.Env) error {
	fs := cli.NewFlagSet("new-config", env)
	nameFlag := fs.String("name", "", "config name or dir basename (e.g. foo or .claude-foo; default: <prefix><N> in ~/.claude-<prefix><N>)")
	copyFrom := fs.String("copy-settings-from", "", "registered config whose settings.json to copy")
	nickFlag := fs.String("nick", "", "team nickname for the account you sign in with, or a team account's nickname to sign that account in (default: ask in a terminal, else the email's local part)")
	noShare := fs.Bool("no-share", false, "keep the account personal: do not share it with the team on first login")
	noLogin := fs.Bool("no-login", false, "only create and register the dir (scripts); print the sign-in command instead of starting claude")
	// Signing in used to be opt-in; --login stays accepted (and does
	// nothing) so older docs, hints and scripts keep working.
	_ = fs.Bool("login", false, "accepted for compatibility; starting claude is the default")
	if err := parseFlags(fs, env.Args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cli.Usagef("unexpected argument %q (use --name)", fs.Arg(0))
	}
	nick := ""
	if *nickFlag != "" {
		if *noShare {
			return cli.Usagef("--nick names the account for sharing; it cannot be combined with --no-share")
		}
		n, err := normalizeNickname(*nickFlag)
		if err != nil {
			return cli.Usagef("%v", err)
		}
		nick = n
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	home, err := homeDir()
	if err != nil {
		return err
	}
	name, dir, err := newDirName(cfg, home, *nameFlag)
	if err != nil {
		return err
	}

	// The copy leaves out julienning's own statusLine and hooks (restoring the
	// statusLine they replaced): Patch then treats it like any user file, and
	// forget/uninstall remove exactly what it added, with no empty "hooks"
	// arrays left behind.
	var settings []byte
	if *copyFrom != "" {
		src, ok := cfg.Find(*copyFrom)
		if !ok {
			return unknownConfig(cfg, *copyFrom)
		}
		if settings, err = claudecfg.SettingsWithoutJulienning(src.Dir); err != nil {
			return fmt.Errorf("copy settings from %q: %w", *copyFrom, err)
		}
	}
	// A nickname a team account already holds names that account: the user
	// is signing it in on this machine (typically a second one), so there is
	// nothing to share and the new dir gets no ShareOnLogin mark. Without the
	// mark a wrong account signed in by mistake stays personal.
	var share *config.ShareOnLogin
	account := ""
	var cache *sharedcache.Cache
	if !*noShare {
		cache = loadCache(env)
		switch {
		case nick != "":
			if account, _ = cache.ByNickname(nick); account != "" {
				here, err := accountHere(cfg, cache, account)
				if err != nil {
					return err
				}
				if here != "" {
					return errors.New(here)
				}
			}
		case isInteractive(env):
			if nick, account, err = askNewNickname(env, cfg, cache); err != nil {
				return err
			}
		}
		if account == "" {
			share = &config.ShareOnLogin{Nickname: nick}
		}
	}

	cd, err := createDir(env, name, dir, settings, share)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Created %s (config %q).\n", shortenHome(cd.Dir), cd.Name)
	if account != "" {
		return signInNewDir(env, cache, cd, account, *noLogin)
	}
	if *noLogin {
		fmt.Fprintf(env.Stdout, "Sign in: julienning login %s\n", name)
	} else {
		fmt.Fprintln(env.Stdout, "Starting claude in it so you can sign in.")
	}
	if share == nil {
		fmt.Fprintln(env.Stdout, "This account stays personal (not shared).")
	} else {
		as := share.Nickname
		if as == "" {
			as = "its email name"
		}
		fmt.Fprintf(env.Stdout, "The account you sign in with will be shared with the team as %s; julienning does that automatically when your first session starts.\n", as)
	}
	if *noLogin {
		return nil
	}
	// Exec replaces this process on success. On failure (claude missing)
	// the dir stays registered, so the error says how to sign in later.
	if err := launch.Exec(dir, nil, ""); err != nil {
		return fmt.Errorf("%w; %s is created, sign in later with: julienning login %s", err, shortenHome(dir), name)
	}
	return nil
}

// signInNewDir signs the team account email into cd, the dir new-config just
// created for it, the way login does: `claude auth login` pre-filled with the
// email, then the check of what it got. A failed or aborted sign-in leaves
// the dir registered and unused, and `login` reuses such a dir, so the hint
// names the account rather than the config.
func signInNewDir(env cli.Env, cache *sharedcache.Cache, cd config.ConfigDir, email string, noLogin bool) error {
	nick := cache.Nickname(email)
	again := nickOrEmail(nick, email)
	if noLogin {
		fmt.Fprintf(env.Stdout, "Sign in: julienning login %s\n", again)
		return nil
	}
	fmt.Fprintf(env.Stdout, "%s is already shared with the team; signing it in here.\n", accountLabel(nick, email))
	if err := authLogin(env, cd.Dir, email); err != nil {
		return fmt.Errorf("%w; %s is created, sign in later with: julienning login %s", err, shortenHome(cd.Dir), again)
	}
	return verifyLogin(env, cache, cd, email, "", true)
}

// accountHere says why a team account gets no new dir here, "" when it may:
// a registered dir holds it (ready to use), a dir julienning does not manage
// holds it (adopt that one), or a registered dir held it last and lost it to
// a sign-out (login signs in there again, sessions and all). A second dir
// would split the account's sessions.
func accountHere(cfg *config.Config, cache *sharedcache.Cache, email string) (string, error) {
	nick := cache.Nickname(email)
	who := nickOrEmail(nick, email)
	if dirs := resolve.Dirs(cfg, email, nil); len(dirs) > 0 {
		return fmt.Sprintf("%s is already signed in on this machine, in %s; use it with: julienning use %s",
			accountLabel(nick, email), shortenHome(dirs[0].Dir), who), nil
	}
	held, err := resolve.Unregistered(cfg, email)
	if err != nil {
		return "", err
	}
	if len(held) > 0 {
		return fmt.Sprintf("%s is already signed in on this machine, in %s, which julienning does not manage yet; register it with: julienning adopt %s",
			accountLabel(nick, email), shortenHome(held[0]), shortenHome(held[0])), nil
	}
	if out := resolve.SignedOutDirs(cfg, email); len(out) > 0 {
		return (&resolve.NotLocalError{Nickname: nick, Email: email, Shared: true, SignedOut: out}).Error(), nil
	}
	return "", nil
}

// createDir creates, patches and registers a new config dir (see
// newDirName for name and dir). It refuses an existing dir and undoes its
// work on failure, so a retry with the same name works.
func createDir(env cli.Env, name, dir string, settings []byte, share *config.ShareOnLogin) (config.ConfigDir, error) {
	exe, err := patchCommand(env)
	if err != nil {
		return config.ConfigDir{}, err
	}
	// os.Mkdir, not MkdirAll: MkdirAll succeeds on an existing dir, so two
	// concurrent runs would both "create" the same config and the second Save
	// would drop the first registration. EEXIST is the exclusion.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return config.ConfigDir{}, fmt.Errorf("%s already exists (run `julienning adopt %s` to register it)", shortenHome(dir), shortenHome(dir))
		}
		return config.ConfigDir{}, fmt.Errorf("create %s: %w", dir, err)
	}
	// Until registration succeeds the dir is ours alone; undo on failure so a
	// retry with the same name works.
	registered := false
	defer func() {
		if !registered {
			_, _ = claudecfg.Unpatch(dir)
			_ = os.Remove(claudecfg.SettingsPath(dir))
			_ = os.Remove(dir)
		}
	}()
	if settings != nil {
		if err := claudecfg.WriteSettings(dir, settings); err != nil {
			return config.ConfigDir{}, err
		}
	}
	if _, err := claudecfg.Patch(dir, exe); err != nil {
		return config.ConfigDir{}, err
	}
	// Re-read config.json right before registering: Save rewrites the whole
	// file, so a concurrent run that registered between our Load and here would
	// otherwise be lost.
	cfg, err := loadConfig()
	if err != nil {
		return config.ConfigDir{}, err
	}
	cd := config.ConfigDir{Name: name, Dir: dir, ShareOnLogin: share}
	if err := cfg.Add(cd); err != nil {
		return config.ConfigDir{}, err
	}
	if err := cfg.Save(); err != nil {
		return config.ConfigDir{}, err
	}
	registered = true
	return cd, nil
}

// askNewNickname asks for the team nickname of the account the new dir will
// be signed into. The email is not known yet, so Enter (or the end of
// input) returns "", which the share on login turns into the email's local
// part. An invalid answer is explained and asked again: the share itself
// happens later in the background, where it can no longer be asked about.
// A nickname the cached allowlist shows a team account holding may name
// that account (returned as account, nick ""): a confirmation, because
// meaning a new nickname and hitting a teammate's would sign in theirs.
func askNewNickname(env cli.Env, cfg *config.Config, cache *sharedcache.Cache) (nick, account string, err error) {
	in := bufio.NewReader(env.Stdin)
	const again = "choose another nickname (or press Enter for the email's name)"
	for {
		answer, ok := ask(env.Stdout, in, "Nickname for this account [the email's name]: ")
		if !ok || answer == "" {
			return "", "", nil
		}
		n, err := normalizeNickname(answer)
		if err != nil {
			fmt.Fprintf(env.Stdout, "%v\n", err)
			continue
		}
		holder, taken := cache.ByNickname(n)
		if !taken {
			return n, "", nil
		}
		yes, err := tui.AskYesNo(in, env.Stdout, fmt.Sprintf("%s is the team account %s. Sign it in here? [Y/n] ", n, holder))
		if err != nil {
			return "", "", err
		}
		if !yes {
			fmt.Fprintf(env.Stdout, "Then %s.\n", again)
			continue
		}
		here, err := accountHere(cfg, cache, holder)
		if err != nil {
			return "", "", err
		}
		if here == "" {
			return "", holder, nil
		}
		fmt.Fprintf(env.Stdout, "%s\nOr %s.\n", here, again)
	}
}

// newDirName is resolveNewName that also refuses a config name that is
// registered already.
func newDirName(cfg *config.Config, home, flagValue string) (string, string, error) {
	name, dir, err := resolveNewName(cfg, home, flagValue)
	if err != nil {
		return "", "", err
	}
	if _, taken := cfg.Find(name); taken {
		return "", "", fmt.Errorf("config %q already registered (pick another --name)", name)
	}
	return name, dir, nil
}

// resolveNewName turns --name into a (name, dir) pair, or picks the smallest
// free ~/.claude-<prefix><N>, named <prefix><N> (dir and name share N). Both
// the directory and the registry must be free for a number to count as
// available.
func resolveNewName(cfg *config.Config, home, flagValue string) (string, string, error) {
	if flagValue != "" {
		name := strings.TrimPrefix(strings.TrimSpace(flagValue), dirPrefix)
		if !config.ValidConfigName(name) {
			return "", "", invalidName(flagValue)
		}
		return name, filepath.Join(home, dirPrefix+name), nil
	}
	prefix := cfg.Prefix()
	for n := 1; n <= maxAutoNumber; n++ {
		name := prefix + strconv.Itoa(n)
		dir := filepath.Join(home, dirPrefix+name)
		if _, taken := cfg.Find(name); taken {
			continue
		}
		if _, err := os.Lstat(dir); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("check %s: %w", dir, err)
		}
		return name, dir, nil
	}
	return "", "", fmt.Errorf("no free %s<N> name up to %d (name one with: julienning new-config --name NAME)", prefix, maxAutoNumber)
}
