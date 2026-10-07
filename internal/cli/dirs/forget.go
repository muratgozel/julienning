package dirs

import (
	"fmt"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "forget",
		Summary: "unregister a config dir and undo its settings.json changes (files are kept)",
		Usage:   "forget NAME",
		Run:     runForget,
	})
}

func runForget(env cli.Env) error {
	if len(env.Args) != 1 {
		return cli.Usagef("expected exactly one config name")
	}
	name := env.Args[0]
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cd, ok := cfg.Find(name)
	if !ok {
		return unknownConfig(cfg, name)
	}
	current, hasCurrent, err := cfg.Current()
	if err != nil {
		return err
	}

	// A settings.json shared with another registered dir (symlinks) still
	// serves that dir: unpatching it would unwire julienning there too.
	sharedWith := sharesSettingsWith(cfg, cd)

	// Unpatch first: if it fails the dir stays registered, so the user can fix
	// settings.json and retry instead of losing track of julienning's entries.
	var res claudecfg.Result
	if len(sharedWith) == 0 {
		if res, err = claudecfg.Unpatch(cd.Dir); err != nil {
			return fmt.Errorf("%w (fix it and re-run `julienning forget %s`)", err, name)
		}
	}
	cfg.Remove(name)
	if hasCurrent && current.Name == name {
		if err := config.ClearCurrent(); err != nil {
			return err
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "Forgot %q. %s was not deleted.\n", name, shortenHome(cd.Dir))
	if len(sharedWith) > 0 {
		fmt.Fprintf(env.Stdout, "settings.json: left as is; it is the same file as the settings.json of %s, which still uses julienning.\n", strings.Join(sharedWith, ", "))
	}
	if len(res.Changes) > 0 {
		fmt.Fprintf(env.Stdout, "settings.json: %s.\n", strings.Join(res.Changes, ", "))
	}
	if hasCurrent && current.Name == name {
		fmt.Fprintln(env.Stdout, "It was the current config; nothing is selected now.")
	}
	return nil
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
