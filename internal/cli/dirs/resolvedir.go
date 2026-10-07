package dirs

import (
	"fmt"
	"path/filepath"

	"github.com/muratgozel/julienning/internal/cli"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "resolve-dir",
		Summary: "print the config dir a nickname, email or config name resolves to (used by the claude-<nickname> shell functions)",
		Usage:   "resolve-dir NICKNAME|EMAIL|CONFIG",
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
		return cli.Usagef("expected exactly one nickname, email or config name")
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
