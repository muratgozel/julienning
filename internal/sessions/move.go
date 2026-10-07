package sessions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
)

// Refusal kinds; test with errors.Is. The error text itself is the
// actionable, user-facing message.
var (
	ErrLive      = errors.New("session is open")
	ErrMaybeOpen = errors.New("session may be open")
	ErrCollision = errors.New("session id already in target")
)

type refusal struct {
	kind error
	msg  string
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return r.kind }

// now is the clock for the "modified recently" refusal; tests replace it.
var now = time.Now

// Report describes a completed move.
type Report struct {
	ID       string
	Label    string // title, first prompt or short id
	From, To string // config names; callers may relabel them for display (switching shows nicknames)
	Items    []string
	Memory   MemoryReport
	// Warnings are problems after the session was safely in place (source
	// leftovers, memory files that could not be merged). The move itself
	// succeeded; callers should print them.
	Warnings []string
}

// String renders the one-line summary from the spec:
// Moved "<title>" from julienning1 to julienning3 (memory: 2 files added, 1 conflict kept).
func (r Report) String() string {
	s := fmt.Sprintf("Moved %q from %s to %s", r.Label, r.From, r.To)
	if m := r.Memory.summary(); m != "" {
		s += " (memory: " + m + ")"
	}
	return s + "."
}

// item is one path moved as a unit, relative to the config dir.
type item struct {
	rel      string
	src, dst string
	tmp      string
}

// Move hands s over to target: the transcript, its side files, and the
// project memory (merged, never overwritten). The order is copy under
// temporary names (fsynced) → rename into place → verify sizes → delete the
// source, so a failure before the deletes leaves the source untouched and the
// target without leftovers. SIGINT/SIGTERM/SIGHUP are held until the copy is
// verified: one arriving earlier aborts the move the same way (ErrInterrupted).
func Move(s Session, target config.ConfigDir) (Report, error) {
	rep := Report{ID: s.ID, Label: s.Label(), From: s.ConfigName, To: target.Name}
	if err := validateMove(s, target); err != nil {
		return rep, err
	}
	srcRoot, dstRoot := filepath.Clean(s.ConfigDir), filepath.Clean(target.Dir)

	jsonlRel := filepath.Join("projects", s.ProjectDir, s.ID+".jsonl")
	fi, err := os.Lstat(filepath.Join(srcRoot, jsonlRel))
	if errors.Is(err, os.ErrNotExist) {
		return rep, fmt.Errorf("session %s is no longer in %s (was it moved or deleted?)", s.ShortID(), s.ConfigName)
	}
	if err != nil {
		return rep, fmtErr("inspect", filepath.Join(srcRoot, jsonlRel), err)
	}
	if !fi.Mode().IsRegular() {
		return rep, fmt.Errorf("%s is not a regular file", filepath.Join(srcRoot, jsonlRel))
	}

	if err := refuseIfOpen(s, srcRoot, dstRoot, fi.ModTime()); err != nil {
		return rep, err
	}

	items, err := collectItems(s, srcRoot, dstRoot, jsonlRel)
	if err != nil {
		return rep, err
	}
	if err := refuseCollision(s, target, dstRoot, items); err != nil {
		return rep, err
	}

	// Resolved before anything changes, from the source as it is now.
	memProject, memErr := memoryProject(s, srcRoot)

	tr := &transfer{items: items, trap: trapSignals()}
	err = tr.run()
	if err == nil {
		err = tr.trap.check() // nothing is deleted yet, so a late signal still aborts
	}
	if err != nil {
		cleanupErr := tr.undo() // still trapped: a second Ctrl-C cannot cut the cleanup short
		tr.trap.stop()
		return rep, moveFailed(s, target, err, cleanupErr)
	}
	tr.trap.stop()
	if err := tr.trap.check(); err != nil { // caught between the check above and stop
		return rep, moveFailed(s, target, err, tr.undo())
	}
	for _, it := range items {
		rep.Items = append(rep.Items, it.rel)
	}

	// The target now holds a verified copy. Remove the transcript first so
	// the session stops showing up in the source even if a side file cannot
	// be deleted.
	for _, it := range items {
		if err := removeSource(it.src); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("could not remove %s from %s (%v); delete it by hand", it.rel, s.ConfigName, err))
		}
	}

	// Claude keys memory by the repository's main worktree, not by the
	// session's folder: a session started in a worktree or subdirectory
	// uses projects/<enc(git root)>/memory (see memoryProject).
	if memErr != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("project memory not merged: %v", memErr))
	} else if memProject != "" {
		mem, err := mergeMemory(
			filepath.Join(srcRoot, "projects", memProject, "memory"),
			filepath.Join(dstRoot, "projects", memProject, "memory"),
		)
		rep.Memory = mem
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("project memory not fully merged: %v", err))
		}
	}
	return rep, nil
}

