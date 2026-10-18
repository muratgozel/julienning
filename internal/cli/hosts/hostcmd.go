package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/selfupdate"
	"github.com/muratgozel/julienning/internal/sshx"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "host",
		Summary: "add, list or remove the other machines julienning reaches over ssh",
		Usage:   "host add NAME [SSH_DEST] [--yes] | host list [--json] | host remove NAME",
		Host:    cli.HostLocal,
		Run:     runHost,
	})
	cli.Forward = forward
}

func runHost(env cli.Env) error {
	if len(env.Args) == 0 {
		return cli.Usagef("expected add, list or remove")
	}
	sub, args := env.Args[0], env.Args[1:]
	switch sub {
	case "add":
		return hostAdd(env, args)
	case "list":
		return hostList(env, args)
	case "remove":
		return hostRemove(env, args)
	}
	return cli.Usagef("unknown host command %q (expected add, list or remove)", sub)
}

// parseArgs parses flags anywhere among the positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, cli.Usagef("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// --- host add ---

// hostAdd is SPEC "host add": idempotent, one status line per step.
func hostAdd(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("host add", env)
	yes := fs.Bool("yes", false, "ask nothing: take the questions that default to yes, refuse the others")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || len(pos) > 2 {
		return cli.Usagef("expected NAME [SSH_DEST]")
	}
	name, dest := pos[0], pos[0]
	if len(pos) == 2 {
		dest = pos[1]
	}
	if !hosts.ValidName(name) {
		return cli.Usagef("invalid host name %q: %s", name, hosts.NameRule)
	}
	if err := hosts.ValidSSH(dest); err != nil {
		return cli.Usagef("%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireRemote(); err != nil {
		return fmt.Errorf("this machine has no Worker to share with %s: %w", name, err)
	}
	a := newAsker(env, *yes)
	ctx := context.Background()

	// 1. Entry.
	hf, err := hosts.Load()
	if err != nil {
		return err
	}
	existing, known := hf.Find(name)
	if known && existing.SSH != dest {
		q := fmt.Sprintf("%s is %s now; point it at %s instead?", name, existing.SSH, dest)
		if err := a.ask(q, false, "remove it first: julienning host remove "+name); err != nil {
			if errors.Is(err, errDeclined) {
				return fmt.Errorf("kept %s pointing at %s; nothing changed", name, existing.SSH)
			}
			return err
		}
	}

	// 2. Probe.
	t := sshx.Target{Name: name, SSH: dest}
	p, err := probe(ctx, t)
	if err != nil {
		return addReachHint(err)
	}
	goos, goarch, err := p.Platform(name)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("Host:     %s → %s", name, dest)
	if p.Server != "" && p.Server != dest {
		line += " (" + p.Server + ")"
	}
	line += ": "
	if p.OSName != "" {
		line += p.OSName + ", "
	}
	line += fmt.Sprintf("%s/%s, user %s, home %s", goos, goarch, p.User, p.Home)
	fmt.Fprintln(env.Stdout, line)

	// 3. julienning there.
	hello, err := julienningOn(ctx, env, a, &t, p, goos, goarch)
	if err != nil {
		return err
	}
	entry := hosts.Host{Name: name, SSH: dest, Home: hello.Home, OS: goos, Arch: goarch, Paths: hosts.Paths{Julienning: t.Julienning}}
	if known {
		entry.AddedAt = existing.AddedAt
	}

	// 4. Team settings.
	if err := checkMachineID(a, hf, name, dest, existing, known, hello.ID()); err != nil {
		return err
	}
	created, err := bootstrap(ctx, env, a, cfg, t, hello)
	if err != nil {
		return err
	}
	if hello, err = callHello(ctx, t); err != nil {
		return err
	}
	if created {
		if err := checkMachineID(a, hf, name, dest, existing, known, hello.ID()); err != nil {
			return err
		}
	}
	entry.MachineID = hello.ID()
	entry.Paths.Claude, entry.Paths.Tmux = hello.Paths.Claude, hello.Paths.Tmux
	if entry.Paths.Claude == nil && p.Paths["claude"] != "" {
		c := p.Paths["claude"]
		entry.Paths.Claude = &c
	}
	if entry.Paths.Tmux == nil && p.Paths["tmux"] != "" {
		tm := p.Paths["tmux"]
		entry.Paths.Tmux = &tm
	}
	if !created && hello.Dev != nil && *hello.Dev != cfg.Dev {
		q := fmt.Sprintf("%s knows you as %s. Rename it to %s?", name, *hello.Dev, cfg.Dev)
		switch err := a.ask(q, true, ""); {
		case err == nil:
			code, err := forwardTo(ctx, env, entry, hello, []string{"dev", cfg.Dev}, nil)
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("renaming you on %s failed (see above); retry with: julienning dev %s --host %s", name, cfg.Dev, name)
			}
		case errors.Is(err, errDeclined):
			fmt.Fprintf(env.Stdout, "Note:     %s keeps knowing you as %s (rename it with: julienning dev %s --host %s)\n", name, *hello.Dev, cfg.Dev, name)
		default:
			// No terminal: nothing depends on the dev name.
			fmt.Fprintf(env.Stdout, "Note:     %s knows you as %s (rename it with: julienning dev %s --host %s)\n", name, *hello.Dev, cfg.Dev, name)
		}
	}

	// 5. Setup there.
	setupFailed := false
	if created || hello.SetupWarning != "" || (hello.Tmux == nil && hello.TmuxVersion != nil) {
		argv := []string{"setup"}
		if *yes || !isInteractive(env) {
			argv = append(argv, "--yes")
		}
		fmt.Fprintf(env.Stdout, "Setup:    running julienning setup on %s\n", name)
		code, err := forwardTo(ctx, env, entry, hello, argv, nil)
		if err != nil {
			return err
		}
		setupFailed = code != 0
	} else {
		fmt.Fprintf(env.Stdout, "Setup:    complete on %s (change it with: julienning setup --host %s)\n", name, name)
	}

	// 6. Save.
	n := now().UTC().Truncate(time.Second)
	if entry.AddedAt.IsZero() {
		entry.AddedAt = n
	}
	entry.CheckedAt = n
	fresh, err := hosts.Load()
	if err != nil {
		return err
	}
	fresh.Put(entry)
	if err := fresh.Save(); err != nil {
		return err
	}
	verb := "Added"
	if known {
		verb = "Refreshed"
	}
	fmt.Fprintf(env.Stdout, "%s %s (%s).\n", verb, name, dest)
	fmt.Fprintf(env.Stdout, "Next: julienning use --host %s, or julienning transfer --to %s\n", name, name)
	if setupFailed {
		return fmt.Errorf("setup on %s did not finish (see above); fix it and run: julienning setup --host %s", name, name)
	}
	return nil
}

