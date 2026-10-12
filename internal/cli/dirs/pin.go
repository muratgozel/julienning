package dirs

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/pins"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "pin",
		Summary: "make plain claude and use open an account in this directory and below (TARGET: nickname, email or config name; none: show; --list: all pins)",
		Usage:   "pin [TARGET] | pin --list",
		Run:     runPin,
	})
	cli.Register(&cli.Command{
		Name:    "unpin",
		Summary: "remove the pin on this directory, or on DIR",
		Usage:   "unpin [DIR]",
		Run:     runUnpin,
	})
}

func runPin(env cli.Env) error {
	fs := flag.NewFlagSet("pin", flag.ContinueOnError)
	list := fs.Bool("list", false, "list every pin on this machine")
	pos, err := parseFlagsPermute(fs, env.Args)
	if err != nil {
		return err
	}
	switch {
	case *list && len(pos) > 0:
		return cli.Usagef("pin --list takes no target")
	case len(pos) > 1:
		return cli.Usagef("pin takes at most one target: a team nickname, an email or a config name")
	case len(pos) == 1 && strings.TrimSpace(pos[0]) == "":
		return cli.Usagef("pin needs a non-empty target: a team nickname, an email or a config name")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Local only, like resolve-dir: the cached allowlist decides whether an
	// email is a team account, and nothing is sent anywhere.
	cache := loadCache(env)
	switch {
	case *list:
		return listPins(env, cfg, cache)
	case len(pos) == 0:
		return showPin(env, cfg, cache)
	}

	cwd, err := workingDir()
	if err != nil {
		return err
	}
	email, err := pinTarget(env, cfg, cache, pos[0])
	if err != nil {
		return err
	}
	prev, replaced, created, err := pins.Set(cwd, email)
	if err != nil {
		return err
	}
	here, account := shortenHome(cwd), pinAccount(cfg, cache, email)
	switch {
	case replaced && prev.Email == email:
		fmt.Fprintf(env.Stdout, "%s is already pinned to %s.\n", here, account)
		return nil
	case replaced:
		fmt.Fprintf(env.Stdout, "Pinned %s to %s, replacing %s.\n", here, account, pinAccount(cfg, cache, prev.Email))
	default:
		fmt.Fprintf(env.Stdout, "Pinned %s to %s.\n", here, account)
	}
	fmt.Fprintf(env.Stdout, "Plain claude and julienning use open it in %s and every directory below it; julienning next is unaffected. Remove the pin with: julienning unpin\n", here)
	if created {
		// The claude function is generated when a terminal starts; one from
		// before pins existed never looks at pins.json.
		fmt.Fprintf(env.Stdout, "A terminal opened before this julienning version ignores pins until you run: %s\n", sourceHint(""))
	}
	return nil
}

// pinTarget resolves what the user typed to the account to pin, and checks
// that the account can be opened here now, so a pin never starts out broken.
func pinTarget(env cli.Env, cfg *config.Config, cache *sharedcache.Cache, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if strings.Contains(arg, "@") {
		email := strings.ToLower(arg)
		if !claudecfg.ValidEmail(email) {
			return "", fmt.Errorf("%q is not a valid email address", arg)
		}
		if resolve.IsPersonal(cfg, cache, email) {
			pm, err := resolve.Personal(cfg, email)
			if err != nil {
				return "", err
			}
			for _, n := range pm.Notes() {
				notef(env, "%s", n)
			}
			if err := pm.Remember(); err != nil {
				warnf(env, "%v", err)
			}
			return email, nil
		}
	}
	cur, curOK, err := cfg.Current()
	if err != nil {
		return "", err
	}
	var curPtr *config.ConfigDir
	if curOK {
		curPtr = &cur
	}
	m, err := resolve.Target(cfg, cache, arg, curPtr)
	if err != nil {
		return "", err
	}
	if m.Email == "" {
		return "", fmt.Errorf("%s is not logged in, and a pin names an account; sign in first with: julienning login %s", m.Dir.Name, m.Dir.Name)
	}
	if resolve.IsPersonal(cfg, cache, m.Email) {
		// A config name whose dir holds a personal login: remember that dir,
		// so the pin opens the dir that was named even when the account is
		// also logged in somewhere discovery finds first.
		if err := config.RememberPersonalDir(m.Email, m.Dir.Dir); err != nil {
			warnf(env, "remember %s for %s: %v", shortenHome(m.Dir.Dir), m.Email, err)
		}
	}
	return m.Email, nil
}

func showPin(env cli.Env, cfg *config.Config, cache *sharedcache.Cache) error {
	cwd, err := workingDir()
	if err != nil {
		return err
	}
	list, err := pins.Load()
	if err != nil {
		return err
	}
	p, ok := pins.Match(list, cwd)
	if !ok {
		fmt.Fprintf(env.Stdout, "No pin covers %s; plain claude follows the selection.\n", shortenHome(cwd))
		return nil
	}
	account := pinAccount(cfg, cache, p.Email)
	if pins.Within(p.Dir, cwd) {
		fmt.Fprintf(env.Stdout, "%s is pinned to %s.\n", shortenHome(cwd), account)
	} else {
		fmt.Fprintf(env.Stdout, "%s is inside %s, which is pinned to %s.\n", shortenHome(cwd), shortenHome(p.Dir), account)
	}
	cur, curOK, err := cfg.Current()
	if err != nil {
		return err
	}
	var curPtr *config.ConfigDir
	if curOK {
		curPtr = &cur
	}
	// Read only: showing a pin never rewrites what config.json remembers.
	if _, _, err := resolve.Account(cfg, cache, p.Email, curPtr); err != nil {
		warnf(env, "plain claude refuses to start here until this is fixed: %v", err)
	}
	return nil
}

func listPins(env cli.Env, cfg *config.Config, cache *sharedcache.Cache) error {
	list, err := pins.Load()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(env.Stdout, "No pins. Pin a directory with: julienning pin NICKNAME|EMAIL|CONFIG")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DIR\tACCOUNT")
	for _, p := range list {
		fmt.Fprintf(tw, "%s\t%s\n", shortenHome(p.Dir), pinAccount(cfg, cache, p.Email))
	}
	return tw.Flush()
}

func notef(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: note: %s\n", fmt.Sprintf(format, a...))
}

func runUnpin(env cli.Env) error {
	fs := flag.NewFlagSet("unpin", flag.ContinueOnError)
	if err := parseFlags(fs, env.Args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return cli.Usagef("unpin takes at most one directory")
	}
	var dir string
	if fs.NArg() == 1 {
		if strings.TrimSpace(fs.Arg(0)) == "" {
			return cli.Usagef("unpin needs a non-empty directory")
		}
		// The directory need not exist: a pin can outlive it.
		d, err := expandPath(strings.TrimSpace(fs.Arg(0)))
		if err != nil {
			return err
		}
		dir = d
	} else {
		d, err := workingDir()
		if err != nil {
			return err
		}
		dir = d
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cache := loadCache(env)
	p, ok, err := pins.Remove(dir)
	if err != nil {
		return err
	}
	if ok {
		fmt.Fprintf(env.Stdout, "Unpinned %s (was %s).\n", shortenHome(p.Dir), pinAccount(cfg, cache, p.Email))
		return nil
	}
	list, err := pins.Load()
	if err != nil {
		return err
	}
	if parent, covered := pins.Match(list, dir); covered {
		return fmt.Errorf("%s has no pin of its own; it is inside %s, which is pinned to %s; to remove that pin: julienning unpin %s",
			shortenHome(dir), shortenHome(parent.Dir), pinAccount(cfg, cache, parent.Email), shortenHome(parent.Dir))
	}
	fmt.Fprintf(env.Stdout, "%s is not pinned; nothing to unpin.\n", shortenHome(dir))
	return nil
}

// pinAccount names a pinned account: "alpha (claude1@x.io)" for a team
// account, the bare email without a nickname, "me@x.io (personal)" for a
// personal one (resolve.IsPersonal).
func pinAccount(cfg *config.Config, cache *sharedcache.Cache, email string) string {
	if resolve.IsPersonal(cfg, cache, email) {
		return email + " (personal)"
	}
	if cache == nil {
		return email
	}
	return accountLabel(cache.Nickname(email), email)
}

// workingDir is the directory pins are set and matched for, spelled as the
// shell sees it ($PWD when it names the same directory; os.Getwd).
func workingDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine the current directory: %w", err)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", cwd, err)
	}
	return filepath.Clean(abs), nil
}