// moveFailed builds the error for a move that did not complete. The source
// is never touched before the copy is verified, so it is always intact; the
// target is clean unless cleanupErr says otherwise, and then the message
// must not claim "nothing was changed": the leftovers would make a retry
// look like a collision.
func moveFailed(s Session, target config.ConfigDir, err, cleanupErr error) error {
	if cleanupErr != nil {
		return fmt.Errorf("could not move session %s to %s: %w; the source is untouched, but the partial copy in %s could not be fully removed (%w); delete those leftovers by hand before trying again",
			s.ShortID(), target.Name, err, target.Name, cleanupErr)
	}
	if errors.Is(err, ErrInterrupted) {
		return fmt.Errorf("move of session %s to %s %w (nothing was changed)", s.ShortID(), target.Name, err)
	}
	return fmt.Errorf("could not move session %s to %s (nothing was changed): %w", s.ShortID(), target.Name, err)
}

func validateMove(s Session, target config.ConfigDir) error {
	if !ValidID(s.ID) {
		return fmt.Errorf("invalid session id %q", s.ID)
	}
	if !validProjectDir(s.ProjectDir) {
		return fmt.Errorf("invalid project folder %q", s.ProjectDir)
	}
	if !filepath.IsAbs(s.ConfigDir) || !filepath.IsAbs(target.Dir) {
		return errors.New("config dirs must be absolute paths")
	}
	if filepath.Clean(s.ConfigDir) == filepath.Clean(target.Dir) {
		return fmt.Errorf("session %s is already in %s", s.ShortID(), target.Name)
	}
	fi, err := os.Stat(target.Dir)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("config dir %s of %s does not exist", target.Dir, target.Name)
	}
	return nil
}

func refuseIfOpen(s Session, srcRoot, dstRoot string, mtime time.Time) error {
	where := s.LiveIn
	live := s.Live
	for _, dir := range []string{srcRoot, dstRoot} {
		ls, err := livesess.List(dir)
		if err != nil {
			// Fail closed: without the registry we cannot rule out a writer.
			return fmt.Errorf("cannot check whether session %s is open: %w", s.ShortID(), err)
		}
		for _, l := range ls {
			if l.SessionID == s.ID {
				live = true
				if where == "" {
					where = filepath.Base(dir)
				}
			}
		}
	}
	if live {
		msg := fmt.Sprintf("session %q is open in another terminal", s.Label())
		if where != "" {
			msg += " (" + where + ")"
		}
		return &refusal{ErrLive, msg + "; exit it there first"}
	}
	if !livesess.RegistryExists(srcRoot) && now().Sub(mtime) < RecentWindow {
		return &refusal{ErrMaybeOpen, fmt.Sprintf(
			"session %q changed less than %d minutes ago and %s has no live-session registry, so it may still be open; exit it or try again shortly",
			s.Label(), int(RecentWindow/time.Minute), s.ConfigName)}
	}
	return nil
}

// collectItems lists the source paths that exist for s. The transcript is
// always first.
func collectItems(s Session, srcRoot, dstRoot, jsonlRel string) ([]item, error) {
	rels := []string{
		jsonlRel,
		filepath.Join("projects", s.ProjectDir, s.ID),
		filepath.Join("file-history", s.ID),
		filepath.Join("session-env", s.ID),
		filepath.Join("tasks", s.ID), // Claude's per-session task list
		filepath.Join("debug", s.ID+".txt"),
	}
	// plans/<slug>.md stays: its slug is not derivable from the session id.
	// ValidID excludes glob metacharacters, so the id is safe in a pattern.
	todos, err := filepath.Glob(filepath.Join(srcRoot, "todos", s.ID+"-*.json"))
	if err != nil {
		return nil, err
	}
	for _, t := range todos {
		rels = append(rels, filepath.Join("todos", filepath.Base(t)))
	}
	var items []item
	for i, rel := range rels {
		src := filepath.Join(srcRoot, rel)
		ok, err := exists(src)
		if err != nil {
			return nil, fmtErr("inspect", src, err)
		}
		if !ok {
			if i == 0 {
				return nil, fmt.Errorf("session %s is no longer in %s", s.ShortID(), s.ConfigName)
			}
			continue
		}
		items = append(items, item{rel: rel, src: src, dst: filepath.Join(dstRoot, rel)})
	}
	return items, nil
}

