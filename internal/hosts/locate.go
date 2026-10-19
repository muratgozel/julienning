package hosts

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/sessions"
)

// Located is where a project is on this machine.
type Located struct {
	Cwd      string `json:"cwd"`      // the checkout plus the project's sub folder
	Checkout string `json:"checkout"` // "" when the project is only a path
	GitRoot  string `json:"git_root"`
	Branch   string `json:"branch"`
	// How it was found: "claude" (a project Claude used here), "same-path",
	// "search" (the bounded walk), "path" (no repository to verify).
	How string `json:"how"`
}

// LocateOptions configure Locate.
type LocateOptions struct {
	Home       string   // this machine's home; only inside it is searched
	ConfigDirs []string // registered dirs, whose project folders are step 1
	// Branch is preferred among several matches: the session's branch for
	// transfer, the sending checkout's for a forwarded command.
	Branch string
	// Machine names this machine in messages: "box", or "this machine".
	Machine string
	// RanIn says where the project is on the other machine, for messages:
	// "the session ran in ~/Code/shop/web here".
	RanIn string
	// Pick chooses among several equally good checkouts (index into the
	// list); nil without a terminal, which makes that an AMBIGUOUS error.
	Pick func(checkouts []string) (int, error)
}

// Bounds of the walk (vars for tests).
var (
	walkDepth   = 4
	walkTimeout = 5 * time.Second
	walkFolders = 20000
)

// walkSkip are folders never descended into: dependencies and build output.
var walkSkip = map[string]bool{"node_modules": true, "vendor": true, "target": true}

// darwinPrivate are home folders the walk leaves out on macOS, as discovery
// does: stat-ing inside Desktop, Documents or Downloads can pop a privacy
// dialog, and the rest are large media and system folders. A checkout there
// is still found through step 1 or the same path relative to home, which
// are direct lookups.
var darwinPrivate = map[string]bool{
	"Desktop": true, "Documents": true, "Downloads": true, "Library": true,
	"Movies": true, "Music": true, "Pictures": true, "Applications": true,
}

// NotFound is a project with no counterpart on this machine.
type NotFound struct {
	Machine string
	Home    string
	Project *Project
	Checked int  // projects Claude used here that were looked at
	Stopped bool // the walk hit its bound
}

func (e *NotFound) Error() string {
	p := e.Project
	if !p.InRepo() {
		if !p.InHome {
			return fmt.Sprintf("%s is outside the home directory on the other machine, so nothing on %s corresponds to it", p.Cwd, e.Machine)
		}
		return fmt.Sprintf("%s has no %s (%s)", e.Machine, tildeRel(p.HomeRel), e.Detail())
	}
	return fmt.Sprintf("%s has no checkout of %s under %s (%s)", e.Machine, p.Remotes[0], e.Home, e.Detail())
}

// Detail is the parenthetical: what was searched.
func (e *NotFound) Detail() string {
	p := e.Project
	if !p.InRepo() {
		if p.Checkout != "" {
			return "it is in a git repository without a remote, so only the same path is looked for"
		}
		return "it is outside a git repository, so only the same path is looked for"
	}
	names := folderNames(p)
	quoted := strings.Join(names, " or ")
	s := fmt.Sprintf("checked %s Claude used there and folders named %s", plural(e.Checked, "project", "projects"), quoted)
	if e.Stopped {
		s += "; the search stopped after " + walkTimeout.String()
	}
	return s
}

// AmbiguousError is several checkouts and no way to choose.
type AmbiguousError struct {
	Machine   string
	Repo      string
	Checkouts []string // shortened for the machine's home
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%s has several checkouts of %s: %s; run it from a terminal to choose one", e.Machine, e.Repo, strings.Join(e.Checkouts, ", "))
}

// PathMissingError is a checkout without the project's sub folder.
type PathMissingError struct {
	Machine, Checkout, Sub, RanIn string
}

func (e *PathMissingError) Error() string {
	why := ""
	if e.RanIn != "" {
		why = " (" + e.RanIn + ")"
	}
	return fmt.Sprintf("%s has %s but not its %s folder%s; check out the same branch there", e.Machine, e.Checkout, e.Sub, why)
}

// AsAgentError maps Locate's errors to agent error codes.
func AsAgentError(err error) *AgentError {
	switch err.(type) {
	case *NotFound:
		return Errorf(CodeNoCheckout, "%s", err.Error())
	case *AmbiguousError:
		return Errorf(CodeAmbiguous, "%s", err.Error())
	case *PathMissingError:
		return Errorf(CodePathMissing, "%s", err.Error())
	case *AgentError:
		return err.(*AgentError)
	}
	return Errorf(CodeInvalid, "%s", err.Error())
}

