package hosts

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/cli/switching"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/hosts"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/sshx"
	"github.com/muratgozel/julienning/internal/tui"
)

func init() {
	cli.Register(&cli.Command{
		Name:    "transfer",
		Summary: "move a session to another machine and resume it there (--host: from another machine)",
		Usage:   "transfer [--host SOURCE] [--to HOST] [TARGET] [--all] [--limit N] [--no-launch]",
		Host:    cli.HostSource,
		Run:     runTransfer,
	})
}

// Seams for tests: the pickers and the confirmation draw on a terminal.
var (
	pickRow = func(env cli.Env, items []tui.Item, opts tui.Options) (int, error) {
		opts.In, opts.Out = env.Stdin, env.Stdout
		return tui.Pick(items, opts)
	}
	confirmTransfer = func(env cli.Env, question, detail string) (bool, error) {
		return tui.Confirm(tui.ConfirmOptions{Question: question, Detail: detail, In: env.Stdin, Out: env.Stdout})
	}
)

// progressAbove is the size from which a transfer shows its progress.
const progressAbove = 5 << 20

// side is one machine of a transfer: this machine, or a host.
type side struct {
	local  bool
	host   hosts.Host
	hello  *hosts.Hello
	home   string
	claude string // its Claude Code version, "" when unknown
	id     string // its machine id
}

func (s side) name() string {
	if s.local {
		return "this machine"
	}
	return s.host.Name
}

// on is "on this machine" / "on box".
func (s side) on() string { return "on " + s.name() }

func (s side) short(p string) string { return homeShort(p, s.home) }

// errCanceled ends a transfer the user left: nothing was moved.
var errCanceled = errors.New("transfer canceled")

func runTransfer(env cli.Env) error {
	fs := cli.NewFlagSet("transfer", env)
	to := fs.String("to", "", "the machine to move the session to: a host name, or local")
	all := fs.Bool("all", false, "list the sessions of every project, not just this directory's")
	limit := fs.Int("limit", sessions.DefaultLimit, "how many recent sessions to offer")
	noLaunch := fs.Bool("no-launch", false, "move only; print how to resume it")
	pos, err := parseArgs(fs, env.Args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return cli.Usagef("transfer takes at most one TARGET account")
	}
	if *limit < 1 || *limit > 500 {
		return cli.Usagef("--limit must be between 1 and 500")
	}
	target := ""
	if len(pos) == 1 {
		target = pos[0]
	}
	if !isInteractive(env) {
		return errors.New("transfer asks which session to move and needs a terminal")
	}
	err = transfer(env, target, *to, *all, *limit, *noLaunch)
	if errors.Is(err, sshx.ErrInterrupted) {
		return errors.New("transfer interrupted (Ctrl-C); the source still has the session, and the destination keeps nothing partial")
	}
	if errors.Is(err, errCanceled) {
		fmt.Fprintln(env.Stdout, "Transfer canceled; nothing was moved.")
		return nil
	}
	if errors.Is(err, errUpdatedHere) {
		return nil
	}
	return err
}

