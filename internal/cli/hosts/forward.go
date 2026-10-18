package hosts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/sshx"
)

// cwdCommands depend on the working directory: the controller sends its
// project and the host runs them in its checkout of it.
var cwdCommands = map[string]bool{"use": true, "next": true, "pin": true, "unpin": true, "login": true, "new-config": true}

// notSetUpOK may run on a host that has no team settings: setup asks for
// them there, update and uninstall are how a host gets fixed or removed.
var notSetUpOK = map[string]bool{"setup": true, "update": true, "uninstall": true}

// runForwarded runs `agent run` over ssh with this terminal (a var for
// tests).
var runForwarded = func(ctx context.Context, c sshx.Call) (int, error) { return c.Run(ctx) }

// forward is cli.Forward: the whole command runs on the host (SPEC
// "--host"), its output and exit code passing through unchanged.
func forward(env cli.Env, name string, c *cli.Command) int {
	code, err := forwardCommand(env, name, c)
	if err == nil {
		return code
	}
	if errors.Is(err, errUpdatedHere) {
		return cli.ExitOK
	}
	var ue *cli.UsageError
	if errors.As(err, &ue) {
		fmt.Fprintf(env.Stderr, "julienning: %s\nusage: julienning %s\n", ue.Msg, c.Usage)
		return cli.ExitUsage
	}
	fmt.Fprintf(env.Stderr, "julienning: %v\n", err)
	return cli.ExitError
}

func forwardCommand(env cli.Env, name string, c *cli.Command) (int, error) {
	argv := append([]string{c.Name}, env.Args...)
	if c.Name == "setup" {
		for _, a := range env.Args {
			if a == "--token" || a == "-token" || strings.HasPrefix(a, "--token=") || strings.HasPrefix(a, "-token=") {
				return 0, cli.Usagef("--token is never sent on a host's command line (other users there see it in ps); julienning host add %s sends this machine's team token on ssh's stdin", name)
			}
		}
	}
	_, h, err := loadHost(name)
	if err != nil {
		return 0, err
	}
	ctx := context.Background()
	hello, err := helloChecked(ctx, h)
	if err != nil {
		return 0, err
	}
	if !hello.SetUp && !notSetUpOK[c.Name] {
		return 0, fmt.Errorf("julienning is not set up on %s; run: julienning host add %s", h.Name, h.Name)
	}
	mode := versionsStrict
	if c.Name == "update" || c.Name == "uninstall" {
		mode = versionsLenient
	}
	if hello, err = checkVersions(ctx, env, newAsker(env, false), h, hello, mode); err != nil {
		return 0, err
	}
	var project *hosts.Project
	if needsProject(argv) {
		cwd, err := os.Getwd()
		if err != nil {
			return 0, fmt.Errorf("cannot determine the current directory: %w", err)
		}
		home, _ := os.UserHomeDir()
		project = hosts.ProjectOf(cwd, home)
	}
	return forwardTo(ctx, env, h, hello, argv, project)
}

// helloChecked asks h's agent hello and refuses another machine: a
// destination that now reaches a different machine (a mistyped alias can
// resolve to a router) is never acted on.
func helloChecked(ctx context.Context, h hosts.Host) (*hosts.Hello, error) {
	var hello hosts.Hello
	if err := agentCall(ctx, sshx.TargetOf(h), "hello", nil, &hello); err != nil {
		return nil, hostErr(h.Name, err)
	}
	if id := hello.ID(); id != "" && id != h.MachineID {
		return nil, fmt.Errorf("%s answers as machine %s, not %s: it was rebuilt, or %s reaches another machine now; check with: julienning host add %s",
			h.Name, id, h.MachineID, h.SSH, h.Name)
	}
	return &hello, nil
}

// needsProject reports whether argv runs in the working directory's
// checkout: use, next, login, new-config, and pin and unpin unless they
// name what they act on (pin --list, unpin DIR).
func needsProject(argv []string) bool {
	if !cwdCommands[argv[0]] {
		return false
	}
	rest := argv[1:]
	switch argv[0] {
	case "pin":
		for _, a := range rest {
			if a == "--list" || a == "-list" {
				return false
			}
		}
	case "unpin":
		for _, a := range rest {
			if a == "--" {
				return false
			}
			if !strings.HasPrefix(a, "-") {
				return false
			}
		}
	}
	return true
}

// forwardTo runs argv on h through `agent run` (SPEC "--host" step 3),
// with this terminal when there is one (ssh -t).
func forwardTo(ctx context.Context, env cli.Env, h hosts.Host, hello *hosts.Hello, argv []string, project *hosts.Project) (int, error) {
	tz := hosts.DisplayZone()
	if tz == "" {
		notef(env, "this machine's time zone has no IANA name (set TZ to one, such as Europe/Istanbul); %s shows times in its own zone", h.Name)
	}
	req := hosts.RunRequest{MachineID: hello.ID(), Argv: argv, Project: project, TZ: tz, NoColor: os.Getenv("NO_COLOR") != "", Host: h.Name}
	arg, err := hosts.EncodeArg(req)
	if err != nil {
		return 0, err
	}
	t := sshx.TargetOf(h)
	return runForwarded(ctx, sshx.Call{
		Target: t, TTY: isInteractive(env), Command: sshx.AgentCommand(t.Julienning, "run", arg),
		Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr, Interactive: true,
	})
}
