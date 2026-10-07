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
	Args   []string // args after the subcommand name
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Command is one subcommand. Run returns an error for runtime failures; return
// a *UsageError (via Usagef) for bad invocation so the exit code is ExitUsage.
type Command struct {
	Name    string
	Summary string // one line, shown in `julienning help`
	Usage   string // synopsis, e.g. "new-config [--name NAME] [--login]"
	Hidden  bool   // omit from help (internal commands like statusline helpers)
	Run     func(env Env) error
}

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

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return ExitUsage
	}
	name := args[0]
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
	err := c.Run(Env{Args: args[1:], Stdin: stdin, Stdout: stdout, Stderr: stderr})
	if err == nil {
		return ExitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(stdout, "usage: julienning %s\n", c.Usage)
		return ExitOK
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
