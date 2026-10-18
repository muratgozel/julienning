// Package cli dispatches subcommands. Each command lives in its own file and
// registers itself via Register in an init func; root.go owns dispatch, help
// and exit codes so parallel authors never edit the same file.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/muratgozel/julienning/internal/version"
)

// Exit codes. ExitUsage is for bad flags/args; ExitError for runtime failures.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Env is what a command receives. Commands must write only to Stdout/Stderr
// (never os.Stdout) so tests can capture output.
type Env struct {
	Args   []string // args after the subcommand name, --host taken off
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Host is --host NAME for a HostSource command ("" for this machine).
	Host string
}

// HostMode is what a command does with --host NAME (SPEC "--host").
type HostMode int

const (
	// HostInternal (the zero value, every hidden command): runs where
	// Claude or the shell runs; --host is a usage error.
	HostInternal HostMode = iota
	// HostForward: the whole command runs on the host (Forward).
	HostForward
	// HostSource: the host is where the command's input comes from; the
	// command gets it as Env.Host (transfer).
	HostSource
	// HostLocal: about this machine itself (host add, help); --host is a
	// usage error.
	HostLocal
)

// Forward runs c with env.Args on host and returns the exit code. Set by
// internal/cli/hosts; nil leaves --host unsupported.
var Forward func(env Env, host string, c *Command) int

// Command is one subcommand. Run returns an error for runtime failures; return
// a *UsageError (via Usagef) for bad invocation so the exit code is ExitUsage.
type Command struct {
	Name    string
	Summary string // one line, shown in `julienning help`
	Usage   string // synopsis, e.g. "new-config [--name NAME] [--login]"
	Hidden  bool   // omit from help (internal commands like statusline helpers)
	Host    HostMode
	Run     func(env Env) error
}

// ExitStatus ends the command with Code and prints nothing: the command
// already reported the problem its own way (the agent's framed error, a
// forwarded command's own output).
type ExitStatus struct{ Code int }

func (e *ExitStatus) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Exit builds an ExitStatus.
func Exit(code int) error { return &ExitStatus{Code: code} }

// UsageError marks an invocation error; Main maps it to ExitUsage.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Usagef builds a UsageError.
func Usagef(format string, a ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, a...)}
}

var registry = map[string]*Command{}

// Register adds a command; called from init funcs. Panics on duplicates so a
// naming clash fails at startup rather than silently shadowing.
func Register(c *Command) {
	if c == nil || c.Name == "" || c.Run == nil {
		panic("cli: Register requires Name and Run")
	}
	if _, dup := registry[c.Name]; dup {
		panic("cli: duplicate command " + c.Name)
	}
	registry[c.Name] = c
}

// NewFlagSet returns a FlagSet wired to env.Stderr with ContinueOnError so
// commands can convert parse failures into UsageErrors.
func NewFlagSet(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// LocalHost is the host name that means this machine.
const LocalHost = "local"

// splitHost takes --host NAME / --host=NAME off args, anywhere before a
// "--" (what follows it, as in `login alpha -- --model x`, belongs to
// another program and is never inspected).
func splitHost(args []string) (host string, rest []string, err error) {
	given := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		var v string
		switch {
		case a == "--host":
			if i+1 >= len(args) || args[i+1] == "--" {
				return "", nil, errors.New("--host needs a host name")
			}
			v = args[i+1]
			i++
		case strings.HasPrefix(a, "--host="):
			v = strings.TrimPrefix(a, "--host=")
		default:
			rest = append(rest, a)
			continue
		}
		if given {
			return "", nil, errors.New("--host is given twice")
		}
		if v == "" {
			return "", nil, errors.New("--host needs a host name")
		}
		host, given = v, true
	}
	if host == LocalHost {
		host = ""
	}
	return host, rest, nil
}

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	host, args, err := splitHost(args)
	if err != nil {
		fmt.Fprintf(stderr, "julienning: %v\n", err)
		return ExitUsage
	}
	if len(args) == 0 {
		printHelp(stdout)
		return ExitUsage
	}
	name := args[0]
	if host != "" {
		switch name {
		case "help", "-h", "--help", "version", "-v", "--version":
			fmt.Fprintf(stderr, "julienning: %s does not take --host\n", strings.TrimLeft(name, "-"))
			return ExitUsage
		}
	}
	switch name {
	case "help", "-h", "--help":
		if len(args) > 1 {
			if c, ok := registry[args[1]]; ok {
				fmt.Fprintf(stdout, "usage: julienning %s\n\n%s\n", c.Usage, c.Summary)
				return ExitOK
			}
		}
		printHelp(stdout)
		return ExitOK
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "julienning %s\n", version.Version)
		return ExitOK
	}
	c, ok := registry[name]
	if !ok {
		fmt.Fprintf(stderr, "julienning: unknown command %q (run `julienning help`)\n", name)
		return ExitUsage
	}
	env := Env{Args: args[1:], Stdin: stdin, Stdout: stdout, Stderr: stderr}
	if host != "" {
		switch c.Host {
		case HostForward:
			if Forward == nil {
				fmt.Fprintf(stderr, "julienning: %s cannot run on another machine in this build\n", name)
				return ExitError
			}
			return Forward(env, host, c)
		case HostSource:
			env.Host = host
		default:
			label := name
			if c.Host == HostLocal && len(args) > 1 && !strings.HasPrefix(args[1], "-") {
				label += " " + args[1] // host list, host add
			}
			fmt.Fprintf(stderr, "julienning: %s does not take --host\nusage: julienning %s\n", label, c.Usage)
			return ExitUsage
		}
	}
	err = c.Run(env)
	if err == nil {
		return ExitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(stdout, "usage: julienning %s\n", c.Usage)
		return ExitOK
	}
	var es *ExitStatus
	if errors.As(err, &es) {
		return es.Code
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		fmt.Fprintf(stderr, "julienning: %s\nusage: julienning %s\n", ue.Msg, c.Usage)
		return ExitUsage
	}
	fmt.Fprintf(stderr, "julienning: %s\n", err)
	return ExitError
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, "julienning %s — shared Claude Code account switching and usage tracking\n\n", version.Version)
	fmt.Fprintln(w, "usage: julienning <command> [flags]")
	fmt.Fprintln(w)
	names := make([]string, 0, len(registry))
	width := 0
	for n, c := range registry {
		if c.Hidden {
			continue
		}
		names = append(names, n)
		if len(n) > width {
			width = len(n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-*s  %s\n", width, n, registry[n].Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `julienning help <command>` for details.")
}