func transfer(env cli.Env, target, toName string, all bool, limit int, noLaunch bool) error {
	ctx := context.Background()
	a := newAsker(env, false)
	hf, err := hosts.Load()
	if err != nil {
		return err
	}
	src, err := sideFor(ctx, env, a, hf, env.Host)
	if err != nil {
		return err
	}
	if toName == "" {
		if toName, err = pickDestination(env, hf, src); err != nil {
			return err
		}
	}
	if toName == hosts.Local {
		toName = ""
	}
	if toName == env.Host {
		if src.local {
			return errors.New("this machine is both the source and the destination; to move a session between accounts here, use: julienning use")
		}
		return fmt.Errorf("%s is both the source and the destination; to move a session between accounts there, use: julienning use --host %s", src.name(), src.name())
	}
	dst, err := sideFor(ctx, env, a, hf, toName)
	if err != nil {
		return err
	}
	if err := sameClaude(src, dst); err != nil {
		return err
	}

	now := time.Now()
	loc := switching.Location()
	choice, err := chooseOn(ctx, dst, target)
	if err != nil {
		return err
	}
	for _, w := range choice.Warnings {
		if dst.local {
			warnf(env, "%s", w)
		} else {
			warnf(env, "%s: %s", dst.name(), w)
		}
	}
	header := switching.MovingLine(choice, dst.short(choice.Config.Dir), dst.on(), now, loc)

	ls, err := sessionsOn(ctx, src, all, limit)
	if err != nil {
		return err
	}
	for _, w := range ls.Warnings {
		warnf(env, "%s", w)
	}
	where := ""
	if !all {
		where = " in " + src.short(ls.Cwd)
	}
	if len(ls.Sessions) == 0 {
		return fmt.Errorf("no session to transfer%s %s (--all lists every project)", where, src.on())
	}
	items := transferItems(ls, src, dst, all, now, loc)
	scope := switching.ScopeLine(len(ls.Sessions), limit, all, src.short(ls.Cwd), src.on())

	var s sessions.Session
	var place hosts.Located
	for at := 0; ; {
		idx, err := pickRow(env, items, tui.Options{Header: header, Scope: scope, Initial: at})
		if errors.Is(err, tui.ErrCanceled) {
			return errCanceled
		}
		if err != nil {
			return err
		}
		if idx < 0 || idx >= len(ls.Sessions) || items[idx].Disabled {
			continue
		}
		s = ls.Sessions[idx]
		project := ls.Projects[s.Cwd]
		if project == nil {
			return fmt.Errorf("session %q records no working directory, so it has no place %s", s.Label(), dst.on())
		}
		if place, err = locateOn(ctx, env, a, src, dst, project, s); err != nil {
			return err
		}
		if s.GitBranch != "" && place.Branch != "" && s.GitBranch != place.Branch {
			warnf(env, "the session was on branch %s; %s's %s is on %s", s.GitBranch, dst.name(), dst.short(place.Checkout), place.Branch)
		}
		from := labelOf(ls.Labels, s.ConfigName) + " " + src.on()
		into := choiceLabel(choice) + " " + dst.on()
		ok, err := confirmTransfer(env, fmt.Sprintf("Move %q from %s to %s?", s.Label(), from, into),
			fmt.Sprintf("Its transcript, checkpoints, task list and session environment move to %s %s; %s will no longer have it. Paths inside it are rewritten for %s. Project memory is merged, never overwritten.",
				dst.short(place.Cwd), dst.on(), src.name(), dst.name()))
		if err != nil {
			return err
		}
		if ok {
			break
		}
		at = idx
	}

	res, err := moveBetween(ctx, env, src, dst, s, choice, place)
	if err != nil {
		return err
	}
	from := labelOf(ls.Labels, s.ConfigName) + " " + src.on()
	into := choiceLabel(choice) + " " + dst.on()
	if res.Already {
		where := ""
		if res.Dir != "" && filepath.Clean(res.Dir) != filepath.Clean(choice.Config.Dir) {
			where = " (in " + dst.short(res.Dir) + ")"
		}
		fmt.Fprintf(env.Stdout, "%s already had %q%s from a transfer that lost its connection; finished it.\n", dst.name(), s.Label(), where)
		if where != "" {
			// It resumes where the first transfer put it.
			choice.Config, choice.Nickname = config.ConfigDir{Dir: res.Dir}, ""
		}
	} else {
		var details []string
		if m := res.Memory.Summary(); m != "" {
			details = append(details, "memory: "+m)
		}
		details = append(details, fmt.Sprintf("%s rewritten for %s", plural(res.Rewrites, "path", "paths"), dst.short(place.Cwd)))
		fmt.Fprintf(env.Stdout, "Moved %q from %s to %s (%s).\n", s.Label(), from, into, strings.Join(details, "; "))
		for _, c := range res.Memory.Conflicts {
			warnf(env, "project memory file %s differs between %s and %s; kept %s's version", c, from, into, into)
		}
	}
	for _, w := range res.Warnings {
		warnf(env, "%s", w)
	}

	if noLaunch {
		fmt.Fprintf(env.Stdout, "Resume it %s: cd %s && %s\n", dst.on(), dst.short(place.Cwd), resumeCommand(dst, choice, s.ID))
		return nil
	}
	return launchOn(env, dst, choice, s.ID, place.Cwd)
}

