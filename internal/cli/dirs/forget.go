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
	"github.com/muratgozel/julienning/internal/discover"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "forget",
		Summary: "unregister a config dir by nickname, email or name, undo its settings.json changes, and optionally delete the directory",
		Usage:   "forget TARGET [--delete|--keep]",
		Run:     runForget,
	})
}

// dirChoice is what to do with the forgotten dir's files.
type dirChoice int

const (
	askDir dirChoice = iota
	deleteDir
	keepDir
)

func runForget(env cli.Env) error {
	fs := cli.NewFlagSet("forget", env)
	del := fs.Bool("delete", false, "also delete the config dir and everything in it, without asking")
	keep := fs.Bool("keep", false, "keep the config dir, without asking")
	pos, err := parseFlagsPermute(fs, env.Args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return cli.Usagef("expected exactly one nickname, email or config name")
	}
	if *del && *keep {
		return cli.Usagef("pass --delete or --keep, not both")
	}
	choice := askDir
	switch {
	case *del:
		choice = deleteDir
	case *keep:
		choice = keepDir
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	current, hasCurrent, err := cfg.Current()
	if err != nil {
		return err
	}
	cache := loadCache(env)
	var cur *config.ConfigDir
	if hasCurrent {
		cur = &current
	}
	cd, err := forgetTarget(cfg, cache, pos[0], cur)
	if err != nil {
		return err
	}
	email, loggedIn, emailErr := dirEmail(cd.Dir)

	// A settings.json shared with another registered dir (symlinks) still
	// serves that dir: unpatching it would unwire julienning there too.
	sharedWith := sharesSettingsWith(cfg, cd)

	// Unpatch first: if it fails the dir stays registered, so the user can fix
	// settings.json and retry instead of losing track of julienning's entries.
	var res claudecfg.Result
	if len(sharedWith) == 0 {
		if res, err = claudecfg.Unpatch(cd.Dir); err != nil {
			return fmt.Errorf("%w (fix it and re-run `julienning forget %s`)", err, cd.Name)
		}
	}
	wasCurrent := hasCurrent && current.Name == cd.Name
	cfg.Remove(cd.Name)
	if wasCurrent {
		if err := config.ClearCurrent(); err != nil {
			return err
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	shown := shortenHome(cd.Dir)
	label := resolve.Label(cache, cd, email)
	switch {
	case loggedIn:
		fmt.Fprintf(env.Stdout, "Forgot %s (%s, %s).\n", label, email, shown)
	case emailErr != nil:
		fmt.Fprintf(env.Stdout, "Forgot %s (%s, login unreadable).\n", label, shown)
	default:
		fmt.Fprintf(env.Stdout, "Forgot %s (%s, not logged in).\n", label, shown)
	}
	if len(sharedWith) > 0 {
		fmt.Fprintf(env.Stdout, "settings.json: left as is; it is the same file as the settings.json of %s, which still uses julienning.\n", strings.Join(sharedWith, ", "))
	}
	if len(res.Changes) > 0 {
		fmt.Fprintf(env.Stdout, "settings.json: %s.\n", strings.Join(res.Changes, ", "))
	}
	if wasCurrent {
		fmt.Fprintln(env.Stdout, "It was the current config; nothing is selected now.")
	}
	if loggedIn && cache.Contains(email) {
		who := cache.Nickname(email)
		if who == "" {
			who = email
		}
		fmt.Fprintf(env.Stdout, "%s stays shared with the team; `julienning unshare %s` removes it for everyone.\n", who, who)
	}
	return settleDir(env, cfg, cd.Dir, choice)
}

// forgetTarget resolves arg like `use` does (resolve.Target, nickname then
// email then config name), with two forget-only rules: an account logged
// into several registered dirs is refused, because forget must act on
// exactly the dir the user means; and a config name hidden by a same-named
// nickname still works where the nickname alone cannot pick a dir, so every
// registered dir stays forgettable by its config name.
func forgetTarget(cfg *config.Config, cache *sharedcache.Cache, arg string, cur *config.ConfigDir) (config.ConfigDir, error) {
	m, err := resolve.Target(cfg, cache, arg, cur)
	named, isName := cfg.Find(strings.TrimSpace(arg))
	var nl *resolve.NotLocalError
	switch {
	case errors.As(err, &nl):
		if isName {
			return named, nil
		}
		who := nl.Email
		if nl.Nickname != "" {
			who = accountLabel(nl.Nickname, nl.Email)
		}
		return config.ConfigDir{}, fmt.Errorf("%s is not logged in on this machine; nothing to forget", who)
	case err != nil:
		return config.ConfigDir{}, err
	case m.Via == resolve.ViaDir:
		return m.Dir, nil
	}
	dirs := resolve.Dirs(cfg, m.Email, cur)
	if len(dirs) <= 1 {
		return m.Dir, nil
	}
	listed := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if isName && d.Name == named.Name {
			return named, nil
		}
		listed = append(listed, fmt.Sprintf("%s (%s)", d.Name, shortenHome(d.Dir)))
	}
	return config.ConfigDir{}, fmt.Errorf("%s is logged into more than one config dir here: %s; pass the config name of the one to forget",
		accountLabel(m.Nickname, m.Email), strings.Join(listed, ", "))
}

// settleDir deletes or keeps a just-forgotten dir. Guards run before the
// question so it is never asked about a dir that would not be deleted. A
// guard that overrules an explicit --delete is an error (exit 1), so a
// script never assumes the dir is gone; otherwise the reason is printed.
//
// CRITICAL for future agents: deletion is os.RemoveAll of the registered
// path and nothing else. Never widen it (parent dirs, ~/.claude.json, the
// Keychain): a Claude config dir holds the user's sessions and memory.
func settleDir(env cli.Env, cfg *config.Config, dir string, choice dirChoice) error {
	shown := shortenHome(dir)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.Stdout, "Nothing to delete: %s does not exist.\n", shown)
		return nil
	}
	if choice == keepDir {
		fmt.Fprintf(env.Stdout, "Kept %s.\n", shown)
		return nil
	}
	var reason string
	if err != nil {
		reason = fmt.Sprintf("cannot inspect it (%v)", err)
	} else {
		reason = deletionBlocker(cfg, dir, fi)
	}
	if reason != "" {
		if choice == deleteDir {
			return fmt.Errorf("kept %s: %s", shown, reason)
		}
		fmt.Fprintf(env.Stdout, "Kept %s: %s.\n", shown, reason)
		return nil
	}

	if choice == askDir {
		if !isInteractive(env) {
			fmt.Fprintf(env.Stdout, "Kept %s: no terminal to ask, and --delete was not passed.\n", shown)
			return nil
		}
		q := fmt.Sprintf("Also delete %s and everything in it (its sessions and login)? [y/N] ", shown)
		if !confirm(env.Stdout, bufio.NewReader(env.Stdin), q) {
			fmt.Fprintf(env.Stdout, "Kept %s.\n", shown)
			return nil
		}
	}

	// RemoveAll on a symlink unlinks it and leaves its target alone.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("could not delete all of %s (%v); it is no longer registered, so remove what is left by hand", shown, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintf(env.Stdout, "Deleted the link %s; the directory it points to was kept.\n", shown)
		return nil
	}
	fmt.Fprintf(env.Stdout, "Deleted %s.\n", shown)
	return nil
}

