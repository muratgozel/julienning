package hosts

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/cli/switching"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/resolve"
	"github.com/muratgozel/julienning/internal/sessions"
)

// The agent verbs transfer uses (SPEC "Agent"). Each acts only on this
// machine's registered config dirs.

// registered returns the registered dir at path dir.
func registered(dir string) (*config.Config, config.ConfigDir, error) {
	cfg, err := config.Load()
	if errors.Is(err, config.ErrNotSetup) {
		return nil, config.ConfigDir{}, hosts.Errorf(hosts.CodeNotSetUp, "julienning is not set up")
	}
	if err != nil {
		return nil, config.ConfigDir{}, err
	}
	cd, ok := cfg.FindByDir(dir)
	if !ok {
		return cfg, cd, hosts.Errorf(hosts.CodeInvalid, "%s is not a registered config dir here", dir)
	}
	return cfg, cd, nil
}

// moveErr gives a move error its agent code (LIVE, COLLISION, …).
func moveErr(err error) error {
	var ae *hosts.AgentError
	if errors.As(err, &ae) {
		return err
	}
	if kind := sessions.ErrorKind(err); kind != "" {
		return hosts.Errorf(kind, "%s", err.Error())
	}
	return err
}

func configDirs(cfg *config.Config) []string {
	var dirs []string
	if cfg != nil {
		for _, cd := range cfg.Configs {
			dirs = append(dirs, cd.Dir)
		}
	}
	return dirs
}

func agentLocate(req hosts.LocateRequest) (hosts.Located, error) {
	if req.Project == nil {
		return hosts.Located{}, hosts.Errorf(hosts.CodeInvalid, "the request names no project")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return hosts.Located{}, err
	}
	cfg, _ := config.Load()
	o := hosts.LocateOptions{Home: home, ConfigDirs: configDirs(cfg), Branch: req.Branch, Machine: req.Machine, RanIn: req.RanIn}
	if req.Path != "" {
		loc, err := hosts.Check(req.Project, req.Path, o)
		var pm *hosts.PathMissingError
		switch {
		case errors.As(err, &pm):
			return loc, hosts.AsAgentError(err)
		case err != nil:
			return loc, hosts.Errorf(hosts.CodeNoCheckout, "%s", err.Error())
		}
		return loc, nil
	}
	loc, err := hosts.Locate(req.Project, o)
	if err != nil {
		return loc, hosts.AsAgentError(err)
	}
	return loc, nil
}

func agentSessions(req hosts.SessionsRequest) (hosts.SessionsAnswer, error) {
	ans := hosts.SessionsAnswer{Sessions: []sessions.Session{}, Labels: map[string]string{}, Projects: map[string]*hosts.Project{}}
	home, err := os.UserHomeDir()
	if err != nil {
		return ans, err
	}
	cfg, err := config.Load()
	if errors.Is(err, config.ErrNotSetup) {
		return ans, hosts.Errorf(hosts.CodeNotSetUp, "julienning is not set up")
	}
	if err != nil {
		return ans, err
	}
	if !req.All {
		if req.Project == nil {
			return ans, hosts.Errorf(hosts.CodeInvalid, "the request names no project")
		}
		loc, err := hosts.Locate(req.Project, hosts.LocateOptions{Home: home, ConfigDirs: configDirs(cfg), Branch: req.Project.Branch, Machine: req.Machine})
		if err != nil {
			return ans, hosts.AsAgentError(err)
		}
		ans.Cwd = loc.Cwd
	}
	limit := req.Limit
	if limit <= 0 {
		limit = sessions.DefaultLimit
	}
	list, labels, warnings, err := switching.TransferSessions(ans.Cwd, req.All, limit)
	if err != nil {
		return ans, err
	}
	if list != nil {
		ans.Sessions = list
	}
	if labels != nil {
		ans.Labels = labels
	}
	ans.Warnings = warnings
	for _, s := range list {
		if _, ok := ans.Projects[s.Cwd]; !ok && s.Cwd != "" {
			ans.Projects[s.Cwd] = hosts.ProjectOf(s.Cwd, home)
		}
	}
	return ans, nil
}

func agentChoose(req hosts.ChooseRequest) (hosts.ChooseAnswer, error) {
	loc := time.Local
	if req.TZ != "" {
		if l, err := time.LoadLocation(req.TZ); err == nil {
			loc = l
		}
	}
	c, err := switching.Choose(req.Target, loc)
	var pe *switching.PersonalError
	var nl *resolve.NotLocalError
	switch {
	case errors.Is(err, switching.ErrNoneLoggedIn):
		return hosts.ChooseAnswer{}, hosts.Errorf(hosts.CodeNotLocal, "none of the shared accounts is logged in")
	case errors.As(err, &pe):
		return hosts.ChooseAnswer{}, hosts.Errorf(hosts.CodePersonal, "%s", err.Error())
	case errors.As(err, &nl):
		return hosts.ChooseAnswer{}, hosts.Errorf(hosts.CodeNotLocal, "%s", err.Error())
	case errors.Is(err, config.ErrNotSetup):
		return hosts.ChooseAnswer{}, hosts.Errorf(hosts.CodeNotSetUp, "julienning is not set up")
	case err != nil:
		return hosts.ChooseAnswer{}, err
	}
	return hosts.ChooseAnswer{Config: c.Config.Name, Dir: c.Config.Dir, Email: c.Email, Nickname: c.Nickname, Account: c.Account, Warnings: c.Warnings}, nil
}

