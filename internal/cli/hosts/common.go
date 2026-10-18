// Package hosts holds the commands that involve other machines (SPEC
// "Hosts"): host add|list|remove, the hidden agent every cross-machine
// command talks to, and --host forwarding (cli.Forward).
package hosts

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/sshx"
)

// callTimeout bounds one agent call that is not a forwarded command.
const callTimeout = 30 * time.Second

// Injection points (tests).
var (
	isInteractive = func(env cli.Env) bool { return isTerminal(env.Stdin) && isTerminal(env.Stdout) }
	now           = time.Now
	// agentCall runs one framed agent verb on a host.
	agentCall = func(ctx context.Context, t sshx.Target, verb string, req, resp any) error {
		return sshx.Agent(ctx, t, verb, req, resp, callTimeout)
	}
)

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func warnf(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: warning: %s\n", fmt.Sprintf(format, a...))
}

func notef(env cli.Env, format string, a ...any) {
	fmt.Fprintf(env.Stderr, "julienning: note: %s\n", fmt.Sprintf(format, a...))
}

// hostErr prefixes an agent's error with the host's name, the way every
// message about a host reads: "julienning: box: <message>".
func hostErr(name string, err error) error {
	var ae *hosts.AgentError
	if errors.As(err, &ae) {
		return fmt.Errorf("%s: %s", name, ae.Message)
	}
	return err
}

// unknownHost is the error for a --host or argument hosts.json lacks.
func unknownHost(name string) error {
	return fmt.Errorf("unknown host %q (see julienning host list; add it with: julienning host add %s SSH_DEST)", name, name)
}

// loadHost reads hosts.json and the entry for name.
func loadHost(name string) (*hosts.Hosts, hosts.Host, error) {
	hf, err := hosts.Load()
	if err != nil {
		return nil, hosts.Host{}, err
	}
	h, ok := hf.Find(name)
	if !ok {
		return nil, hosts.Host{}, unknownHost(name)
	}
	return hf, h, nil
}

// asker asks the questions of host add and the version rule. With yes,
// questions that default to yes are taken as yes and the others refused;
// without a terminal (and without yes) every question is an error naming
// the command that answers it.
type asker struct {
	env         cli.Env
	in          *bufio.Reader
	interactive bool
	yes         bool
}

func newAsker(env cli.Env, yes bool) *asker {
	return &asker{env: env, in: bufio.NewReader(env.Stdin), interactive: isInteractive(env), yes: yes}
}

// errDeclined is a question answered no (or refused under --yes).
var errDeclined = errors.New("declined")

// ask asks q ([Y/n] when def, else [y/N]). hint is the command that
// answers it without a terminal. A no is errDeclined; no terminal and no
// --yes is an error naming hint.
func (a *asker) ask(q string, def bool, hint string) error {
	switch {
	case a.yes && def:
		return nil
	case a.yes:
		return errDeclined
	case !a.interactive:
		if hint == "" {
			return fmt.Errorf("%s (no terminal to ask)", strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(q), "?")))
		}
		return fmt.Errorf("%s (no terminal to ask; %s)", strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(q), "?")), hint)
	}
	choices := "[y/N]"
	if def {
		choices = "[Y/n]"
	}
	for {
		fmt.Fprintf(a.env.Stdout, "%s %s ", q, choices)
		line, err := a.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(a.env.Stdout)
			return errDeclined
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			if def {
				return nil
			}
			return errDeclined
		case "y", "yes":
			return nil
		case "n", "no":
			return errDeclined
		}
		fmt.Fprintln(a.env.Stdout, "Answer y or n.")
	}
}

// homeShort renders p with the given home as "~".
func homeShort(p, home string) string {
	if home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}