// transferItems are the session picker's rows. A session written by a
// Claude Code of another major.minor than the destination's is shown but
// cannot be picked (patch releases may differ): it may not resume there.
func transferItems(ls hosts.SessionsAnswer, src, dst side, all bool, now time.Time, loc *time.Location) []tui.Item {
	items := make([]tui.Item, len(ls.Sessions))
	dstMinor := claudeMinor(dst.claude)
	for i, s := range ls.Sessions {
		cwd := ""
		if all {
			cwd = src.short(s.Cwd)
		}
		items[i] = switching.SessionItem(s, labelOf(ls.Labels, s.ConfigName), cwd, now, loc)
		if m := claudeMinor(s.ClaudeVersion); m != "" && dstMinor != "" && m != dstMinor && !s.Live {
			items[i].Disabled = true
			items[i].Note = fmt.Sprintf("(written by Claude Code %s; %s runs %s)", m, dst.name(), dstMinor)
		}
	}
	return items
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func labelOf(labels map[string]string, name string) string {
	if l := labels[name]; l != "" {
		return l
	}
	return name
}

func choiceLabel(c switching.Choice) string {
	if c.Nickname != "" {
		return c.Nickname
	}
	return c.Config.Name
}

// sideFor is the machine named name ("" or local: this one), asked hello
// and held to the version rule.
func sideFor(ctx context.Context, env cli.Env, a *asker, hf *hosts.Hosts, name string) (side, error) {
	if name == "" || name == hosts.Local {
		cfg, err := config.Load()
		if err != nil {
			return side{}, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return side{}, err
		}
		s := side{local: true, home: home, id: cfg.MachineID}
		if c, err := findClaude(); err == nil {
			if v := claudeVersionOf(c); v != nil {
				s.claude = *v
			}
		}
		return s, nil
	}
	h, ok := hf.Find(name)
	if !ok {
		return side{}, unknownHost(name)
	}
	hello, err := helloChecked(ctx, h)
	if err != nil {
		return side{}, err
	}
	if !hello.SetUp {
		return side{}, fmt.Errorf("julienning is not set up on %s; run: julienning host add %s", h.Name, h.Name)
	}
	if hello, err = checkVersions(ctx, env, a, h, hello, versionsStrict); err != nil {
		return side{}, err
	}
	s := side{host: h, hello: hello, home: hello.Home, id: hello.ID()}
	if hello.ClaudeVersion != nil {
		s.claude = *hello.ClaudeVersion
	}
	return s, nil
}

var claudeMinorRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

// claudeMinor is a Claude Code version's major.minor, "" when unknown.
func claudeMinor(v string) string {
	m := claudeMinorRe.FindStringSubmatch(strings.TrimPrefix(v, "v"))
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2]
}

// sameClaude refuses, before anything is chosen, two machines whose Claude
// Code differs in major.minor: a session written by one may not resume on
// the other. Patch releases may differ.
func sameClaude(src, dst side) error {
	a, b := claudeMinor(src.claude), claudeMinor(dst.claude)
	if a == "" || b == "" || a == b {
		return nil
	}
	older := src
	if cmp := compareMinor(a, b); cmp > 0 {
		older = dst
	}
	return fmt.Errorf("%s runs Claude Code %s and %s %s; a transfer needs the same major.minor on both machines (patch releases may differ); update Claude Code %s (claude update) and run the transfer again",
		titleName(src), src.claude, dst.name(), dst.claude, older.on())
}

func titleName(s side) string {
	if s.local {
		return "This machine"
	}
	return s.name()
}

func compareMinor(a, b string) int {
	var a1, a2, b1, b2 int
	fmt.Sscanf(a, "%d.%d", &a1, &a2)
	fmt.Sscanf(b, "%d.%d", &b1, &b2)
	switch {
	case a1 != b1:
		return a1 - b1
	default:
		return a2 - b2
	}
}

// pickDestination asks where the session goes: this machine and every host,
// the source left out. It never probes; an unreachable host fails next.
func pickDestination(env cli.Env, hf *hosts.Hosts, src side) (string, error) {
	var names []string
	var items []tui.Item
	if !src.local {
		names = append(names, hosts.Local)
		items = append(items, tui.Item{Title: "this machine"})
	}
	for _, h := range hf.Hosts {
		if !src.local && h.Name == src.host.Name {
			continue
		}
		names = append(names, h.Name)
		items = append(items, tui.Item{Title: h.Name, Meta: h.SSH + " · " + h.OS + "/" + h.Arch})
	}
	if len(items) == 0 {
		return "", errors.New("no other machine to transfer to; add one with: julienning host add NAME [SSH_DEST]")
	}
	from := "this machine"
	if !src.local {
		from = src.name()
	}
	i, err := pickRow(env, items, tui.Options{Header: "Transfer a session from " + from + " to:", Choice: true})
	if errors.Is(err, tui.ErrCanceled) {
		return "", errCanceled
	}
	if err != nil {
		return "", err
	}
	return names[i], nil
}

