package dirs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/sessions"
)

// moveSession is sessions.Move; tests replace it to inject failures and
// signals between moves.
var moveSession = sessions.Move

// sessionMove is uninstall's offer to move every session of the registered
// dirs into Claude's default dir: once the claude wrapper is gone, plain
// claude opens only that dir and cannot resume anything else.
type sessionMove struct {
	target  config.ConfigDir   // Claude's default dir; may not exist yet
	sources []config.ConfigDir // registered dirs other than target, one per real path
	list    []sessions.Session
}

// newSessionMove lists what uninstall would move. A folder or file that
// cannot be read is a warning: its sessions stay where they are.
func newSessionMove(env cli.Env, cfg *config.Config) (*sessionMove, error) {
	def, err := claudecfg.DefaultDir()
	if err != nil {
		return nil, err
	}
	m := &sessionMove{target: config.ConfigDir{Name: shortenHome(def), Dir: def}}
	// A registered dir that is the default dir (or a symlink to it, or to
	// another registered dir) is listed once or not at all, so no session
	// is offered twice or "moved" onto itself.
	seen := map[string]bool{realDir(def): true}
	for _, cd := range cfg.Configs {
		if key := realDir(cd.Dir); !seen[key] {
			seen[key] = true
			m.sources = append(m.sources, cd)
		}
	}
	if len(m.sources) == 0 {
		return m, nil
	}
	list, err := sessions.List(sessions.ListOptions{Dirs: m.sources, Every: true, Now: now()})
	if err != nil {
		warnf(env, "some sessions could not be read and stay where they are: %v", err)
	}
	m.list = list
	return m, nil
}

func realDir(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// maxListed caps the per-session lines uninstall prints (open sessions,
// sessions left behind).
const maxListed = 10

// describe says what the question is about: how many sessions, which dir
// and account plain claude will use, and which sessions are open (a move
// refuses them, so they stay unless exited first).
func (m *sessionMove) describe(w io.Writer) {
	dirs := map[string]bool{}
	var open []sessions.Session
	for _, s := range m.list {
		dirs[s.ConfigDir] = true
		if s.Live {
			open = append(open, s)
		}
	}
	fmt.Fprintf(w, "Sessions: %s in %s. Without julienning, plain claude opens %s (%s) and cannot resume them.\n",
		countOf(len(m.list), "session", "sessions"), countOf(len(dirs), "registered config dir", "registered config dirs"),
		m.target.Name, defaultAccount(m.target.Dir))
	if _, err := os.Stat(m.target.Dir); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "  %s does not exist yet; moving creates it.\n", m.target.Name)
	}
	switch len(open) {
	case 0:
	case 1:
		fmt.Fprintln(w, "  1 is open in a terminal and stays where it is unless you exit it first:")
	default:
		fmt.Fprintf(w, "  %d are open in a terminal and stay where they are unless you exit them first:\n", len(open))
	}
	if len(open) > 0 {
		for i, s := range open {
			if i == maxListed {
				fmt.Fprintf(w, "    … and %d more\n", len(open)-maxListed)
				break
			}
			fmt.Fprintf(w, "    %q in %s\n", s.Label(), shortenHome(s.ConfigDir))
		}
	}
}

// question is the yes/no asked after describe.
func (m *sessionMove) question() string {
	return fmt.Sprintf("Move them into %s? Project memory is merged, never overwritten. [Y/n] ", m.target.Name)
}

// countOf renders "1 session" / "3 sessions".
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// defaultAccount names the login of Claude's default dir for human output.
func defaultAccount(dir string) string {
	email, ok, err := dirEmail(dir)
	switch {
	case ok:
		return "signed in as " + email
	case err != nil:
		return "its login cannot be read"
	default:
		return "not signed in yet; claude asks you to log in"
	}
}

// keptSession is a session uninstall left in its dir, with why.
type keptSession struct {
	s   sessions.Session
	why string
}

// moveResult is what run did.
type moveResult struct {
	moved       int
	kept        []keptSession
	notTried    int   // left untouched after run stopped early
	stoppedBy   error // the error that stopped the moves, if any
	interrupted bool  // stoppedBy is a signal: nothing after the moves may run
}