// Locate finds p's counterpart on this machine (SPEC "Locating a project"):
// only inside o.Home, stopping at the first step with a match. A candidate
// counts only when it shares a normalized remote URL with p: folder names
// nominate, the repository decides. Errors: *NotFound, *AmbiguousError,
// *PathMissingError.
func Locate(p *Project, o LocateOptions) (Located, error) {
	home := filepath.Clean(o.Home)
	if !p.InRepo() {
		if !p.InHome {
			return Located{}, &NotFound{Machine: o.Machine, Home: home, Project: p}
		}
		target := filepath.Join(home, p.HomeRel)
		if isDir(target) {
			return Located{Cwd: target, How: "path"}, nil
		}
		return Located{}, &NotFound{Machine: o.Machine, Home: home, Project: p}
	}

	nf := &NotFound{Machine: o.Machine, Home: home, Project: p}
	matches, how := claudeProjects(p, o, home, nf)
	if len(matches) == 0 {
		matches, how = byName(p, home, nf)
	}
	if len(matches) == 0 {
		return Located{}, nf
	}
	wt, err := choose(p, o, home, matches)
	if err != nil {
		return Located{}, err
	}
	loc := Located{Checkout: wt.top, Branch: wt.branch(), How: how, Cwd: wt.top}
	if root, ok := sessions.GitRoot(wt.top); ok {
		loc.GitRoot = root
	}
	if p.Sub != "" {
		loc.Cwd = filepath.Join(wt.top, p.Sub)
		if !isDir(loc.Cwd) {
			return Located{}, &PathMissingError{Machine: o.Machine, Checkout: shorten(wt.top, home), Sub: p.Sub, RanIn: o.RanIn}
		}
	}
	return loc, nil
}

// claudeProjects is step 1: the checkouts of the projects Claude used here.
func claudeProjects(p *Project, o LocateOptions, home string, nf *NotFound) ([]worktree, string) {
	seen := map[string]bool{}
	var out []worktree
	for _, cwd := range sessions.RecentCwds(o.ConfigDirs) {
		if _, ok := relInside(home, cwd); !ok {
			continue
		}
		wt, ok := findWorktree(cwd)
		if !ok || seen[wt.top] {
			continue
		}
		if _, ok := relInside(home, wt.top); !ok {
			continue
		}
		seen[wt.top] = true
		nf.Checked++
		if verify(p, wt) {
			out = append(out, wt)
		}
	}
	return out, "claude"
}

// byName is step 2: the same path relative to home, then a bounded
// breadth-first walk for folders named like the repository or the checkout.
func byName(p *Project, home string, nf *NotFound) ([]worktree, string) {
	if rel, ok := p.CheckoutHomeRel(); ok && rel != "" {
		if wt, ok := worktreeAt(filepath.Join(home, rel)); ok && verify(p, wt) {
			return []worktree{wt}, "same-path"
		}
	}
	names := map[string]bool{}
	for _, n := range folderNames(p) {
		names[n] = true
	}
	type node struct {
		dir   string
		depth int
	}
	deadline := time.Now().Add(walkTimeout)
	queue := []node{{home, 0}}
	visited := 0
	var out []worktree
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if visited >= walkFolders || time.Now().After(deadline) {
			nf.Stopped = true
			break
		}
		visited++
		entries, err := os.ReadDir(n.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			// Type() comes from the directory entry: a symlink is never
			// followed, so the walk cannot loop or leave home.
			name := e.Name()
			if !e.IsDir() || strings.HasPrefix(name, ".") || walkSkip[name] {
				continue
			}
			if n.depth == 0 && runtime.GOOS == "darwin" && darwinPrivate[name] {
				continue
			}
			child := filepath.Join(n.dir, name)
			if names[name] {
				if wt, ok := worktreeAt(child); ok && verify(p, wt) {
					out = append(out, wt)
					continue
				}
			}
			if n.depth+1 < walkDepth {
				queue = append(queue, node{child, n.depth + 1})
			}
		}
	}
	return out, "search"
}

// worktreeAt is the worktree whose top is dir itself.
func worktreeAt(dir string) (worktree, bool) {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		return worktree{}, false
	}
	wt, ok := findWorktree(dir)
	if !ok || wt.top != filepath.Clean(dir) {
		return worktree{}, false
	}
	return wt, true
}