// chooseOn picks the destination account (it becomes that machine's
// selection, as next does).
func chooseOn(ctx context.Context, dst side, target string) (switching.Choice, error) {
	if dst.local {
		c, err := switching.Choose(target, switching.Location())
		if errors.Is(err, switching.ErrNoneLoggedIn) {
			return c, errors.New("none of the shared accounts is logged in on this machine; sign one in: julienning login NICKNAME (`julienning accounts` lists them)")
		}
		return c, err
	}
	var ans hosts.ChooseAnswer
	err := agentCall(ctx, sshx.TargetOf(dst.host), "choose", hosts.ChooseRequest{Target: target, TZ: hosts.DisplayZone()}, &ans)
	var ae *hosts.AgentError
	if errors.As(err, &ae) && ae.Code == hosts.CodeNotLocal && ae.Message == "none of the shared accounts is logged in" {
		return switching.Choice{}, fmt.Errorf("none of the shared accounts is logged in on %s; sign one in: julienning login NICKNAME --host %s", dst.name(), dst.name())
	}
	if err != nil {
		return switching.Choice{}, hostErr(dst.name(), err)
	}
	return switching.Choice{Config: config.ConfigDir{Name: ans.Config, Dir: ans.Dir}, Email: ans.Email, Nickname: ans.Nickname, Account: ans.Account, Warnings: ans.Warnings}, nil
}

// sessionsOn lists what may move from src, in its checkout of this
// machine's working directory (every project with all).
func sessionsOn(ctx context.Context, src side, all bool, limit int) (hosts.SessionsAnswer, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return hosts.SessionsAnswer{}, fmt.Errorf("cannot determine the current directory: %w", err)
	}
	home, _ := os.UserHomeDir()
	if src.local {
		listed := cwd
		if all {
			listed = ""
		}
		list, labels, warnings, err := switching.TransferSessions(listed, all, limit)
		if err != nil {
			return hosts.SessionsAnswer{}, err
		}
		ans := hosts.SessionsAnswer{Cwd: listed, Sessions: list, Labels: labels, Projects: map[string]*hosts.Project{}, Warnings: warnings}
		for _, s := range list {
			if _, ok := ans.Projects[s.Cwd]; !ok && s.Cwd != "" {
				ans.Projects[s.Cwd] = hosts.ProjectOf(s.Cwd, home)
			}
		}
		return ans, nil
	}
	req := hosts.SessionsRequest{All: all, Limit: limit, Machine: src.name()}
	if !all {
		req.Project = hosts.ProjectOf(cwd, home)
	}
	var ans hosts.SessionsAnswer
	if err := agentCall(ctx, sshx.TargetOf(src.host), "sessions", req, &ans); err != nil {
		return ans, hostErr(src.name(), err)
	}
	return ans, nil
}

