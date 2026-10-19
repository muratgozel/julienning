package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/paths"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sharedcache"
	"github.com/muratgozel/julienning/internal/tui"
	"github.com/muratgozel/julienning/internal/version"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "agent",
		Summary: "answer another machine's julienning over ssh (see julienning host)",
		Usage:   "agent hello|bootstrap|inventory|run REQUEST|install-self|locate|sessions|choose|export|receive|release|launch REQUEST",
		Hidden:  true,
		Run:     runAgent,
	})
}

// maxRequest bounds a request read from stdin.
const maxRequest = 1 << 20

// runAgent is the host end of every cross-machine command (SPEC "Agent").
// Answers go to stdout after the frame line; an error is the framed error
// body and exit 1, never a plain stderr line the controller cannot parse.
// It never writes an email to errors.log.
func runAgent(env cli.Env) error {
	if len(env.Args) == 0 {
		return cli.Usagef("expected an agent verb")
	}
	verb, args := env.Args[0], env.Args[1:]
	switch verb {
	case "hello":
		return answer(env, agentHello(), nil)
	case "bootstrap":
		var req hosts.BootstrapRequest
		if err := readRequest(env.Stdin, &req); err != nil {
			return answer(env, nil, err)
		}
		res, err := agentBootstrap(req)
		return answer(env, res, err)
	case "inventory":
		inv, err := agentInventory()
		return answer(env, inv, err)
	case "run":
		if len(args) != 1 {
			return cli.Usagef("agent run takes one request argument")
		}
		return agentRun(env, args[0])
	case "install-self":
		res, err := agentInstallSelf()
		return answer(env, res, err)
	case "locate":
		var req hosts.LocateRequest
		if err := readRequest(env.Stdin, &req); err != nil {
			return answer(env, nil, err)
		}
		res, err := agentLocate(req)
		return answer(env, res, err)
	case "sessions":
		var req hosts.SessionsRequest
		if err := readRequest(env.Stdin, &req); err != nil {
			return answer(env, nil, err)
		}
		res, err := agentSessions(req)
		return answer(env, res, err)
	case "choose":
		var req hosts.ChooseRequest
		if err := readRequest(env.Stdin, &req); err != nil {
			return answer(env, nil, err)
		}
		res, err := agentChoose(req)
		return answer(env, res, err)
	case "export":
		var req hosts.ExportRequest
		if err := readRequest(env.Stdin, &req); err != nil {
			return answer(env, nil, err)
		}
		return agentExport(env, req)
	case "receive":
		return agentReceive(env)
	case "release":
		var req hosts.ReleaseRequest
		if err := readRequest(env.Stdin, &req); err != nil {
			return answer(env, nil, err)
		}
		res, err := agentRelease(req)
		return answer(env, res, err)
	case "launch":
		if len(args) != 1 {
			return cli.Usagef("agent launch takes one request argument")
		}
		return agentLaunch(env, args[0])
	}
	return answer(env, nil, hosts.Errorf(hosts.CodeProto, "julienning %s (agent protocol %d) does not know the agent verb %q", version.Version, hosts.Proto, verb))
}

// answer writes v, or err as the framed error body (exit 1).
func answer(env cli.Env, v any, err error) error {
	if err != nil {
		var ae *hosts.AgentError
		if !errors.As(err, &ae) {
			ae = hosts.Errorf(hosts.CodeInvalid, "%s", err.Error())
		}
		_, _ = env.Stdout.Write(hosts.EncodeAnswer(ae))
		return cli.Exit(1)
	}
	_, werr := env.Stdout.Write(hosts.EncodeAnswer(v))
	return werr
}

func readRequest(r io.Reader, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r, maxRequest))
	if err != nil {
		return hosts.Errorf(hosts.CodeInvalid, "read the request: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return hosts.Errorf(hosts.CodeInvalid, "the request is not valid JSON: %v", err)
	}
	return nil
}

// --- hello ---

