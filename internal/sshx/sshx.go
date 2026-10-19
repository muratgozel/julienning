// Package sshx runs commands on hosts through the system ssh (SPEC
// "Transport"): never a Go ssh library, so the user's ~/.ssh/config, agent,
// ProxyJump and known_hosts apply unchanged, and the dependencies stay the
// standard library plus x/term.
package sshx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
)

// SocketDir holds one ssh control socket per host (0700).
const SocketDir = "ssh"

// maxSocketPath bounds the control socket path. macOS caps socket paths at
// 104 bytes, and OpenSSH binds the master at ControlPath plus a temporary
// "." and 16 random characters before renaming it; a path that does not fit
// makes ssh exit 255 instead of falling back, so a longer one runs without
// ControlMaster.
const maxSocketPath = 100

// tempSuffix is the length of OpenSSH's temporary master socket suffix.
const tempSuffix = 17

// hashLen is the length %C expands to (a SHA-1 in hex).
const hashLen = 40

// Target is the host end of a call.
type Target struct {
	Name string // the name in hosts.json, for messages
	SSH  string // the ssh destination, always passed after --
	// Julienning is the absolute path of julienning on the host; the agent
	// command line names it in single quotes (hosts.Host.Validate forbids
	// quotes and backslashes in it).
	Julienning string
}

// TargetOf is the Target of a hosts.json entry.
func TargetOf(h hosts.Host) Target {
	return Target{Name: h.Name, SSH: h.SSH, Julienning: h.Paths.Julienning}
}

// Args is the ssh argv for one call (without "ssh" itself): the fixed
// options, -t or -T, "--", the destination and the remote command line.
// BatchMode means a call never stops at a password or host-key prompt;
// the control socket makes a command's several calls cost one handshake.
func Args(stateDir string, t Target, tty bool, command string) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	sock := filepath.Join(stateDir, SocketDir, "%C")
	// ssh splits an -o value at whitespace unless it is double-quoted, and
	// cannot take a path with a double quote at all.
	if len(sock)-2+hashLen+tempSuffix <= maxSocketPath && !strings.Contains(sock, `"`) {
		if strings.ContainsAny(sock, " \t") {
			sock = `"` + sock + `"`
		}
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+sock, "-o", "ControlPersist=60")
	}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	return append(args, "--", t.SSH, command)
}

// AgentCommand is the remote command line of an agent verb. Nothing variable
// travels on it but the julienning path and, for run and launch, one
// base64url argument.
func AgentCommand(julienning, verb string, arg ...string) string {
	cmd := "exec '" + julienning + "' agent " + verb
	for _, a := range arg {
		cmd += " " + a
	}
	return cmd
}

// ShellCommand is the remote command line of a POSIX script sent on stdin:
// before julienning is known there (the probe, the fallback install).
const ShellCommand = "exec /bin/sh -s"

// Binary is the ssh executable (a var for tests).
var Binary = "ssh"

// ErrInterrupted is a call ended from this side (Ctrl-C).
var ErrInterrupted = errors.New("interrupted")

// UnreachableError is ssh's own failure (exit 255; julienning never exits
// 255).
type UnreachableError struct {
	Target Target
	Detail string // ssh's last stderr line
}

func (e *UnreachableError) Error() string {
	d := e.Detail
	if d == "" {
		d = "ssh exited 255"
	}
	return fmt.Sprintf("cannot reach %s (%s); check with: ssh %s", e.Target.Name, d, e.Target.SSH)
}

// Call is one ssh invocation.
type Call struct {
	Target  Target
	TTY     bool
	Command string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer // what ssh and the host print; nil discards
	// Timeout kills ssh after this long (0: none).
	Timeout time.Duration
	// Interactive: SIGINT and SIGQUIT go to ssh (and through it to the host)
	// while it runs, never to this process.
	Interactive bool
}

// Run runs the call and returns the remote exit code. 255 is an
// *UnreachableError.
func (c Call) Run(ctx context.Context) (int, error) {
	state, err := config.Dir()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Join(state, SocketDir), 0o700); err != nil {
		return 0, fmt.Errorf("create %s: %w", filepath.Join(state, SocketDir), err)
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, Binary, Args(state, c.Target, c.TTY, c.Command)...)
	cmd.Env = sshEnv(os.Environ())
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	last := &lastLine{}
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(c.Stderr, last)
	} else {
		cmd.Stderr = last
	}
	cmd.WaitDelay = 2 * time.Second
	if c.Interactive {
		// A handler, not signal.Ignore: an ignored signal stays ignored in
		// the child, and ssh must still get Ctrl-C.
		sig := make(chan os.Signal, 4)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGQUIT)
		defer signal.Stop(sig)
	}
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return 0, fmt.Errorf("%s did not answer in time (%v); check with: ssh %s", c.Target.Name, ctx.Err(), c.Target.SSH)
	case ctx.Err() != nil:
		return 0, ErrInterrupted
	case errors.As(err, &ee):
		code := ee.ExitCode()
		if code == 255 {
			return code, &UnreachableError{Target: c.Target, Detail: last.String()}
		}
		if code < 0 {
			return 1, fmt.Errorf("ssh to %s ended by a signal", c.Target.Name)
		}
		return code, nil
	default:
		return 0, fmt.Errorf("run ssh: %w", err)
	}
}