// locateOn finds where s goes on dst (SPEC "Locating a project"). When
// nothing is found, or several checkouts fit and dst cannot ask, the user
// is told how to clone it and may type the path of a checkout there.
func locateOn(ctx context.Context, env cli.Env, a *asker, src, dst side, p *hosts.Project, s sessions.Session) (hosts.Located, error) {
	here := "here"
	if !src.local {
		here = src.on()
	}
	ranIn := "the session ran in " + src.short(s.Cwd) + " " + here
	ask := func(path string) (hosts.Located, error) {
		if dst.local {
			cfg, _ := config.Load()
			o := hosts.LocateOptions{Home: dst.home, ConfigDirs: configDirs(cfg), Branch: s.GitBranch, Machine: "this machine", RanIn: ranIn}
			if path != "" {
				return hosts.Check(p, path, o)
			}
			o.Pick = func(checkouts []string) (int, error) {
				items := make([]tui.Item, len(checkouts))
				for i, c := range checkouts {
					items[i] = tui.Item{Title: c}
				}
				i, err := pickRow(env, items, tui.Options{Header: "Several checkouts of " + p.Remotes[0] + " on this machine; move the session into:", Choice: true})
				if errors.Is(err, tui.ErrCanceled) {
					return 0, errCanceled
				}
				return i, err
			}
			return hosts.Locate(p, o)
		}
		var loc hosts.Located
		err := agentCall(ctx, sshx.TargetOf(dst.host), "locate", hosts.LocateRequest{Project: p, Branch: s.GitBranch, Machine: dst.name(), RanIn: ranIn, Path: path}, &loc)
		return loc, err
	}
	loc, err := ask("")
	if err == nil {
		return loc, nil
	}
	var nf *hosts.NotFound
	var amb *hosts.AmbiguousError
	var ae *hosts.AgentError
	switch {
	case errors.Is(err, errCanceled):
		return loc, err
	case errors.As(err, &nf), errors.As(err, &amb), errors.As(err, &ae) && (ae.Code == hosts.CodeNoCheckout || ae.Code == hosts.CodeAmbiguous):
	case errors.As(err, &ae):
		return loc, hostErr(dst.name(), err)
	default:
		return loc, err
	}
	msg := err.Error()
	if ae != nil {
		msg = ae.Message
	}
	fmt.Fprintln(env.Stdout, msg+".")
	question := "Type the path of the checkout to use " + dst.on() + " (Enter cancels): "
	switch {
	case amb != nil || (ae != nil && ae.Code == hosts.CodeAmbiguous):
	case p.InRepo():
		target := ""
		if rel, ok := p.CheckoutHomeRel(); ok && rel != "" {
			target = " ~/" + rel
		}
		fmt.Fprintf(env.Stdout, "Clone it there, for example:\n  git clone %s%s\n", p.CloneURL, target)
		question = "then run the transfer again, or type the path of a checkout " + dst.on() + " (Enter cancels): "
	case p.InHome:
		fmt.Fprintf(env.Stdout, "Create it there, for example: mkdir -p ~/%s\n", p.HomeRel)
		question = "then run the transfer again, or type a path " + dst.on() + " (Enter cancels): "
	default:
		return loc, err
	}
	for {
		fmt.Fprint(env.Stdout, question)
		line, rerr := a.in.ReadString('\n')
		typed := strings.TrimSpace(line)
		if typed == "" || (rerr != nil && line == "") {
			return loc, errCanceled
		}
		loc, err := ask(typed)
		if err == nil {
			return loc, nil
		}
		msg := err.Error()
		if errors.As(err, &ae) {
			msg = ae.Message
		}
		fmt.Fprintln(env.Stdout, msg+".")
		question = "Type the path of a checkout " + dst.on() + " (Enter cancels): "
	}
}

// moveBetween moves s from src into dst's chosen account (SPEC "Moving a
// session between machines"): the source packs, the destination receives
// (this machine relays when both are hosts; the two never connect), and
// once the destination committed, the source releases.
func moveBetween(ctx context.Context, env cli.Env, src, dst side, s sessions.Session, choice switching.Choice, place hosts.Located) (sessions.UnpackResult, error) {
	var res sessions.UnpackResult
	// Ctrl-C ends the ssh processes, so a receiving agent sees the end of
	// its input and undoes; this machine's own Unpack traps it itself.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, srcDone, err := openExport(ctx, src, s)
	if err != nil {
		return res, err
	}
	defer stream.Close()
	header, body, err := splitHeader(stream, !src.local)
	if err != nil {
		cancel()
		<-srcDone
		return res, hostErr(src.name(), err)
	}
	var archive io.Reader = io.MultiReader(bytes.NewReader(header), body)
	var prog *progress
	if h, err := sessions.ReadHeader(bufio.NewReader(bytes.NewReader(header))); err == nil && h.Bytes > progressAbove {
		prog = newProgress(env, fmt.Sprintf("Sending %q to %s", s.Label(), dst.name()), h.Bytes)
		archive = io.MultiReader(bytes.NewReader(header), io.TeeReader(body, prog))
	}

	req := hosts.ReceiveRequest{ConfigDir: choice.Config.Dir, Email: choice.Email, Cwd: place.Cwd, Checkout: place.Checkout, GitRoot: place.GitRoot, From: src.id}
	if dst.local {
		received, perr := config.Path(hosts.ReceivedDir)
		if perr != nil {
			return res, perr
		}
		res, err = sessions.Unpack(archive, sessions.UnpackOptions{
			Dir: req.ConfigDir, Email: req.Email, Cwd: req.Cwd, Checkout: req.Checkout, GitRoot: req.GitRoot,
			Home: dst.home, ReceivedDir: received, From: req.From, Now: time.Now(),
		})
	} else {
		res, err = receiveOn(ctx, dst, req, archive)
	}
	if prog != nil {
		prog.done(err == nil)
	}
	if err != nil {
		cancel()
		stream.Close()
		<-srcDone
		var ae *hosts.AgentError
		if dst.local || !errors.As(err, &ae) {
			return res, err
		}
		return res, hostErr(dst.name(), err)
	}
	// The destination verified the whole archive; whatever the source's
	// process says now cannot change that.
	stream.Close()
	<-srcDone

	warnings, err := releaseOn(ctx, src, s, res.ManifestSHA256)
	var ae *hosts.AgentError
	if errors.Is(err, sessions.ErrChanged) || (errors.As(err, &ae) && ae.Code == hosts.CodeChanged) {
		return res, fmt.Errorf("%q changed %s while it was being sent; both copies are kept (%s has it as it was before the change)", s.Label(), src.on(), dst.name())
	}
	if err != nil {
		return res, fmt.Errorf("%s has the session, but releasing it %s failed (%v); it is in both places now", dst.name(), src.on(), hostErr(src.name(), err))
	}
	res.Warnings = append(res.Warnings, warnings...)
	return res, nil
}