// addReachHint adds what to run when the probe could not get in: BatchMode
// never answers a password or host-key prompt.
func addReachHint(err error) error {
	var ue *sshx.UnreachableError
	if !errors.As(err, &ue) {
		return err
	}
	switch d := strings.ToLower(ue.Detail); {
	case strings.Contains(d, "host key"):
		return fmt.Errorf("%v; its host key is not known yet: run `ssh %s` once by hand to accept it", err, ue.Target.SSH)
	case strings.Contains(d, "permission denied"):
		return fmt.Errorf("%v; key-based login is needed: add a key with `ssh-copy-id %s`", err, ue.Target.SSH)
	}
	return err
}

func callHello(ctx context.Context, t sshx.Target) (*hosts.Hello, error) {
	var hello hosts.Hello
	if err := agentCall(ctx, t, "hello", nil, &hello); err != nil {
		return nil, hostErr(t.Name, err)
	}
	return &hello, nil
}

// julienningOn is host add step 3: julienning there, installed (or
// updated to this machine's version) when needed. It sets t.Julienning.
func julienningOn(ctx context.Context, env cli.Env, a *asker, t *sshx.Target, p hosts.Probe, goos, goarch string) (*hosts.Hello, error) {
	path := p.Paths["julienning"]
	install := path == ""
	var hello *hosts.Hello
	if !install {
		if strings.ContainsAny(path, `'\`) {
			return nil, fmt.Errorf("julienning on %s is at %q, a path with a quote or backslash this machine cannot call; move it", t.Name, path)
		}
		t.Julienning = path
		var err error
		hello, err = callHello(ctx, *t)
		var na *sshx.NoAgentError
		switch {
		case errors.As(err, &na):
			install = true // too old to have the agent
		case err != nil:
			return nil, err
		}
	}
	if install {
		tag, err := installTag(ctx, localVersion())
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(env.Stdout, "Julienning: installing %s on %s…\n", selfupdate.DisplayVersion(tag), t.Name)
		if t.Julienning, err = installOnHost(ctx, env, *t, p, goos, goarch, tag); err != nil {
			return nil, err
		}
		if hello, err = callHello(ctx, *t); err != nil {
			return nil, err
		}
		usePath(t, hello)
		fmt.Fprintf(env.Stdout, "Julienning: %s at %s (installed)\n", hello.Version, homeShort(t.Julienning, hello.Home))
		return hello, nil
	}
	usePath(t, hello)
	if hello.Managed {
		fmt.Fprintf(env.Stdout, "Julienning: %s at %s\n", hello.Version, homeShort(t.Julienning, hello.Home))
	} else {
		fmt.Fprintf(env.Stdout, "Julienning: %s at %s (not from the installer: auto-update and update --host leave it alone)\n", hello.Version, homeShort(t.Julienning, hello.Home))
	}
	tmp := hosts.Host{Name: t.Name, SSH: t.SSH, Paths: hosts.Paths{Julienning: t.Julienning}}
	again, err := checkVersions(ctx, env, a, tmp, hello, versionsStrict)
	if err != nil {
		return nil, err
	}
	usePath(t, again)
	return again, nil
}

// usePath prefers the stable command the agent reports (the installer's
// link survives updates; the probe may have found a version file).
func usePath(t *sshx.Target, hello *hosts.Hello) {
	if p := hello.Paths.Julienning; p != "" && !strings.ContainsAny(p, `'\`) && strings.HasPrefix(p, "/") {
		t.Julienning = p
	}
}

// checkMachineID refuses a machine already known under another name, and
// asks before an entry takes a changed machine id.
func checkMachineID(a *asker, hf *hosts.Hosts, name, dest string, existing hosts.Host, known bool, id string) error {
	if id == "" {
		return nil
	}
	if other, ok := hf.ByMachineID(id); ok && other.Name != name {
		return fmt.Errorf("%s is the same machine as %s; remove that entry first: julienning host remove %s", dest, other.Name, other.Name)
	}
	if known && existing.MachineID != id {
		q := fmt.Sprintf("%s now answers as machine %s (was %s): rebuilt, or %s reaches another machine now. Replace the entry?", name, id, existing.MachineID, dest)
		if err := a.ask(q, false, "remove it first: julienning host remove "+name); err != nil {
			if errors.Is(err, errDeclined) {
				return fmt.Errorf("kept the entry for %s as it was; nothing changed", name)
			}
			return err
		}
	}
	return nil
}

// bootstrap is host add step 4: this machine's dev name, Worker URL and
// token, sent on ssh's stdin, never on a command line.
func bootstrap(ctx context.Context, env cli.Env, a *asker, cfg *config.Config, t sshx.Target, hello *hosts.Hello) (created bool, err error) {
	req := hosts.BootstrapRequest{Dev: cfg.Dev, WorkerURL: cfg.Remote.URL, Token: cfg.Remote.Token}
	if hello.SetUp && hello.WorkerURL != nil && strings.TrimRight(*hello.WorkerURL, "/") != strings.TrimRight(cfg.Remote.URL, "/") {
		q := fmt.Sprintf("%s is set up for another Worker (%s); hosts must use this machine's. Switch %s to it? Its shared accounts and claims stay with the other Worker.", t.Name, *hello.WorkerURL, t.Name)
		if err := a.ask(q, false, ""); err != nil {
			if errors.Is(err, errDeclined) {
				return false, fmt.Errorf("%s keeps its Worker (%s); hosts must use this machine's, so it was not added", t.Name, *hello.WorkerURL)
			}
			return false, err
		}
		req.ReplaceWorker = true
	}
	var res hosts.BootstrapResult
	if err := agentCall(ctx, t, "bootstrap", req, &res); err != nil {
		return false, bootstrapErr(t.Name, err)
	}
	if res.TokenRejected {
		q := fmt.Sprintf("The Worker rejects %s's team token. Send this machine's?", t.Name)
		switch err := a.ask(q, true, ""); {
		case err == nil:
			req.ReplaceToken = true
			if err := agentCall(ctx, t, "bootstrap", req, &res); err != nil {
				return false, bootstrapErr(t.Name, err)
			}
		case errors.Is(err, errDeclined):
			warnf(env, "%s keeps a team token the Worker rejects; its hooks cannot report until it has the current one", t.Name)
		default:
			return false, err
		}
	}
	switch {
	case res.Created:
		fmt.Fprintf(env.Stdout, "Team:     set up on %s (dev %s, Worker %s)\n", t.Name, cfg.Dev, cfg.Remote.URL)
	case res.WorkerReplaced:
		fmt.Fprintf(env.Stdout, "Team:     %s now uses this machine's Worker (%s)\n", t.Name, cfg.Remote.URL)
	case res.TokenUpdated:
		fmt.Fprintf(env.Stdout, "Team:     sent this machine's team token to %s\n", t.Name)
	default:
		fmt.Fprintf(env.Stdout, "Team:     unchanged on %s\n", t.Name)
	}
	if res.WorkerError != "" {
		warnf(env, "%s could not check its team token with the Worker (%s)", t.Name, res.WorkerError)
	}
	return res.Created, nil
}

func bootstrapErr(name string, err error) error {
	var ae *hosts.AgentError
	if errors.As(err, &ae) && ae.Code == hosts.CodeWorkerUnreachable {
		return fmt.Errorf("%s cannot reach the Worker (%s); its hooks report usage and claims from there", name, ae.Message)
	}
	return hostErr(name, err)
}

// --- host list ---

// listTimeout bounds each call of host list: it never prompts and never
// waits long on an unreachable host.
const listTimeout = 10 * time.Second

type listAccount struct {
	Email    string `json:"email"`
	Nickname string `json:"nickname"`
	Config   string `json:"config"`
	Current  bool   `json:"current"`
}

type listRow struct {
	Name       string          `json:"name"`
	SSH        string          `json:"ssh"`
	MachineID  string          `json:"machine_id"`
	OS         string          `json:"os"`
	Arch       string          `json:"arch"`
	Julienning *string         `json:"julienning"`
	Claude     *string         `json:"claude"`
	Tmux       *string         `json:"tmux"`
	TmuxMode   *hosts.TmuxMode `json:"tmux_mode"`
	Accounts   []listAccount   `json:"accounts"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	personal   int             // personal accounts logged in there
	reached    bool            // the host answered
	inventory  []hosts.InventoryConfig
}

// listCall is one call of host list (a var for tests).
var listCall = func(ctx context.Context, t sshx.Target, verb string, resp any) error {
	return sshx.Agent(ctx, t, verb, nil, resp, listTimeout)
}

func hostList(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("host list", env)
	asJSON := fs.Bool("json", false, "print {\"hosts\": [...]}")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return cli.Usagef("host list takes no arguments")
	}
	hf, err := hosts.Load()
	if err != nil {
		return err
	}
	if len(hf.Hosts) == 0 {
		if *asJSON {
			fmt.Fprintln(env.Stdout, `{"hosts": []}`)
			return nil
		}
		fmt.Fprintln(env.Stdout, "No hosts. Add one with: julienning host add NAME [SSH_DEST]")
		return nil
	}
	rows := make([]listRow, len(hf.Hosts))
	var wg sync.WaitGroup
	for i, h := range hf.Hosts {
		wg.Add(1)
		go func(i int, h hosts.Host) {
			defer wg.Done()
			rows[i] = listOne(h)
		}(i, h)
	}
	wg.Wait()

	// Record when each answering host was checked; a failed save only
	// costs that timestamp.
	if fresh, err := hosts.Load(); err == nil {
		n := now().UTC().Truncate(time.Second)
		for _, r := range rows {
			if h, ok := fresh.Find(r.Name); ok && r.reached {
				h.CheckedAt = n
				fresh.Put(h)
			}
		}
		if err := fresh.Save(); err != nil {
			warnf(env, "could not record when the hosts were checked: %v", err)
		}
	}

	if *asJSON {
		raw, err := json.MarshalIndent(map[string]any{"hosts": rows}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Stdout, string(raw))
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tSSH\tOS\tJULIENNING\tCLAUDE\tTMUX\tACCOUNTS\tSTATUS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.SSH, r.OS, r.Arch,
			deref(r.Julienning), deref(r.Claude), tmuxCell(r), accountsCell(r), r.Status)
	}
	return tw.Flush()
}

func listOne(h hosts.Host) listRow {
	r := listRow{Name: h.Name, SSH: h.SSH, MachineID: h.MachineID, OS: h.OS, Arch: h.Arch, Accounts: []listAccount{}}
	t := sshx.TargetOf(h)
	var hello hosts.Hello
	if err := listCall(context.Background(), t, "hello", &hello); err != nil {
		var ue *sshx.UnreachableError
		switch {
		case errors.As(err, &ue):
			r.Status = "unreachable (" + orText(ue.Detail, "ssh failed") + ")"
		default:
			r.Status = "unreachable"
		}
		r.Error = hostErr(h.Name, err).Error()
		return r
	}
	r.reached = true
	r.Julienning, r.Claude, r.Tmux, r.TmuxMode = &hello.Version, hello.ClaudeVersion, hello.TmuxVersion, hello.Tmux
	local := localVersion()
	switch id := hello.ID(); {
	case id != "" && id != h.MachineID:
		r.Status = "machine id changed"
		return r
	case !hello.SetUp:
		r.Status = "not set up"
		return r
	case !selfupdate.IsDevBuild(local) && !selfupdate.IsDevBuild(hello.Version):
		if same, err := selfupdate.SameMinor(local, hello.Version); err != nil || !same {
			r.Status = fmt.Sprintf("julienning %s (this machine %s)", hello.Version, local)
		}
	}
	var inv hosts.Inventory
	if err := listCall(context.Background(), t, "inventory", &inv); err != nil {
		if r.Status == "" {
			r.Status = "unreachable"
		}
		r.Error = hostErr(h.Name, err).Error()
		return r
	}
	personal := map[string]bool{}
	seen := map[string]bool{}
	for _, c := range inv.Configs {
		switch {
		case c.Email == "":
		case c.Shared:
			if !seen[c.Email] || c.Current {
				r.Accounts = upsertAccount(r.Accounts, listAccount{Email: c.Email, Nickname: c.Nickname, Config: c.Name, Current: c.Current})
				seen[c.Email] = true
			}
		default:
			personal[c.Email] = true
		}
	}
	r.personal = len(personal)
	if r.Status == "" {
		if hello.SetupWarning != "" {
			r.Status = "setup incomplete"
		} else {
			r.Status = "ok"
		}
	}
	return r
}

// upsertAccount keeps one row per email, the selected dir winning.
func upsertAccount(list []listAccount, a listAccount) []listAccount {
	for i := range list {
		if list[i].Email == a.Email {
			if a.Current {
				list[i] = a
			}
			return list
		}
	}
	return append(list, a)
}

func accountsCell(r listRow) string {
	var parts []string
	accts := append([]listAccount(nil), r.Accounts...)
	sort.Slice(accts, func(i, j int) bool { return label(accts[i]) < label(accts[j]) })
	for _, a := range accts {
		s := label(a)
		if a.Current {
			s = "*" + s
		}
		parts = append(parts, s)
	}
	if r.personal > 0 {
		parts = append(parts, fmt.Sprintf("+%d personal", r.personal))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func label(a listAccount) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.Email
}

func tmuxCell(r listRow) string {
	switch {
	case r.Tmux == nil:
		return "-"
	case r.TmuxMode == nil:
		return *r.Tmux + " (undecided)"
	case r.TmuxMode.Enabled:
		return *r.Tmux + " (on: " + r.TmuxMode.Session + ")"
	}
	return *r.Tmux + " (off)"
}

func deref(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// --- host remove ---

func hostRemove(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("host remove", env)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return cli.Usagef("expected one host name")
	}
	name := pos[0]
	hf, h, err := loadHost(name)
	if err != nil {
		return err
	}
	hf.Remove(name)
	if err := hf.Save(); err != nil {
		return err
	}
	closeSocket(sshx.TargetOf(h))
	fmt.Fprintf(env.Stdout, "Removed %s from this machine's hosts; julienning on %s is unchanged (remove it there first with: julienning uninstall --host %s)\n", name, name, name)
	return nil
}

// closeSocket ends a host's ssh control socket (a var for tests).
var closeSocket = sshx.Close