// sshEnv is the environment ssh runs with: this machine's JULIENNING_*
// overrides never reach a host (a SendEnv in the user's ssh config would
// otherwise forward them).
func sshEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "JULIENNING_") {
			out = append(out, kv)
		}
	}
	return out
}

// lastLine keeps the last non-empty line written to it.
type lastLine struct {
	mu   sync.Mutex
	cur  []byte
	last string
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' || b == '\r' {
			if s := strings.TrimSpace(string(l.cur)); s != "" {
				l.last = s
			}
			l.cur = l.cur[:0]
			continue
		}
		if len(l.cur) < 4096 {
			l.cur = append(l.cur, b)
		}
	}
	return len(p), nil
}

func (l *lastLine) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := strings.TrimSpace(string(l.cur)); s != "" {
		return s
	}
	return l.last
}

// NoAnswerError is output without a frame line: whatever runs at the path
// is not a julienning agent (or the entry's path is stale).
type NoAnswerError struct {
	Target Target
	Detail string
}

func (e *NoAnswerError) Error() string {
	s := fmt.Sprintf("%s: no answer from julienning at %s (run: julienning host add %s to repair the entry)", e.Target.Name, e.Target.Julienning, e.Target.Name)
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// NoAgentError is a julienning too old to have the agent (exit 2 on it).
type NoAgentError struct{ Target Target }

func (e *NoAgentError) Error() string {
	return fmt.Sprintf("julienning on %s is too old to answer this machine (no agent command); update it with: julienning host add %s", e.Target.Name, e.Target.Name)
}

// maxAnswer bounds a framed answer read into memory.
const maxAnswer = 16 << 20

// Agent runs `agent VERB` on t with req as JSON on stdin (nil: nothing) and
// decodes the framed answer into resp. An error answer is an
// *hosts.AgentError, worded in the host's terms; callers print it as
// "julienning: box: <message>".
func Agent(ctx context.Context, t Target, verb string, req, resp any, timeout time.Duration) error {
	var stdin io.Reader = strings.NewReader("")
	if req != nil {
		raw, err := jsonMarshal(req)
		if err != nil {
			return err
		}
		stdin = bytes.NewReader(raw)
	}
	var out capped
	out.max = maxAnswer
	var errBuf capped
	errBuf.max = 64 << 10
	code, err := Call{Target: t, Command: AgentCommand(t.Julienning, verb), Stdin: stdin, Stdout: &out, Stderr: &errBuf, Timeout: timeout}.Run(ctx)
	if err != nil {
		return err
	}
	_, derr := hosts.DecodeAnswer(out.Bytes(), resp)
	switch {
	case derr == nil:
		return nil
	case errors.Is(derr, hosts.ErrNoFrame) && code == 2:
		return &NoAgentError{Target: t}
	case errors.Is(derr, hosts.ErrNoFrame):
		return &NoAnswerError{Target: t, Detail: lastOf(errBuf.String())}
	}
	return derr
}

// Script runs a POSIX script on t as `exec /bin/sh -s` and returns its
// stdout and exit code.
func Script(ctx context.Context, t Target, script string, timeout time.Duration, stderr io.Writer) ([]byte, int, error) {
	var out capped
	out.max = maxAnswer
	code, err := Call{Target: t, Command: ShellCommand, Stdin: strings.NewReader(script), Stdout: &out, Stderr: stderr, Timeout: timeout}.Run(ctx)
	return out.Bytes(), code, err
}

// Close ends t's control socket (`ssh -O exit`), best effort.
func Close(t Target) {
	state, err := config.Dir()
	if err != nil {
		return
	}
	sock := filepath.Join(state, SocketDir, "%C")
	if strings.ContainsAny(sock, " \t") {
		sock = `"` + sock + `"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, Binary, "-o", "ControlPath="+sock, "-O", "exit", "--", t.SSH)
	cmd.Env = sshEnv(os.Environ())
	_ = cmd.Run()
}

func lastOf(s string) string {
	l := &lastLine{}
	_, _ = l.Write([]byte(s))
	return l.String()
}

// capped is a buffer that stops growing at max (the rest is discarded and
// remembered as truncated).
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *capped) String() string { return string(c.Bytes()) }

func jsonMarshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode the request: %w", err)
	}
	return raw, nil
}
