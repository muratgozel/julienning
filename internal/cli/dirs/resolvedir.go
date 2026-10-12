package dirs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/pins"
	"github.com/muratgozel/julienning/internal/resolve"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "resolve-dir",
		Summary: "print the config dir a nickname, email or config name resolves to (used by the claude-<nickname> shell functions)",
		Usage:   "resolve-dir NICKNAME|EMAIL|CONFIG | resolve-dir --pin",
		Hidden:  true,
		Run:     runResolveDir,
	})
}

// runResolveDir runs on every claude-<nick> invocation, so it never touches
// the network. stdout carries only the path (captured by the function);
// errors go to stderr and exit 1, which makes the function return 1.
//
// The path is printed cleaned: the function compares it byte for byte with
// claudecfg.DefaultDir (baked in by shell-init) to decide whether to unset
// CLAUDE_CONFIG_DIR, and claudecfg.IsDefaultDir uses the same comparison.
func runResolveDir(env cli.Env) error {
	if len(env.Args) != 1 {
		return cli.Usagef("expected exactly one nickname, email or config name, or --pin")
	}
	if env.Args[0] == "--pin" {
		return resolvePin(env)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := resolveTarget(env, cfg, env.Args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, filepath.Clean(m.Dir.Dir))
	return nil
}

// resolvePin is what the claude wrapper asks while pins.json exists: the
// dir of the account pinned on the working directory (or a parent), or no
// output and exit 0 when no pin applies, so the wrapper follows the
// selection. A pin that cannot be resolved is an error, never an empty
// answer: the wrapper must not fall back to the selection and open a
// pinned project on another account.
func resolvePin(env cli.Env) error {
	list, err := pins.Load()
	if err != nil || len(list) == 0 {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot determine the current directory to look up its pin (%v); start claude from an existing directory, or bypass julienning with: command claude", err)
	}
	p, ok := pins.Match(list, cwd)
	if !ok {
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// The selection only breaks ties between dirs of one team account; the
	// pin alone names the account, so an unreadable `current` must not stop
	// claude here (`use` treats it the same way).
	var cur *config.ConfigDir
	if current, hasCurrent, err := cfg.Current(); err != nil {
		warnf(env, "ignoring the current selection (%v)", err)
	} else if hasCurrent {
		cur = &current
	}
	m, pm, err := resolve.Account(cfg, loadCache(env), p.Email, cur)
	if err != nil {
		return pins.UseError(p, err)
	}
	if pm != nil {
		for _, n := range pm.Notes() {
			notef(env, "%s", n)
		}
		if err := pm.Remember(); err != nil {
			warnf(env, "%v", err)
		}
	}
	fmt.Fprintln(env.Stdout, filepath.Clean(m.Dir.Dir))
	return nil
}
