package dirs

import (
	"fmt"
	"os"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/discover"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "adopt",
		Summary: "register an existing Claude config dir",
		Usage:   "adopt DIR [--name NAME]",
		Run:     runAdopt,
	})
}

func runAdopt(env cli.Env) error {
	fs := cli.NewFlagSet("adopt", env)
	nameFlag := fs.String("name", "", "config name (default: <prefix><N>, e.g. julienning1; ~/.claude is default)")
	args, err := parseFlagsPermute(fs, env.Args)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return cli.Usagef("expected exactly one directory")
	}
	if *nameFlag != "" && !config.ValidConfigName(*nameFlag) {
		return invalidName(*nameFlag)
	}

	dir, err := expandPath(args[0])
	if err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s does not exist (use `julienning new-config` to create one)", dir)
	}
	if err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if home, err := os.UserHomeDir(); err == nil && discover.IsUnsafeConfigDir(dir, home) {
		return fmt.Errorf("%s is your home directory or one of its parents, not a Claude config dir", dir)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if existing, ok := cfg.FindByDir(dir); ok {
		return fmt.Errorf("%s is already registered as %q", shortenHome(dir), existing.Name)
	}
	name := *nameFlag
	if name == "" {
		name = discover.AutoName(dir, cfg.Prefix(), func(n string) bool {
			_, taken := cfg.Find(n)
			return taken
		})
	}
	if err := cfg.Add(config.ConfigDir{Name: name, Dir: dir}); err != nil {
		return err
	}

	exe, err := patchCommand(env)
	if err != nil {
		return err
	}
	res, err := claudecfg.Patch(dir, exe)
	if err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Adopted %s as config %q (settings.json %s).\n", shortenHome(dir), name, res)
	if res.ReplacedStatusLine != "" {
		fmt.Fprintf(env.Stdout, "Replaced statusLine %q (saved; `julienning forget %s` restores it).\n", res.ReplacedStatusLine, name)
	}
	if email, ok, _ := dirEmail(dir); ok {
		if cache, err := sharedcache.Load(); err == nil && !cache.Contains(email) {
			fmt.Fprintf(env.Stdout, "%s is not shared with the team yet: julienning share %s\n", email, email)
		}
	}
	return nil
}