// packSourceOf is where s is on this machine, for the archive header.
func packSourceOf(s sessions.Session, home string) sessions.PackSource {
	src := sessions.PackSource{Home: home, Cwd: s.Cwd}
	if s.Cwd == "" {
		return src
	}
	p := hosts.ProjectOf(s.Cwd, home)
	src.Checkout, src.GitRoot = p.Checkout, p.GitRoot
	if p.Checkout != "" {
		if real, err := filepath.EvalSymlinks(p.Checkout); err == nil {
			src.CheckoutReal = real
		}
	}
	return src
}

// countingWriter remembers whether anything was written.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// agentExport streams a session: the frame line, the header line, the
// archive. A refusal before the first byte of the header is a framed error;
// a failure later cuts the stream, which the receiver notices (no manifest).
func agentExport(env cli.Env, req hosts.ExportRequest) error {
	fail := func(err error) error {
		_, _ = env.Stdout.Write(hosts.EncodeAnswer(moveErr(err)))
		return cli.Exit(1)
	}
	_, cd, err := registered(req.ConfigDir)
	if err != nil {
		return fail(err)
	}
	s, err := sessions.FindSession(cd, req.ID)
	if err != nil {
		return fail(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintln(env.Stdout, hosts.Frame()); err != nil {
		return err
	}
	cw := &countingWriter{w: env.Stdout}
	if _, _, err := sessions.Pack(s, packSourceOf(s, home), cw); err != nil {
		if cw.n == 0 {
			raw, _ := json.Marshal(map[string]any{"error": moveErr(err)})
			_, _ = env.Stdout.Write(append(raw, '\n'))
		} else {
			fmt.Fprintf(env.Stderr, "julienning: %v\n", err)
		}
		return cli.Exit(1)
	}
	return nil
}

// agentReceive takes the controller's request line, then the export's
// header line and archive (sessions.Unpack).
func agentReceive(env cli.Env) error {
	br := bufio.NewReaderSize(env.Stdin, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return answer(env, nil, hosts.Errorf(hosts.CodeInvalid, "no receive request: %v", err))
	}
	var req hosts.ReceiveRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return answer(env, nil, hosts.Errorf(hosts.CodeInvalid, "the receive request is not JSON: %v", err))
	}
	if _, _, err := registered(req.ConfigDir); err != nil {
		return answer(env, nil, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return answer(env, nil, err)
	}
	received, err := config.Path(hosts.ReceivedDir)
	if err != nil {
		return answer(env, nil, err)
	}
	res, err := sessions.Unpack(br, sessions.UnpackOptions{
		Dir: req.ConfigDir, Email: req.Email, Cwd: req.Cwd, Checkout: req.Checkout, GitRoot: req.GitRoot,
		Home: home, ReceivedDir: received, From: req.From, Now: time.Now(),
	})
	if err != nil {
		return answer(env, nil, moveErr(err))
	}
	return answer(env, res, nil)
}

func agentRelease(req hosts.ReleaseRequest) (hosts.ReleaseAnswer, error) {
	_, cd, err := registered(req.ConfigDir)
	if err != nil {
		return hosts.ReleaseAnswer{}, err
	}
	s, err := sessions.FindSession(cd, req.ID)
	if err != nil {
		return hosts.ReleaseAnswer{}, moveErr(err)
	}
	warnings, err := sessions.Release(s, sessions.PackSource{}, req.ManifestSHA256)
	if err != nil {
		return hosts.ReleaseAnswer{}, moveErr(err)
	}
	return hosts.ReleaseAnswer{Released: true, Warnings: warnings}, nil
}

// agentLaunch resumes a moved session here: claude on Dir's account from
// Cwd, in this machine's tmux mode (launch.Exec), on the controller's
// terminal through ssh -t.
func agentLaunch(env cli.Env, arg string) error {
	var req hosts.LaunchRequest
	fail := func(format string, a ...any) error {
		name := req.Host
		if name == "" {
			name = "host"
		}
		fmt.Fprintf(env.Stderr, "julienning: %s: %s\n", name, fmt.Sprintf(format, a...))
		return cli.Exit(1)
	}
	if err := hosts.DecodeArg(arg, &req); err != nil {
		return fail("%v", err)
	}
	cfg, cd, err := registered(req.Dir)
	if err != nil {
		return fail("%v", err)
	}
	if req.MachineID != cfg.MachineID {
		return fail("this is machine %q, not %q; refusing to start claude for another machine", cfg.MachineID, req.MachineID)
	}
	if err := execClaude(cd.Dir, req.Args, req.Cwd); err != nil {
		return fail("%v", err)
	}
	return nil
}

// execClaude replaces the process with claude (a var for tests).
var execClaude = launch.Exec
