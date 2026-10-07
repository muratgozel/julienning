package dirs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/paths"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "uninstall",
		Summary: "undo setup: settings.json entries, shell hook, claims, selection (--purge: also data and binary)",
		Usage:   "uninstall [--purge] [--yes]",
		Run:     runUninstall,
	})
}

// uninstallRCFiles are every rc file setup may have written, whatever $SHELL
// is now.
var uninstallRCFiles = []string{".zshrc", ".bashrc", ".bash_profile"}

func runUninstall(env cli.Env) error {
	fs := cli.NewFlagSet("uninstall", env)
	purge := fs.Bool("purge", false, "also delete ~/.julienning, installed versions and the julienning command")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := parseFlags(fs, env.Args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cli.Usagef("unexpected argument %q", fs.Arg(0))
	}
	home, err := homeDir()
	if err != nil {
		return err
	}

	// Everything to purge is resolved and confirmed before anything changes,
	// so answering "no" leaves the machine exactly as it was.
	var plan *purgePlan
	if *purge {
		plan, err = newPurgePlan(home)
		if err != nil {
			return err
		}
		if !*yes {
			if !isInteractive(env) {
				return errors.New("refusing to --purge without confirmation: run it in a terminal, or add --yes")
			}
			shown := make([]string, 0, 3)
			for _, p := range []string{plan.stateDir, plan.versionsDir, plan.link} {
				shown = append(shown, shortenHome(p))
			}
			q := fmt.Sprintf("Delete %s? This cannot be undone. [y/N] ", strings.Join(shown, ", "))
			if !confirm(env.Stdout, bufio.NewReader(env.Stdin), q) {
				fmt.Fprintln(env.Stdout, "Cancelled; nothing was changed.")
				return nil
			}
		}
	}

	cfg, err := config.Load()
	cfgUnreadable := false
	switch {
	case errors.Is(err, config.ErrNotSetup):
		cfg = nil
	case err != nil:
		warnf(env, "cannot read the julienning config (%v); registered dirs' settings.json are left as they are", err)
		cfg = nil
		cfgUnreadable = true
	}

	changed, problems := 0, 0
	say := func(format string, a ...any) {
		changed++
		fmt.Fprintf(env.Stdout, format+"\n", a...)
	}

	// settings.json of every registered dir. Dirs sharing one settings.json
	// (symlinks) are unpatched once: the first Unpatch spends the record, and
	// a second pass would find nothing left to restore.
	unpatchFailures := 0
	if cfg != nil {
		done := map[string]bool{}
		for _, cd := range cfg.Configs {
			if key, err := claudecfg.ResolvedSettingsPath(cd.Dir); err == nil {
				if done[key] {
					continue
				}
				done[key] = true
			}
			res, err := claudecfg.Unpatch(cd.Dir)
			if err != nil {
				fmt.Fprintf(env.Stderr, "julienning: %v\n", err)
				problems++
				unpatchFailures++
				continue
			}
			if len(res.Changes) > 0 {
				say("Settings: %s: %s", shortenHome(res.Path), strings.Join(res.Changes, ", "))
			}
		}
	}

	// rc hook lines
	for _, name := range uninstallRCFiles {
		rc := filepath.Join(home, name)
		n, err := removeHookLines(rc)
		if err != nil {
			fmt.Fprintf(env.Stderr, "julienning: %v\n", err)
			problems++
			continue
		}
		if n > 0 {
			say("Shell:    removed the julienning hook line from %s", shortenHome(rc))
		}
	}

	// claims (best effort: the Worker's TTL cleans up whatever is left)
	if cfg != nil {
		released, err := releaseClaims(cfg)
		if released > 0 {
			say("Claims:   released %d", released)
		}
		if err != nil {
			warnf(env, "could not release every claim (%v); they expire on the Worker by themselves", err)
		}
	}

	// current selection
	cur, err := config.Path(config.CurrentFile)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(cur); err == nil {
		if err := config.ClearCurrent(); err != nil {
			fmt.Fprintf(env.Stderr, "julienning: %v\n", err)
			problems++
		} else {
			say("Current:  cleared the selected config dir")
		}
	}

	// CRITICAL: purging deletes patches.json, the only copy of the users'
	// replaced statusLines, and config.json, the list of dirs still wired to
	// julienning. While either could still be needed, nothing is purged, and
	// the command stays installed so the user can fix things and re-run.
	if plan != nil {
		switch {
		case cfgUnreadable:
			fmt.Fprintf(env.Stderr, "julienning: not purging: %s could not be read, so registered dirs may still run julienning and %s may hold statusLines to restore; fix or remove it, then re-run `julienning uninstall --purge`\n",
				shortenHome(filepath.Join(plan.stateDir, config.ConfigFile)), claudecfg.PatchesFile)
			problems++
		case unpatchFailures > 0:
			fmt.Fprintf(env.Stderr, "julienning: not purging: %d settings.json could not be restored (see above); %s keeps what is needed to restore them and the julienning command stays, so fix them and re-run `julienning uninstall --purge`\n",
				unpatchFailures, shortenHome(plan.stateDir))
			problems++
		default:
			problems += plan.run(env, say)
		}
	}

	if changed == 0 && problems == 0 {
		fmt.Fprintln(env.Stdout, "Nothing to uninstall.")
	}
	if !*purge && cfg != nil {
		dir, _ := config.Dir()
		fmt.Fprintf(env.Stdout, "Kept %s (identity, registered dirs): `julienning setup` re-installs, `julienning uninstall --purge` removes it and the julienning command.\n", shortenHome(dir))
	}
	if problems > 0 {
		return fmt.Errorf("uninstall finished with %d problem(s) (see above); fix them and re-run it", problems)
	}
	return nil
}

