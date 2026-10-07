package dirs

import (
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
)

// dirPrefix is the prefix of every non-default Claude config dir julienning
// creates ("~/.claude-foo").
const dirPrefix = ".claude-"

// maxAutoNumber bounds new-config's search for a free <prefix><N>.
const maxAutoNumber = 999

func init() {
	cli.Register(&cli.Command{
		Name:    "new-config",
		Summary: "create and register a new Claude config dir",
		Usage:   "new-config [--name NAME] [--copy-settings-from NAME] [--login]",
		Run:     runNewConfig,
	})
}

func runNewConfig(env cli.Env) error {
	fs := cli.NewFlagSet("new-config", env)
	nameFlag := fs.String("name", "", "config name or dir basename (e.g. foo or .claude-foo; default: <prefix><N> in ~/.claude-<prefix><N>)")
	copyFrom := fs.String("copy-settings-from", "", "registered config whose settings.json to copy")
	login := fs.Bool("login", false, "start claude in the new config afterwards so it can sign in")
	if err := parseFlags(fs, env.Args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cli.Usagef("unexpected argument %q (use --name)", fs.Arg(0))
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
	cd := config.ConfigDir{Name: name, Dir: dir}
	if err := cfg.Add(cd); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	registered = true

	fmt.Fprintf(env.Stdout, "Created %s (config %q).\n", shortenHome(dir), name)
	fmt.Fprintf(env.Stdout, "Sign in: julienning login %s\n", name)
	fmt.Fprintln(env.Stdout, "Then share the account with the team: julienning setup (or julienning share EMAIL).")

	if *login {
		return launch.Exec(dir, nil, "")
	}
	return nil
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