// deletionBlocker returns why dir must not be deleted, or "" when it may.
// cfg no longer lists dir itself.
func deletionBlocker(cfg *config.Config, dir string, fi os.FileInfo) string {
	if isDefaultConfigDir(dir) {
		return "it is Claude's default config dir, which julienning never deletes"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "cannot resolve your home directory to check it is safe to delete (set $HOME)"
	}
	if discover.IsUnsafeConfigDir(dir, home) {
		return "it is /, your home directory or one of its parents"
	}
	if fi.Mode()&os.ModeSymlink == 0 && !fi.IsDir() {
		return "it is not a directory"
	}
	if users := dirUsers(cfg, dir); len(users) > 0 {
		return fmt.Sprintf("%s still uses files in it", strings.Join(users, ", "))
	}
	// Any live entry blocks, daemons and spares included: they run with this
	// dir as CLAUDE_CONFIG_DIR and would recreate files in it.
	sessions, err := livesess.List(dir)
	if err != nil {
		return fmt.Sprintf("cannot check it for running Claude sessions (%v)", err)
	}
	if len(sessions) > 0 {
		pids := make([]string, 0, len(sessions))
		for _, s := range sessions {
			pids = append(pids, strconv.Itoa(s.PID))
		}
		return fmt.Sprintf("Claude is running in it (pid %s); quit Claude there, then delete the dir yourself if you still want it gone", strings.Join(pids, ", "))
	}
	return ""
}

// isDefaultConfigDir is claudecfg.IsDefaultDir that also catches the same
// dir reached through a symlinked path (e.g. /var vs /private/var on macOS).
func isDefaultConfigDir(dir string) bool {
	if claudecfg.IsDefaultDir(dir) {
		return true
	}
	def, err := claudecfg.DefaultDir()
	if err != nil {
		return false
	}
	a, err1 := filepath.EvalSymlinks(dir)
	b, err2 := filepath.EvalSymlinks(def)
	return err1 == nil && err2 == nil && a == b
}

// dirUsers lists the registered dirs that deleting dir would break: dirs
// inside it, and dirs whose settings.json is a link to a file inside it.
func dirUsers(cfg *config.Config, dir string) []string {
	root := realPath(dir)
	var out []string
	for _, other := range cfg.Configs {
		if within(realPath(other.Dir), root) {
			out = append(out, other.Name)
			continue
		}
		if key, err := claudecfg.ResolvedSettingsPath(other.Dir); err == nil && within(key, root) {
			out = append(out, other.Name)
		}
	}
	return out
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// sharesSettingsWith lists the other registered dirs whose settings.json
// resolves to the same file as cd's.
func sharesSettingsWith(cfg *config.Config, cd config.ConfigDir) []string {
	key, err := claudecfg.ResolvedSettingsPath(cd.Dir)
	if err != nil {
		return nil // Unpatch reports the same problem
	}
	var out []string
	for _, other := range cfg.Configs {
		if other.Name == cd.Name {
			continue
		}
		if k, err := claudecfg.ResolvedSettingsPath(other.Dir); err == nil && k == key {
			out = append(out, other.Name)
		}
	}
	return out
}