// run moves every listed session into the target, creating the target when
// it is missing. A refusal (open, recently changed, id already there)
// keeps that one session and goes on; any other error stops the moves,
// because it is usually the disk or permissions and would repeat for every
// session. A signal stops them too, between two moves or inside one (which
// then undoes itself). Sessions never move halfway: each is in exactly one
// of the two dirs (sessions.Move).
func (m *sessionMove) run(env cli.Env, say func(string, ...any)) moveResult {
	var res moveResult
	if err := m.ensureTarget(say); err != nil {
		res.stoppedBy, res.notTried = err, len(m.list)
		return res
	}

	// Held for the whole loop so a Ctrl-C between two moves stops cleanly
	// instead of killing the process before the summary.
	trap := sessions.HoldSignals()
	defer trap.Stop()

	var mem sessions.MemoryReport
	conflicts := map[string]bool{}
	warned := map[string]bool{}
	todo := m.bySource()
	for i, s := range todo {
		if err := trap.Check(); err != nil {
			res.stoppedBy, res.interrupted = err, true
		}
		if res.stoppedBy != nil {
			res.notTried = len(todo) - i
			break
		}
		if i == 0 || s.ConfigDir != todo[i-1].ConfigDir {
			fmt.Fprintf(env.Stdout, "Sessions: moving %s from %s…\n", countOf(countIn(todo[i:], s.ConfigDir), "session", "sessions"), shortenHome(s.ConfigDir))
		}
		// The listing ran before the question; a session exited since then
		// may move now. Move re-reads both live-session registries itself.
		s.Live, s.LiveIn = false, ""
		rep, err := moveSession(s, m.target)
		var why string
		switch {
		case errors.Is(err, sessions.ErrInterrupted):
			res.stoppedBy, res.interrupted = err, true
		case errors.Is(err, sessions.ErrLive):
			why = "open in a terminal"
		case errors.Is(err, sessions.ErrMaybeOpen):
			why = "changed in the last few minutes, so it may be open"
		case errors.Is(err, sessions.ErrCollision):
			why = m.target.Name + " already has a session with this id"
		case err != nil:
			res.stoppedBy = err
		}
		if res.stoppedBy != nil {
			res.notTried = len(todo) - i
			break
		}
		if why != "" {
			res.kept = append(res.kept, keptSession{s, why})
			continue
		}
		res.moved++
		mem.Added = append(mem.Added, rep.Memory.Added...)
		mem.IndexLines += rep.Memory.IndexLines
		// Every move of a repository's sessions merges the same memory
		// folder again and reports the same conflicts; name each once.
		for _, c := range rep.Memory.Conflicts {
			key := s.ConfigDir + "\x00" + rep.MemoryProject + "\x00" + c
			if conflicts[key] {
				continue
			}
			conflicts[key] = true
			mem.Conflicts = append(mem.Conflicts, c)
			warnf(env, "kept %s's version of project memory file %s; the one from %s stays at %s",
				m.target.Name, c, shortenHome(s.ConfigDir), shortenHome(filepath.Join(s.ConfigDir, "projects", rep.MemoryProject, "memory", c)))
		}
		for _, w := range rep.Warnings {
			if !warned[w] {
				warned[w] = true
				warnf(env, "%s", w)
			}
		}
	}
	// A signal during the last move's source deletion or memory merge
	// (outside Move's own trap) reached only this trap. Nothing after the
	// moves may run then.
	if res.stoppedBy == nil {
		if err := trap.Check(); err != nil {
			res.stoppedBy, res.interrupted = err, true
		}
	}
	if res.moved > 0 {
		line := fmt.Sprintf("Sessions: moved %s into %s", countOf(res.moved, "session", "sessions"), m.target.Name)
		if sum := mem.Summary(); sum != "" {
			line += " (memory: " + sum + ")"
		}
		say("%s", line)
	}
	return res
}

// bySource orders the sessions by source dir (registration order), newest
// first within a dir, so progress is reported once per dir.
func (m *sessionMove) bySource() []sessions.Session {
	out := make([]sessions.Session, 0, len(m.list))
	for _, cd := range m.sources {
		for _, s := range m.list {
			if s.ConfigDir == cd.Dir {
				out = append(out, s)
			}
		}
	}
	return out
}

func countIn(list []sessions.Session, dir string) int {
	n := 0
	for _, s := range list {
		if s.ConfigDir == dir {
			n++
		}
	}
	return n
}

// ensureTarget creates Claude's default dir when it is missing (Claude
// creates it 0700 too). Anything else in its place stops the moves.
func (m *sessionMove) ensureTarget(say func(string, ...any)) error {
	fi, err := os.Stat(m.target.Dir)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%s is not a directory; move it out of the way to let uninstall move the sessions there", m.target.Name)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("check %s: %w", m.target.Name, err)
	}
	if err := os.Mkdir(m.target.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", m.target.Name, err)
	}
	say("Sessions: created %s", m.target.Name)
	return nil
}

// reportKept lists the sessions that stay in their dirs and how to resume
// each without julienning.
func reportKept(w io.Writer, res moveResult) {
	n := len(res.kept) + res.notTried
	if n == 0 {
		return
	}
	where := "1 stays where it is"
	if n != 1 {
		where = fmt.Sprintf("%d stay where they are", n)
	}
	fmt.Fprintf(w, "Sessions: %s; resume one with CLAUDE_CONFIG_DIR set to its dir:\n", where)
	for i, k := range res.kept {
		if i == maxListed {
			break
		}
		fmt.Fprintf(w, "  %q (%s):\n    %s\n", k.s.Label(), k.why, resumeCommand(k.s))
	}
	if more := n - min(len(res.kept), maxListed); more > 0 {
		fmt.Fprintf(w, "  … and %d more\n", more)
	}
}

// safeShellWord matches paths that need no quoting after `~/` expansion.
var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9_./+@%:,=-]+$`)

// resumeCommand is the command that resumes s natively: tilde-shortened when
// that needs no quoting (an assignment expands ~/), else the absolute path
// single-quoted.
func resumeCommand(s sessions.Session) string {
	dir := shortenHome(s.ConfigDir)
	switch {
	case strings.HasPrefix(dir, "~/") && safeShellWord.MatchString(dir[2:]):
	case safeShellWord.MatchString(s.ConfigDir):
		dir = s.ConfigDir
	default:
		dir = "'" + strings.ReplaceAll(s.ConfigDir, "'", `'\''`) + "'"
	}
	return "CLAUDE_CONFIG_DIR=" + dir + " claude --resume " + s.ID
}
