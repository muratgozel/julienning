package sessions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
)

// DefaultLimit is how many sessions List returns when Limit is not set. The
// CLI's --limit default mirrors it (switching.parseFlags).
const DefaultLimit = 100

// maxOpen caps how many candidate files one listing opens. Sessions without
// a title or prompt (opened and left empty) are skipped, so walking past
// `limit` candidates is expected; the cap keeps a folder full of them from
// turning a listing into a full scan. Skipped files are tiny in practice, so
// the worst case stays cheap: 420 opens for the default 100, 2020 for the
// CLI maximum of 500, each reading at most a head and a tail chunk.
func maxOpen(limit int) int { return limit*4 + 20 }

// readMetaFn is readMeta; tests swap it to count the files a listing opens.
var readMetaFn = readMeta

// ListOptions selects which sessions List returns.
type ListOptions struct {
	Dirs  []config.ConfigDir // registered dirs to scan (any email)
	Cwd   string             // project to list; ignored with All
	All   bool               // every project folder instead of Cwd's
	Limit int                // newest N across all dirs; <= 0 means DefaultLimit
	Now   time.Time          // reference for MaybeOpen; zero means time.Now()
}

// candidate is a session file picked by mtime before anything is opened.
type candidate struct {
	cd      config.ConfigDir
	project string
	id      string
	path    string
	mtime   time.Time
	size    int64
	// confirm: the folder was matched by the 200-character prefix of a long
	// cwd, so the session's own cwd must be one of these to count.
	confirm []string
}

// List returns up to Limit sessions, newest first (by last activity).
//
// Per-dir and per-file problems (unreadable folders, files vanishing while
// we look) do not stop the listing: the sessions found are returned together
// with a non-nil error joining the problems, so callers can warn and go on.
func List(opts ListOptions) ([]Session, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var problems []error

	var cands []candidate
	for _, cd := range opts.Dirs {
		cs, err := candidates(cd, opts)
		if err != nil {
			problems = append(problems, err)
		}
		cands = append(cands, cs...)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if !cands[i].mtime.Equal(cands[j].mtime) {
			return cands[i].mtime.After(cands[j].mtime)
		}
		return cands[i].path < cands[j].path
	})

	live, err := liveIndex(opts.Dirs)
	if err != nil {
		problems = append(problems, err)
	}
	hasRegistry := map[string]bool{}
	for _, cd := range opts.Dirs {
		hasRegistry[cd.Dir] = livesess.RegistryExists(cd.Dir)
	}

	opens := maxOpen(limit)
	var out []Session
	for i, c := range cands {
		if len(out) >= limit || i >= opens {
			break
		}
		m, err := readMetaFn(c.path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) { // deleted since ReadDir: not a problem
				problems = append(problems, fmtErr("read", c.path, err))
			}
			continue
		}
		if c.confirm != nil && !contains(c.confirm, filepath.Clean(m.cwd)) {
			continue
		}
		if m.title == "" && m.firstPrompt == "" {
			continue
		}
		s := Session{
			ID:          c.id,
			ConfigName:  c.cd.Name,
			ConfigDir:   c.cd.Dir,
			ProjectDir:  c.project,
			Cwd:         m.cwd,
			Title:       m.title,
			FirstPrompt: m.firstPrompt,
			LastActive:  m.lastActive,
			ModTime:     c.mtime,
			Size:        c.size,
		}
		if s.LastActive.IsZero() {
			s.LastActive = c.mtime
		}
		if in, ok := live[c.id]; ok {
			s.Live, s.LiveIn = true, in
		}
		s.MaybeOpen = !s.Live && !hasRegistry[c.cd.Dir] && now.Sub(c.mtime) < RecentWindow
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastActive.After(out[j].LastActive)
	})
	return out, errors.Join(problems...)
}

// candidates returns the session files of one config dir for opts.
func candidates(cd config.ConfigDir, opts ListOptions) ([]candidate, error) {
	root := filepath.Join(cd.Dir, "projects")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmtErr("read", root, err)
	}
	type folder struct {
		name    string
		confirm []string
	}
	var folders []folder
	if opts.All {
		for _, e := range entries {
			if e.IsDir() && validProjectDir(e.Name()) {
				folders = append(folders, folder{name: e.Name()})
			}
		}
	} else {
		for _, cwd := range cwdVariants(opts.Cwd) {
			enc := EncodeCwd(cwd)
			if len(enc) <= maxEncodedLen {
				folders = append(folders, folder{name: enc})
				continue
			}
			// Claude Code cuts long names at 200 characters and appends a
			// hash we cannot reproduce; match the prefix and let each
			// session's cwd confirm it belongs to this project.
			prefix := enc[:maxEncodedLen]
			for _, e := range entries {
				if e.IsDir() && len(e.Name()) > maxEncodedLen && strings.HasPrefix(e.Name(), prefix) {
					folders = append(folders, folder{name: e.Name(), confirm: cwdVariants(opts.Cwd)})
				}
			}
		}
	}

	seen := map[string]bool{}
	var out []candidate
	var problems []error
	for _, fo := range folders {
		if seen[fo.name] {
			continue
		}
		seen[fo.name] = true
		dir := filepath.Join(root, fo.name)
		files, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, fmtErr("read", dir, err))
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !ValidID(id) {
				continue
			}
			fi, err := f.Info()
			if err != nil {
				continue // removed between ReadDir and Info
			}
			out = append(out, candidate{
				cd: cd, project: fo.name, id: id,
				path:  filepath.Join(dir, name),
				mtime: fi.ModTime(), size: fi.Size(),
				confirm: fo.confirm,
			})
		}
	}
	return out, errors.Join(problems...)
}

// cwdVariants returns cwd and, when different, its symlink-free form: Claude
// Code records the real path (getcwd), while a shell's $PWD may go through a
// symlink such as /tmp → /private/tmp.
func cwdVariants(cwd string) []string {
	if cwd == "" {
		return nil
	}
	cwd = filepath.Clean(cwd)
	out := []string{cwd}
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		out = append(out, real)
	}
	return out
}

// liveIndex maps session id → config name of the dir whose registry has it
// open.
func liveIndex(dirs []config.ConfigDir) (map[string]string, error) {
	out := map[string]string{}
	var problems []error
	for _, cd := range dirs {
		ls, err := livesess.List(cd.Dir)
		if err != nil {
			problems = append(problems, fmt.Errorf("live sessions of %s: %w", cd.Name, err))
			continue
		}
		for _, l := range ls {
			if _, dup := out[l.SessionID]; !dup {
				out[l.SessionID] = cd.Name
			}
		}
	}
	return out, errors.Join(problems...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