// releaseClaims deletes this machine's claims when the Worker is configured,
// returning how many were released.
func releaseClaims(cfg *config.Config) (int, error) {
	if cfg.RequireRemote() != nil {
		return 0, nil
	}
	before, err := claims.LoadHeld()
	if err != nil {
		return 0, err
	}
	if len(before) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*remoteTimeout)
	defer cancel()
	relErr := claims.ReleaseAll(ctx, cfg, newClient(cfg))
	after, err := claims.LoadHeld()
	if err != nil {
		return 0, err
	}
	if relErr != nil {
		relErr = remoteErr("release claims", relErr)
	}
	return len(before) - len(after), relErr
}

// removeHookLines drops julienning's hook lines (new and legacy markers) from
// an rc file (rewriteRC: atomic, through symlinks). It returns how many
// lines it removed.
func removeHookLines(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	kept := lines[:0:0]
	removed := 0
	for _, line := range lines {
		if hookLineKind(line) != hookAbsent {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, rewriteRC(path, strings.Join(kept, "\n"))
}

// purgePlan is what --purge may delete. Only julienning's own files are
// removed from these locations, and a directory only once it is empty:
// $JULIENNING_HOME and $JULIENNING_VERSIONS_DIR can point anywhere, and a
// recursive delete there could take the user's data with it.
type purgePlan struct {
	stateDir    string // ~/.julienning
	versionsDir string // ~/.local/share/julienning/versions
	link        string // ~/.local/bin/julienning
}

// newPurgePlan resolves the purge locations, refusing paths that could only
// be a misconfiguration ($HOME, / or a parent of $HOME).
func newPurgePlan(home string) (*purgePlan, error) {
	jl, err := config.Dir()
	if err != nil {
		return nil, err
	}
	versions, err := paths.VersionsDir()
	if err != nil {
		return nil, err
	}
	link, err := paths.BinLink()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, 3)
	for _, p := range []string{jl, versions, link} {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", p, err)
		}
		if err := safeToDelete(abs, home); err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return &purgePlan{stateDir: out[0], versionsDir: out[1], link: out[2]}, nil
}

// run deletes what the plan covers, reporting through say, and returns how
// many problems it hit.
func (p *purgePlan) run(env cli.Env, say func(string, ...any)) int {
	problems := 0
	fail := func(err error) {
		fmt.Fprintf(env.Stderr, "julienning: %v\n", err)
		problems++
	}
	// Decided before the versions dir is emptied: the link's target is gone
	// afterwards.
	linkOwned, linkWhy := linkIntoDir(p.link, p.versionsDir)

	if err := purgeDir(env, say, p.stateDir, ownedStateEntry); err != nil {
		fail(err)
	}
	if err := purgeDir(env, say, p.versionsDir, ownedVersionEntry); err != nil {
		fail(err)
	} else if filepath.Base(p.versionsDir) == "versions" && filepath.Base(filepath.Dir(p.versionsDir)) == "julienning" {
		// ~/.local/share/julienning holds nothing but versions/.
		_ = removeEmptyDir(filepath.Dir(p.versionsDir))
	}

	switch {
	case linkOwned:
		if err := os.Remove(p.link); err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(fmt.Errorf("delete %s: %w", p.link, err))
		} else if err == nil {
			say("Purged:   %s", shortenHome(p.link))
		}
	case linkWhy != "":
		fmt.Fprintf(env.Stdout, "Kept %s: %s.\n", shortenHome(p.link), linkWhy)
	}
	return problems
}