// openExport starts the source's pack: in process here, or `agent export`
// over ssh. done is closed when it has finished.
func openExport(ctx context.Context, src side, s sessions.Session) (io.ReadCloser, <-chan struct{}, error) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	if src.local {
		go func() {
			defer close(done)
			_, _, err := sessions.Pack(s, packSourceOf(s, src.home), pw)
			pw.CloseWithError(err)
		}()
		return pr, done, nil
	}
	req, err := json.Marshal(hosts.ExportRequest{ID: s.ID, ConfigDir: s.ConfigDir})
	if err != nil {
		return nil, nil, err
	}
	t := sshx.TargetOf(src.host)
	var stderr bytes.Buffer
	go func() {
		defer close(done)
		code, err := runStream(ctx, sshx.Call{Target: t, Command: sshx.AgentCommand(t.Julienning, "export"), Stdin: bytes.NewReader(req), Stdout: pw, Stderr: &stderr})
		if err == nil && code != 0 {
			err = fmt.Errorf("export ended with exit %d: %s", code, lastLine(stderr.Bytes()))
		}
		pw.CloseWithError(err)
	}()
	return pr, done, nil
}

// runStream runs a streaming ssh call (a var for tests).
var runStream = func(ctx context.Context, c sshx.Call) (int, error) { return c.Run(ctx) }

// splitHeader returns the header line and the rest of an export. A host's
// answer (framed) has startup-file noise dropped up to the frame line; the
// in-process pack starts with the header. An error answer instead of a
// header becomes an *AgentError.
func splitHeader(r io.Reader, framed bool) ([]byte, io.Reader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	first, err := br.ReadBytes('\n')
	if err != nil {
		return nil, nil, fmt.Errorf("no session arrived: %v", err)
	}
	if framed {
		for !strings.HasPrefix(strings.TrimSpace(string(first)), hosts.FramePrefix) {
			if first, err = br.ReadBytes('\n'); err != nil {
				return nil, nil, fmt.Errorf("no answer from julienning: %v", err)
			}
		}
		if first, err = br.ReadBytes('\n'); err != nil {
			return nil, nil, fmt.Errorf("no session arrived: %v", err)
		}
	}
	var probe struct {
		Error *hosts.AgentError `json:"error"`
	}
	if json.Unmarshal(first, &probe) == nil && probe.Error != nil {
		return nil, nil, probe.Error
	}
	return first, br, nil
}

// receiveOn runs `agent receive` on dst: the request line, then the
// archive, on its stdin.
func receiveOn(ctx context.Context, dst side, req hosts.ReceiveRequest, archive io.Reader) (sessions.UnpackResult, error) {
	var res sessions.UnpackResult
	line, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	t := sshx.TargetOf(dst.host)
	var out, stderr bytes.Buffer
	code, err := runStream(ctx, sshx.Call{Target: t, Command: sshx.AgentCommand(t.Julienning, "receive"),
		Stdin: io.MultiReader(bytes.NewReader(append(line, '\n')), archive), Stdout: &out, Stderr: &stderr})
	if err != nil {
		return res, err
	}
	if _, err := hosts.DecodeAnswer(out.Bytes(), &res); err != nil {
		if errors.Is(err, hosts.ErrNoFrame) {
			return res, fmt.Errorf("%s did not answer the transfer (exit %d): %s", dst.name(), code, lastLine(stderr.Bytes()))
		}
		return res, err
	}
	return res, nil
}