func verify(p *Project, wt worktree) bool {
	var remotes []string
	for _, r := range readRemotes(filepath.Join(wt.commonDir, "config")) {
		if n := NormalizeRemote(r.url); n != "" {
			remotes = append(remotes, n)
		}
	}
	return SharesRemote(p.Remotes, remotes)
}

// choose narrows several matches: the preferred branch, then the same path
// relative to home, then o.Pick (none: *AmbiguousError).
func choose(p *Project, o LocateOptions, home string, matches []worktree) (worktree, error) {
	sort.Slice(matches, func(i, j int) bool { return matches[i].top < matches[j].top })
	if len(matches) == 1 {
		return matches[0], nil
	}
	if o.Branch != "" {
		var onBranch []worktree
		for _, m := range matches {
			if m.branch() == o.Branch {
				onBranch = append(onBranch, m)
			}
		}
		if len(onBranch) == 1 {
			return onBranch[0], nil
		}
		if len(onBranch) > 1 {
			matches = onBranch
		}
	}
	if rel, ok := p.CheckoutHomeRel(); ok {
		for _, m := range matches {
			if m.top == filepath.Join(home, rel) {
				return m, nil
			}
		}
	}
	shown := make([]string, len(matches))
	for i, m := range matches {
		shown[i] = shorten(m.top, home)
	}
	if o.Pick == nil {
		return worktree{}, &AmbiguousError{Machine: o.Machine, Repo: p.Remotes[0], Checkouts: shown}
	}
	i, err := o.Pick(shown)
	if err != nil {
		return worktree{}, err
	}
	if i < 0 || i >= len(matches) {
		return worktree{}, fmt.Errorf("no checkout chosen")
	}
	return matches[i], nil
}

// folderNames are the names step 2 looks for: the repository's and this
// checkout folder's.
func folderNames(p *Project) []string {
	var out []string
	for _, n := range []string{p.Repo, p.Folder} {
		if n != "" && n != "." && n != "/" && !contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// shorten renders p with home as "~".
func shorten(p, home string) string {
	if rel, ok := relInside(home, p); ok {
		return tildeRel(rel)
	}
	return p
}

func tildeRel(rel string) string {
	if rel == "" {
		return "~"
	}
	return "~/" + rel
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Check takes a checkout the user typed (SPEC "Locating a project": the
// question after nothing was found): `~` is this machine's home, and it
// need not be inside it, since it is not a search. For a project in a
// repository it must be a checkout sharing a remote URL; the project's sub
// folder must exist in it.
func Check(p *Project, typed string, o LocateOptions) (Located, error) {
	home := filepath.Clean(o.Home)
	dir := strings.TrimSpace(typed)
	switch {
	case dir == "~":
		dir = home
	case strings.HasPrefix(dir, "~/"):
		dir = filepath.Join(home, dir[2:])
	}
	if !filepath.IsAbs(dir) {
		return Located{}, fmt.Errorf("%s is not an absolute path (start it with / or ~/)", typed)
	}
	dir = filepath.Clean(dir)
	if !isDir(dir) {
		return Located{}, fmt.Errorf("%s has no directory %s", o.Machine, shorten(dir, home))
	}
	if !p.InRepo() {
		return Located{Cwd: dir, How: "typed"}, nil
	}
	wt, ok := findWorktree(dir)
	if !ok {
		return Located{}, fmt.Errorf("%s is not a git checkout", shorten(dir, home))
	}
	if !verify(p, wt) {
		var theirs string
		if rs := readRemotes(filepath.Join(wt.commonDir, "config")); len(rs) > 0 {
			theirs = NormalizeRemote(rs[0].url)
		}
		if theirs == "" {
			return Located{}, fmt.Errorf("that checkout has no remote, so it cannot be told apart from %s", p.Remotes[0])
		}
		return Located{}, fmt.Errorf("that is a checkout of %s, not %s", theirs, p.Remotes[0])
	}
	loc := Located{Checkout: wt.top, Branch: wt.branch(), How: "typed", Cwd: wt.top}
	if root, ok := sessions.GitRoot(wt.top); ok {
		loc.GitRoot = root
	}
	if p.Sub != "" {
		loc.Cwd = filepath.Join(wt.top, p.Sub)
		if !isDir(loc.Cwd) {
			return Located{}, &PathMissingError{Machine: o.Machine, Checkout: shorten(wt.top, home), Sub: p.Sub, RanIn: o.RanIn}
		}
	}
	return loc, nil
}