// claudeVersionOf reads the version of the claude at path (a var for tests).
var claudeVersionOf = claudeVersion

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// claudeVersion reads it from Claude Code's native layout (the claude
// command links to …/claude/versions/<version>), else from `claude
// --version`, bounded to 5 s.
func claudeVersion(path string) *string {
	if target, err := filepath.EvalSymlinks(path); err == nil && filepath.Base(filepath.Dir(target)) == "versions" {
		if v := filepath.Base(target); versionRe.MatchString(v) {
			return &v
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	if v := versionRe.FindString(string(out)); v != "" {
		return &v
	}
	return nil
}

// Tool lookups of the agent (vars so tests never find this machine's real
// claude or tmux).
var (
	findClaude = launch.FindClaude
	findTmux   = launch.FindTmux
)

// ownPaths are this machine's julienning, claude and tmux as the agent
// finds them: the stable command (else the running binary), then PATH and
// the usual install dirs.
func ownPaths() (hosts.HelloPaths, *launch.Tmux) {
	var p hosts.HelloPaths
	if exe, _, err := paths.StableCommand(); err == nil {
		p.Julienning = exe
	}
	if c, err := findClaude(); err == nil {
		p.Claude = &c
	}
	var tmux *launch.Tmux
	if t, err := findTmux(); err == nil {
		p.Tmux = &t.Path
		tmux = &t
	}
	return p, tmux
}

func agentHello() hosts.Hello {
	h := hosts.Hello{Proto: hosts.Proto, Version: version.Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if home, err := os.UserHomeDir(); err == nil {
		h.Home = home
	}
	if _, _, err := selfupdate.ActiveLink(); err == nil {
		h.Managed = true
	}
	var tmux *launch.Tmux
	h.Paths, tmux = ownPaths()
	if tmux != nil {
		h.TmuxVersion = &tmux.Version
	}
	if h.Paths.Claude != nil {
		h.ClaudeVersion = claudeVersionOf(*h.Paths.Claude)
	}
	cfg, err := config.Load()
	switch {
	case errors.Is(err, config.ErrNotSetup):
		return h
	case err != nil:
		h.SetupWarning = err.Error()
		return h
	}
	h.SetUp = true
	h.MachineID, h.Dev = &cfg.MachineID, &cfg.Dev
	if cfg.Remote.URL != "" {
		h.WorkerURL = &cfg.Remote.URL
	}
	if cfg.Tmux != nil {
		h.Tmux = &hosts.TmuxMode{Enabled: cfg.Tmux.Enabled, Session: cfg.Tmux.Session}
	}
	switch {
	case cfg.RequireRemote() != nil:
		h.SetupWarning = "no Worker is configured"
	default:
		h.SetupWarning = claudecfg.SetupWarning(cfg.Configs)
	}
	return h
}

// --- bootstrap ---

// Worker access from the agent (vars for tests).
var (
	agentClient = func(url, token string) remote.Client { return remote.NewHTTP(url, token, workerTimeout) }
	agentHealth = func(ctx context.Context, base string) error { return remote.Healthz(ctx, base, workerTimeout) }
	agentNow    = time.Now
)

const workerTimeout = 10 * time.Second

var devRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// checkWorker proves url and token from this machine: /healthz, then an
// authenticated /accounts, whose listing is the allowlist.
func checkWorker(url, token, dev string) (*remote.Listing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*workerTimeout)
	defer cancel()
	if err := agentHealth(ctx, url); err != nil {
		return nil, err
	}
	return agentClient(url, token).ListAccounts(ctx, dev)
}

// unreachable is the error for a Worker this host cannot use. The
// controller words it as "box cannot reach the Worker (…)".
func unreachable(err error) *hosts.AgentError {
	if remote.IsAuth(err) {
		return hosts.Errorf(hosts.CodeWorkerUnreachable, "the Worker rejects the team token")
	}
	return hosts.Errorf(hosts.CodeWorkerUnreachable, "%v", err)
}

// agentBootstrap gives this machine the controller's team settings (SPEC
// "host add" step 4). The machine id is never regenerated: claims refer to
// it. Nothing is saved unless the Worker answers from here.
func agentBootstrap(req hosts.BootstrapRequest) (hosts.BootstrapResult, error) {
	var res hosts.BootstrapResult
	req.WorkerURL = strings.TrimRight(strings.TrimSpace(req.WorkerURL), "/")
	switch {
	case !devRe.MatchString(req.Dev):
		return res, hosts.Errorf(hosts.CodeInvalid, "invalid dev name %q", req.Dev)
	case !strings.HasPrefix(req.WorkerURL, "https://") && !strings.HasPrefix(req.WorkerURL, "http://"):
		return res, hosts.Errorf(hosts.CodeInvalid, "invalid Worker URL %q", req.WorkerURL)
	case req.Token == "":
		return res, hosts.Errorf(hosts.CodeInvalid, "no team token was sent")
	}
	cfg, err := config.Load()
	switch {
	case errors.Is(err, config.ErrNotSetup):
		listing, err := checkWorker(req.WorkerURL, req.Token, req.Dev)
		if err != nil {
			return res, unreachable(err)
		}
		cfg, err := config.New(req.Dev)
		if err != nil {
			return res, err
		}
		cfg.Remote = config.Remote{URL: req.WorkerURL, Token: req.Token}
		if err := cfg.Save(); err != nil {
			return res, err
		}
		if _, err := sharedcache.FromListing(listing, agentNow()); err != nil {
			return res, fmt.Errorf("saved the team settings, but not the allowlist: %w", err)
		}
		res.Created = true
		return res, nil
	case err != nil:
		return res, err
	}

	if strings.TrimRight(cfg.Remote.URL, "/") != req.WorkerURL {
		if !req.ReplaceWorker {
			return res, hosts.Errorf(hosts.CodeWorkerMismatch, "set up for another Worker (%s)", cfg.Remote.URL)
		}
		listing, err := checkWorker(req.WorkerURL, req.Token, cfg.Dev)
		if err != nil {
			return res, unreachable(err)
		}
		cfg.Remote = config.Remote{URL: req.WorkerURL, Token: req.Token}
		if err := cfg.Save(); err != nil {
			return res, err
		}
		if _, err := sharedcache.FromListing(listing, agentNow()); err != nil {
			return res, fmt.Errorf("switched the Worker, but could not save its allowlist: %w", err)
		}
		res.WorkerReplaced = true
		return res, nil
	}

	_, err = checkWorker(cfg.Remote.URL, cfg.Remote.Token, cfg.Dev)
	switch {
	case err == nil:
		return res, nil
	case !remote.IsAuth(err):
		res.WorkerError = err.Error()
		return res, nil
	case !req.ReplaceToken:
		res.TokenRejected = true
		return res, nil
	}
	if _, err := checkWorker(cfg.Remote.URL, req.Token, cfg.Dev); err != nil {
		return res, unreachable(err)
	}
	cfg.Remote.Token = req.Token
	if err := cfg.Save(); err != nil {
		return res, err
	}
	res.TokenUpdated = true
	return res, nil
}

// --- inventory ---

func agentInventory() (hosts.Inventory, error) {
	inv := hosts.Inventory{Configs: []hosts.InventoryConfig{}}
	cfg, err := config.Load()
	if errors.Is(err, config.ErrNotSetup) {
		return inv, hosts.Errorf(hosts.CodeNotSetUp, "julienning is not set up")
	}
	if err != nil {
		return inv, err
	}
	cache, err := sharedcache.Load()
	if err != nil {
		cache = &sharedcache.Cache{}
	}
	cur, curOK, _ := cfg.Current()
	for _, cd := range cfg.Configs {
		c := hosts.InventoryConfig{Name: cd.Name, Dir: cd.Dir, Current: curOK && cur.Dir == cd.Dir}
		if email, err := claudecfg.ReadEmail(cd.Dir); err == nil {
			c.Email = email
			c.Shared = cache.Contains(email)
			if c.Shared {
				c.Nickname = cache.Nickname(email)
			}
		}
		inv.Configs = append(inv.Configs, c)
	}
	return inv, nil
}

// --- run ---

// strictCommands fail without a checkout of the controller's working
// directory; the other commands that depend on it run in the home.
var strictCommands = map[string]bool{"pin": true, "unpin": true}

// execSelf replaces the process with julienning ARGV (a var for tests).
var execSelf = func(argv []string, env []string) error {
	exe, err := paths.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, append([]string{"julienning"}, argv...), env)
}

// agentRun is a forwarded command (SPEC "--host"): check the machine id,
// change into the checkout of the controller's working directory (or the
// home), put this machine's julienning, claude and tmux first on PATH, set
// the controller's display zone and NO_COLOR, and become `julienning ARGV`.
// Its output is the command's, so problems before the exec are printed as
// "julienning: box: …" (box being the controller's name for this host).
func agentRun(env cli.Env, arg string) error {
	var req hosts.RunRequest
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
	if len(req.Argv) == 0 {
		return fail("the request names no command")
	}
	own := ""
	cfg, err := config.Load()
	switch {
	case err == nil:
		own = cfg.MachineID
	case !errors.Is(err, config.ErrNotSetup):
		return fail("%v", err)
	}
	if req.MachineID != own {
		return fail("this is machine %q, not %q; refusing to run a command meant for another machine (run: julienning host add %s)", own, req.MachineID, req.Host)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("cannot find the home directory: %v", err)
	}
	dir := home
	if req.Project != nil {
		var dirs []string
		if cfg != nil {
			for _, cd := range cfg.Configs {
				dirs = append(dirs, cd.Dir)
			}
		}
		var pick func([]string) (int, error)
		if isTerminal(env.Stdin) && isTerminal(env.Stdout) {
			pick = pickCheckout(env)
		}
		machine := req.Host
		if machine == "" {
			machine = "this machine"
		}
		loc, err := hosts.Locate(req.Project, hosts.LocateOptions{
			Home: home, ConfigDirs: dirs, Branch: req.Project.Branch, Machine: machine,
			RanIn: "the command was typed in " + controllerPath(req.Project) + " on the other machine", Pick: pick,
		})
		var nf *hosts.NotFound
		switch {
		case err == nil:
			dir = loc.Cwd
		case errors.Is(err, tui.ErrCanceled):
			return cli.Exit(1)
		case strictCommands[req.Argv[0]]:
			return fail("%v", err)
		case errors.As(err, &nf):
			fmt.Fprintf(env.Stderr, "julienning: note: %s has no checkout on %s (%s); running in ~ there\n", controllerPath(req.Project), machine, nf.Detail())
		default:
			fmt.Fprintf(env.Stderr, "julienning: note: %v; running in ~ there\n", err)
		}
	}
	if err := os.Chdir(dir); err != nil {
		return fail("change directory to %s: %v", dir, err)
	}
	environ := runEnv(os.Environ(), req, dir)
	if err := execSelf(req.Argv, environ); err != nil {
		return fail("run julienning: %v", err)
	}
	return nil
}

// runEnv is the forwarded command's environment: this machine's tool dirs
// first on PATH (ssh's non-interactive PATH lacks ~/.local/bin), PWD for the
// directory it runs in, the controller's zone and NO_COLOR.
func runEnv(environ []string, req hosts.RunRequest, dir string) []string {
	p, _ := ownPaths()
	var first []string
	for _, tool := range []*string{&p.Julienning, p.Claude, p.Tmux} {
		if tool != nil && *tool != "" {
			if d := filepath.Dir(*tool); !containsStr(first, d) {
				first = append(first, d)
			}
		}
	}
	// The controller's zone replaces this machine's only when it names a
	// zone the embedded tzdata knows.
	tz := ""
	if req.TZ != "" {
		if _, err := time.LoadLocation(req.TZ); err == nil {
			tz = req.TZ
		}
	}
	out := make([]string, 0, len(environ)+4)
	path := os.Getenv("PATH")
	for _, kv := range environ {
		switch {
		case strings.HasPrefix(kv, "PATH="), strings.HasPrefix(kv, "PWD="):
		case strings.HasPrefix(kv, "TZ=") && tz != "":
		case strings.HasPrefix(kv, "NO_COLOR=") && req.NoColor:
		default:
			out = append(out, kv)
		}
	}
	if len(first) > 0 {
		path = strings.Join(first, string(os.PathListSeparator)) + string(os.PathListSeparator) + path
	}
	out = append(out, "PATH="+path, "PWD="+dir)
	if tz != "" {
		out = append(out, "TZ="+tz)
	}
	if req.NoColor {
		out = append(out, "NO_COLOR=1")
	}
	return out
}

// controllerPath renders the controller's directory in its own terms.
func controllerPath(p *hosts.Project) string {
	if p.InHome {
		if p.HomeRel == "" {
			return "~"
		}
		return "~/" + p.HomeRel
	}
	return p.Cwd
}

func pickCheckout(env cli.Env) func([]string) (int, error) {
	return func(checkouts []string) (int, error) {
		items := make([]tui.Item, len(checkouts))
		for i, c := range checkouts {
			items[i] = tui.Item{Title: c}
		}
		return tui.Pick(items, tui.Options{Header: "Several checkouts of this project here; run the command in:", Choice: true, In: env.Stdin, Out: env.Stdout})
	}
}

// --- install-self ---

// agentInstallSelf installs the running binary (a temporary copy streamed
// here by host add's fallback) into the installer layout.
func agentInstallSelf() (hosts.InstallSelfResult, error) {
	exe, err := paths.Executable()
	if err != nil {
		return hosts.InstallSelfResult{}, err
	}
	ver := selfupdate.DisplayVersion(version.Version)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	link, err := selfupdate.InstallSelf(ctx, exe, ver)
	if err != nil {
		return hosts.InstallSelfResult{}, err
	}
	return hosts.InstallSelfResult{Version: ver, Link: link}, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