// releaseOn deletes the source's copy, only when it is unchanged since it
// was packed.
func releaseOn(ctx context.Context, src side, s sessions.Session, manifestSHA string) ([]string, error) {
	if src.local {
		return sessions.Release(s, manifestSHA)
	}
	var ans hosts.ReleaseAnswer
	err := agentCall(ctx, sshx.TargetOf(src.host), "release", hosts.ReleaseRequest{ID: s.ID, ConfigDir: s.ConfigDir, ManifestSHA256: manifestSHA}, &ans)
	return ans.Warnings, err
}

// resumeCommand is how to resume the moved session by hand on dst.
func resumeCommand(dst side, c switching.Choice, id string) string {
	isDefault := false
	if dst.local {
		isDefault = claudecfg.IsDefaultDir(c.Config.Dir)
	} else {
		isDefault = filepath.Clean(c.Config.Dir) == filepath.Join(dst.home, ".claude")
	}
	switch {
	case isDefault:
		return "command claude --resume " + id
	case c.Nickname != "":
		return "claude-" + c.Nickname + " --resume " + id
	}
	return "CLAUDE_CONFIG_DIR=" + dst.short(c.Config.Dir) + " claude --resume " + id
}

// launchOn resumes the moved session on dst, in that machine's tmux mode.
func launchOn(env cli.Env, dst side, c switching.Choice, id, cwd string) error {
	args := []string{"--resume", id}
	if dst.local {
		return execClaude(c.Config.Dir, args, cwd)
	}
	arg, err := hosts.EncodeArg(hosts.LaunchRequest{MachineID: dst.id, Dir: c.Config.Dir, Cwd: cwd, Args: args, Host: dst.name()})
	if err != nil {
		return err
	}
	t := sshx.TargetOf(dst.host)
	code, err := runForwarded(context.Background(), sshx.Call{Target: t, TTY: isInteractive(env), Command: sshx.AgentCommand(t.Julienning, "launch", arg),
		Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr, Interactive: true})
	if err != nil {
		return err
	}
	if code != 0 {
		return cli.Exit(code)
	}
	return nil
}

// progress shows "Sending "Fix flaky clock tests" to box: 23 of 61 MB"
// while a large session travels. It counts the archive's bytes after
// decompression, so the total is the files' size the header announced.
type progress struct {
	env      cli.Env
	what     string
	total    int64
	pw       *io.PipeWriter
	mu       sync.Mutex
	seen     int64
	wg       sync.WaitGroup
	lastShow time.Time
}

func newProgress(env cli.Env, what string, total int64) *progress {
	pr, pw := io.Pipe()
	p := &progress{env: env, what: what, total: total, pw: pw}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer pr.Close()
		zr, err := gzip.NewReader(pr)
		if err != nil {
			_, _ = io.Copy(io.Discard, pr)
			return
		}
		buf := make([]byte, 64<<10)
		for {
			n, err := zr.Read(buf)
			p.add(int64(n))
			if err != nil {
				_, _ = io.Copy(io.Discard, pr)
				return
			}
		}
	}()
	return p
}

// Write feeds the compressed archive; a failure here never stops the
// transfer.
func (p *progress) Write(b []byte) (int, error) {
	_, _ = p.pw.Write(b)
	return len(b), nil
}

func (p *progress) add(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen += n
	if time.Since(p.lastShow) < 200*time.Millisecond {
		return
	}
	p.lastShow = time.Now()
	fmt.Fprintf(p.env.Stdout, "\r%s: %d of %d MB", p.what, min(p.seen, p.total)>>20, p.total>>20)
}

// done ends the line: the total when the destination took it all, else
// what got through.
func (p *progress) done(ok bool) {
	p.pw.Close()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	sent := min(p.seen, p.total)
	if ok {
		sent = p.total
	}
	fmt.Fprintf(p.env.Stdout, "\r%s: %d of %d MB\n", p.what, sent>>20, p.total>>20)
}