// purgeDir deletes the entries of dir that owned accepts, then dir itself if
// that left it empty. Anything else stays and is reported.
func purgeDir(env cli.Env, say func(string, ...any), dir string, owned func(dir string, e os.DirEntry) bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	removed := 0
	var kept []string
	for _, e := range entries {
		if !owned(dir, e) {
			kept = append(kept, e.Name())
			continue
		}
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			// Only sent/ is an owned directory; it is emptied the same way.
			if err := purgeDir(env, func(string, ...any) {}, path, ownedSentEntry); err != nil {
				return err
			}
			if _, err := os.Lstat(path); err == nil {
				kept = append(kept, e.Name())
				continue
			}
		} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete %s: %w", path, err)
		}
		removed++
	}
	if len(kept) == 0 {
		if err := removeEmptyDir(dir); err != nil {
			return err
		}
		say("Purged:   %s", shortenHome(dir))
		return nil
	}
	if removed > 0 {
		say("Purged:   julienning's files in %s", shortenHome(dir))
	}
	shown := kept
	if len(shown) > 3 {
		shown = append(append([]string(nil), kept[:3]...), "…")
	}
	fmt.Fprintf(env.Stdout, "Kept %s: it also holds files julienning did not create (%s).\n", shortenHome(dir), strings.Join(shown, ", "))
	return nil
}

func removeEmptyDir(dir string) error {
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", dir, err)
	}
	return nil
}

// stateFiles are the files julienning writes in its state dir (SPEC "Local
// state"). Atomic writers leave ".<name>.<random>" temp files after a crash.
var stateFiles = map[string]bool{
	config.ConfigFile: true, config.CurrentFile: true, sharedcache.File: true,
	claims.File: true, claudecfg.PatchesFile: true, selfupdate.CheckFile: true,
	config.ErrorLog: true, config.ErrorLog + ".trim": true, claims.LockFile: true,
	".claims-reconciled": true,
}

func ownedStateEntry(_ string, e os.DirEntry) bool {
	name := e.Name()
	if e.IsDir() { // false for a symlink, which is never julienning's
		return name == config.SentDir
	}
	if stateFiles[name] || strings.HasPrefix(name, ".last-") {
		return true
	}
	for f := range stateFiles {
		if strings.HasPrefix(name, "."+f+".") {
			return true
		}
	}
	return false
}

var sentFileRe = regexp.MustCompile(`^([0-9a-f]{8}|[^/]*@[^/]*)\.(json|inflight)$|^\.tmp-`)

func ownedSentEntry(_ string, e os.DirEntry) bool {
	return !e.IsDir() && sentFileRe.MatchString(e.Name())
}

// versionNameRe matches installed version files (`dev` or a semver without
// a leading v); versionTempRe the temp files selfupdate leaves after a crash.
var (
	versionNameRe = regexp.MustCompile(`^(dev|[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?)$`)
	versionTempRe = regexp.MustCompile(`^\.download-[^/]*\.tar\.gz$|^\.(.+)\.[0-9]+\.tmp$`)
)

func ownedVersionEntry(_ string, e os.DirEntry) bool {
	if e.IsDir() {
		return false
	}
	name := e.Name()
	if versionNameRe.MatchString(name) {
		return true
	}
	m := versionTempRe.FindStringSubmatch(name)
	return m != nil && (m[1] == "" || versionNameRe.MatchString(m[1]))
}

// linkIntoDir reports whether link is a symlink into dir (the install
// layout). Otherwise why says why it is kept; why is empty when there is
// no link at all.
func linkIntoDir(link, dir string) (owned bool, why string) {
	fi, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return false, ""
	}
	if err != nil {
		return false, "cannot inspect it (" + err.Error() + ")"
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return false, "it is not a symlink into " + shortenHome(dir) + ", so julienning did not install it"
	}
	target, err := os.Readlink(link)
	if err != nil {
		return false, "cannot read the symlink (" + err.Error() + ")"
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if !sameDirPath(filepath.Dir(filepath.Clean(target)), dir) {
		return false, "it points to " + shortenHome(target) + ", outside " + shortenHome(dir)
	}
	return true, ""
}

func sameDirPath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func safeToDelete(p, home string) error {
	p = filepath.Clean(p)
	home = filepath.Clean(home)
	sep := string(filepath.Separator)
	depth := len(strings.Split(strings.Trim(p, sep), sep))
	if p == sep || depth < 2 || strings.HasPrefix(home+sep, p+sep) {
		return fmt.Errorf("refusing to delete %s: it is /, $HOME or one of its parents (check JULIENNING_HOME, JULIENNING_VERSIONS_DIR, JULIENNING_BIN_DIR)", p)
	}
	return nil
}
