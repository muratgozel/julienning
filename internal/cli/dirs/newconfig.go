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
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// dirPrefix is the prefix of every non-default Claude config dir julienning
// creates ("~/.claude-foo").
const dirPrefix = ".claude-"

// maxAutoNumber bounds new-config's search for a free <prefix><N>.
const maxAutoNumber = 999

func init() {
	cli.Register(&cli.Command{
		Name:    "new-config",
		Summary: "create and register a config dir for a team account and start claude in it to sign in; the account is shared automatically",
		Usage:   "new-config [--name NAME] [--copy-settings-from NAME] [--nick NICK] [--no-share] [--no-login]",
		Run:     runNewConfig,
	})
}

func runNewConfig(env cli.Env) error {
	fs := cli.NewFlagSet("new-config", env)
	nameFlag := fs.String("name", "", "config name or dir basename (e.g. foo or .claude-foo; default: <prefix><N> in ~/.claude-<prefix><N>)")
	copyFrom := fs.String("copy-settings-from", "", "registered config whose settings.json to copy")
	nickFlag := fs.String("nick", "", "team nickname for the account you sign in with (default: ask in a terminal, else the email's local part)")
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
	name, dir, err := resolveNewName(cfg, home, *nameFlag)
	if err != nil {
		return err
	}
	if _, taken := cfg.Find(name); taken {
		return fmt.Errorf("config %q already registered (pick another --name)", name)
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
	var share *config.ShareOnLogin
	if !*noShare {
		cache := loadCache(env)
		switch {
		case nick != "":
			if holder, taken := cache.ByNickname(nick); taken {
				return fmt.Errorf("nickname %q is already used by %s; pick another --nick, or leave it out to use the email's local part", nick, holder)
			}
		case isInteractive(env):
			nick = askNewNickname(env, cache)
		}
		share = &config.ShareOnLogin{Nickname: nick}
	}
	exe, err := patchCommand(env)
	if err != nil {
		return err
	}

	// os.Mkdir, not MkdirAll: MkdirAll succeeds on an existing dir, so two
	// concurrent runs would both "create" the same config and the second Save
	// would drop the first registration. EEXIST is the exclusion.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists (run `julienning adopt %s` to register it)", shortenHome(dir), shortenHome(dir))
		}
		return fmt.Errorf("create %s: %w", dir, err)
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
			return err
		}
	}
	if _, err := claudecfg.Patch(dir, exe); err != nil {
		return err
	}
	// Re-read config.json right before registering: Save rewrites the whole
	// file, so a concurrent run that registered between our Load and here would
	// otherwise be lost.
	if cfg, err = loadConfig(); err != nil {
		return err
	}
	cd := config.ConfigDir{Name: name, Dir: dir, ShareOnLogin: share}
	if err := cfg.Add(cd); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	registered = true

	fmt.Fprintf(env.Stdout, "Created %s (config %q).\n", shortenHome(dir), name)
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

// askNewNickname asks for the team nickname of the account the new dir will
// be signed into. The email is not known yet, so Enter (or the end of
// input) returns "", which the share on login turns into the email's local
// part. An invalid answer, or one the cached allowlist says another account
// holds, is explained and asked again: the share itself happens later in
// the background, where a taken nickname can no longer be asked about.
func askNewNickname(env cli.Env, cache *sharedcache.Cache) string {
	in := bufio.NewReader(env.Stdin)
	for {
		answer, ok := ask(env.Stdout, in, "Nickname for this account [the email's name]: ")
		if !ok || answer == "" {
			return ""
		}
		n, err := normalizeNickname(answer)
		if err != nil {
			fmt.Fprintf(env.Stdout, "%v\n", err)
			continue
		}
		if holder, taken := cache.ByNickname(n); taken {
			fmt.Fprintf(env.Stdout, "nickname %q is already used by %s; choose another (or press Enter for the email's name)\n", n, holder)
			continue
		}
		return n
	}
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
	return "", "", fmt.Errorf("no free %s<N> name up to %d; pass --name", prefix, maxAutoNumber)
}