func refuseCollision(s Session, target config.ConfigDir, dstRoot string, items []item) error {
	clash := func() error {
		return &refusal{ErrCollision, fmt.Sprintf(
			"%s already has a session with id %s; resume it there, or remove it from %s first",
			target.Name, s.ID, target.Name)}
	}
	// The same id in any project folder of the target counts: Claude Code
	// resolves --resume by id.
	matches, err := filepath.Glob(filepath.Join(dstRoot, "projects", "*", s.ID+".jsonl"))
	if err != nil {
		return err
	}
	if len(matches) > 0 {
		return clash()
	}
	for _, it := range items {
		ok, err := exists(it.dst)
		if err != nil {
			return fmtErr("inspect", it.dst, err)
		}
		if ok {
			return clash()
		}
	}
	return nil
}

// transfer copies items into the target: every item under a temporary name,
// then renamed into place, then verified against the source. It never
// modifies the source. run stops at the first error (or caught signal) and
// leaves the cleanup to undo, so Move can also undo a transfer that
// completed just as a signal arrived.
type transfer struct {
	items   []item
	trap    *sigTrap
	created []string // parent dirs made in the target, outermost first
	renamed []string // item paths renamed into place in the target
}

func (t *transfer) run() error {
	for i := range t.items {
		it := &t.items[i]
		if err := t.trap.check(); err != nil {
			return err
		}
		perm := os.FileMode(0o700)
		if pfi, err := os.Stat(filepath.Dir(it.src)); err == nil {
			perm = pfi.Mode().Perm()
		}
		made, err := mkdirAllTracked(filepath.Dir(it.dst), perm)
		t.created = append(t.created, made...)
		if err != nil {
			return err
		}
		tmp, err := tempName(it.dst)
		if err != nil {
			return err
		}
		it.tmp = tmp
		if err := copyTree(it.src, tmp, t.trap); err != nil {
			return err
		}
	}

	parents := map[string]bool{}
	for i := range t.items {
		it := &t.items[i]
		if err := t.trap.check(); err != nil {
			return err
		}
		if err := fail("rename", it.src); err != nil {
			return err
		}
		// Re-check right before the rename: rename(2) would silently replace
		// a file that appeared since the collision check.
		if ok, err := exists(it.dst); err != nil || ok {
			if err == nil {
				err = fmt.Errorf("%s appeared in the target while copying", it.rel)
			}
			return err
		}
		if err := os.Rename(it.tmp, it.dst); err != nil {
			return err
		}
		t.renamed = append(t.renamed, it.dst)
		it.tmp = ""
		parents[filepath.Dir(it.dst)] = true
	}
	for p := range parents {
		if err := syncDir(p); err != nil {
			return err
		}
	}

	for _, it := range t.items {
		if err := t.trap.check(); err != nil {
			return err
		}
		if err := fail("verify", it.src); err != nil {
			return err
		}
		want, err := statTree(it.src)
		if err != nil {
			return err
		}
		got, err := statTree(it.dst)
		if err != nil {
			return err
		}
		if want != got {
			return fmt.Errorf("verification failed for %s (source %s, copy %s); the source may have changed during the move",
				it.rel, describe(want), describe(got))
		}
	}
	return nil
}

// undo removes what run created in the target: renamed copies, temporaries,
// then the parent dirs it made (only while empty). It never touches the
// source. The error lists what could not be removed.
func (t *transfer) undo() error {
	var problems []error
	for _, p := range t.renamed {
		if err := removeOwned(p); err != nil {
			problems = append(problems, err)
		}
	}
	t.renamed = nil
	for i := range t.items {
		if tmp := t.items[i].tmp; tmp != "" {
			if err := removeOwned(tmp); err != nil {
				problems = append(problems, err)
				continue
			}
			t.items[i].tmp = ""
		}
	}
	for i := len(t.created) - 1; i >= 0; i-- {
		err := os.Remove(t.created[i])
		// Not empty: something else wrote there meanwhile, or a leftover
		// already reported above. Either way the dir is not ours to empty.
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			problems = append(problems, err)
		}
	}
	t.created = nil
	return errors.Join(problems...)
}

func describe(st treeStats) string {
	return strconv.Itoa(st.files) + " files, " + strconv.FormatInt(st.bytes, 10) + " bytes"
}

func removeSource(path string) error {
	if err := fail("delete", path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// plural renders "1 file" / "2 files".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}
