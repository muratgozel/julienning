package dirs

import (
	"fmt"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "rename",
		Summary: "rename a registered config (an internal label; `nick` renames an account)",
		Usage:   "rename OLD NEW",
		Run:     runRename,
	})
}

// runRename only edits config.json: `current` holds the dir path, the shell
// functions are named after account nicknames, and settings.json never
// mentions the name.
func runRename(env cli.Env) error {
	if len(env.Args) != 2 {
		return cli.Usagef("expected the current and the new config name")
	}
	oldName, newName := env.Args[0], env.Args[1]
	if !config.ValidConfigName(newName) {
		return invalidName(newName)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cd, ok := cfg.Find(oldName)
	if !ok {
		return unknownConfig(cfg, oldName)
	}
	if oldName == newName {
		return fmt.Errorf("config %q already has that name", oldName)
	}
	if other, taken := cfg.Find(newName); taken {
		return fmt.Errorf("config name %q is taken by %s (pick another name, or rename that one first)", newName, shortenHome(other.Dir))
	}
	if err := cfg.Rename(oldName, newName); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Renamed %q to %q (%s).\n", oldName, newName, shortenHome(cd.Dir))
	return nil
}
