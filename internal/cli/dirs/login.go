package dirs

import (
	"strings"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/launch"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "login",
		Summary: "run `claude` against a config dir so it can sign in",
		Usage:   "login NICKNAME|EMAIL|CONFIG [-- claude args...]",
		Run:     runLogin,
	})
}

// runLogin resolves a nickname or email to the dir logged into it, and a
// config name to that dir (a fresh dir nobody is signed into has only its
// config name). A shared account no local dir is logged into is a
// resolve.NotLocalError, whose message says how to sign in.
func runLogin(env cli.Env) error {
	target, extra, err := splitLoginArgs(env.Args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := resolveTarget(env, cfg, target)
	if err != nil {
		return err
	}
	// launch.Exec unsets CLAUDE_CONFIG_DIR for the default dir.
	return launch.Exec(m.Dir.Dir, extra, "")
}

// splitLoginArgs takes the target and the args after "--". Flags are not
// accepted so everything is unambiguously forwarded to claude.
func splitLoginArgs(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, cli.Usagef("missing nickname, email or config name")
	}
	target := args[0]
	rest := args[1:]
	if strings.HasPrefix(target, "-") {
		return "", nil, cli.Usagef("expected a nickname, email or config name, got %q", target)
	}
	if len(rest) == 0 {
		return target, nil, nil
	}
	if rest[0] != "--" {
		return "", nil, cli.Usagef("unexpected argument %q (pass claude args after `--`)", rest[0])
	}
	return target, rest[1:], nil
}
