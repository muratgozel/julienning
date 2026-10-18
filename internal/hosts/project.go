package hosts

import (
	"bufio"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/muratgozel/julienning/internal/sessions"
)

// Project is a working directory as another machine needs it to find its
// own checkout (SPEC "Locating a project"), computed where the directory
// is. All paths are that machine's.
type Project struct {
	Cwd      string `json:"cwd"`
	Checkout string `json:"checkout,omitempty"` // top of the worktree containing Cwd
	GitRoot  string `json:"git_root,omitempty"` // its main worktree's top (the memory key)
	Sub      string `json:"sub,omitempty"`      // Cwd relative to Checkout, "" at the top
	Folder   string `json:"folder,omitempty"`   // Checkout's folder name
	Branch   string `json:"branch,omitempty"`   // from HEAD; "" when detached
	// Remotes are the repository's remote URLs, normalized
	// (NormalizeRemote), origin first.
	Remotes []string `json:"remotes,omitempty"`
	// Repo is the last element of the first remote URL.
	Repo string `json:"repo,omitempty"`
	// CloneURL is origin's URL as written, for messages.
	CloneURL string `json:"clone_url,omitempty"`
	// InHome and HomeRel: Cwd relative to home ("" for home itself).
	InHome  bool   `json:"in_home"`
	HomeRel string `json:"home_rel,omitempty"`
}

// InRepo reports whether the project can be verified by a remote URL; a
// directory outside git, or in a repository without remotes, is only its
// path relative to home.
func (p *Project) InRepo() bool { return p.Checkout != "" && len(p.Remotes) > 0 }

// CheckoutHomeRel is the checkout's path relative to home ("" when the
// checkout is not inside home).
func (p *Project) CheckoutHomeRel() (string, bool) {
	if !p.InHome || p.Checkout == "" {
		return "", false
	}
	rel := p.HomeRel
	if p.Sub != "" {
		if !strings.HasSuffix(rel, p.Sub) {
			return "", false
		}
		rel = strings.TrimSuffix(strings.TrimSuffix(rel, p.Sub), "/")
	}
	return rel, true
}

// ProjectOf computes the project of dir (absolute, cleaned) on this machine.
func ProjectOf(dir, home string) *Project {
	dir = filepath.Clean(dir)
	p := &Project{Cwd: dir}
	if rel, ok := relInside(home, dir); ok {
		p.InHome, p.HomeRel = true, rel
	}
	wt, ok := findWorktree(dir)
	if !ok {
		return p
	}
	p.Checkout = wt.top
	p.Folder = filepath.Base(wt.top)
	if sub, ok := relInside(wt.top, dir); ok {
		p.Sub = sub
	}
	if root, ok := sessions.GitRoot(dir); ok {
		p.GitRoot = root
	}
	p.Branch = wt.branch()
	remotes := readRemotes(filepath.Join(wt.commonDir, "config"))
	for _, r := range remotes {
		if n := NormalizeRemote(r.url); n != "" && !contains(p.Remotes, n) {
			p.Remotes = append(p.Remotes, n)
		}
	}
	if len(remotes) > 0 {
		p.CloneURL = remotes[0].url
	}
	if len(p.Remotes) > 0 {
		p.Repo = repoName(p.Remotes[0])
	}
	return p
}

// relInside returns path relative to base when path is base or below it
// ("" for base itself).
func relInside(base, path string) (string, bool) {
	if base == "" {
		return "", false
	}
	base, path = filepath.Clean(base), filepath.Clean(path)
	if path == base {
		return "", true
	}
	if strings.HasPrefix(path, base+string(filepath.Separator)) {
		return path[len(base)+1:], true
	}
	return "", false
}

// worktree is a git working tree found by reading files, never by running
// git (it may not be installed).
type worktree struct {
	top       string // the directory holding .git
	gitDir    string // its git dir (a linked worktree's own)
	commonDir string // the shared repository (config, refs)
}

// findWorktree walks up from dir to the first .git (directory or
// "gitdir:" file).
func findWorktree(dir string) (worktree, bool) {
	for d := dir; ; d = filepath.Dir(d) {
		gitPath := filepath.Join(d, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			switch {
			case fi.IsDir():
				return worktree{top: d, gitDir: gitPath, commonDir: gitPath}, true
			case fi.Mode().IsRegular():
				gd := gitDirOf(d, gitPath)
				if gd == "" {
					return worktree{}, false
				}
				return worktree{top: d, gitDir: gd, commonDir: commonDirOf(gd)}, true
			}
		}
		if filepath.Dir(d) == d {
			return worktree{}, false
		}
	}
}

func gitDirOf(top, gitFile string) string {
	line := firstLine(gitFile)
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	gd := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if gd == "" {
		return ""
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(top, gd)
	}
	return filepath.Clean(gd)
}

func commonDirOf(gitDir string) string {
	common := firstLine(filepath.Join(gitDir, "commondir"))
	if common == "" {
		return gitDir
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	return filepath.Clean(common)
}

// branch is HEAD's branch, "" when HEAD is detached or unreadable.
func (w worktree) branch() string {
	line := firstLine(filepath.Join(w.gitDir, "HEAD"))
	if b, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
		return strings.TrimSpace(b)
	}
	return ""
}

func firstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, _ := io.ReadAll(io.LimitReader(f, 4<<10))
	line, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(line)
}

type remote struct{ name, url string }

var sectionRe = regexp.MustCompile(`^\[\s*remote\s+"((?:[^"\\]|\\.)*)"\s*\]`)

// readRemotes reads every url of every [remote "…"] section of a git config
// file, origin first, then in file order. url.*.insteadOf is ignored, as
// SPEC says: the URL written is what identifies the repository.
func readRemotes(path string) []remote {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []remote
	current, inRemote := "", false
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			m := sectionRe.FindStringSubmatch(line)
			inRemote = m != nil
			if inRemote {
				current = m[1]
			}
			continue
		}
		if !inRemote {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		if v := configValue(val); v != "" {
			out = append(out, remote{name: current, url: v})
		}
	}
	// origin first, the rest in file order.
	var sorted []remote
	for _, r := range out {
		if r.name == "origin" {
			sorted = append(sorted, r)
		}
	}
	for _, r := range out {
		if r.name != "origin" {
			sorted = append(sorted, r)
		}
	}
	return sorted
}

// configValue unquotes a git config value and drops an inline comment.
func configValue(v string) string {
	v = strings.TrimSpace(v)
	var b strings.Builder
	quoted := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			quoted = !quoted
		case c == '\\' && i+1 < len(v):
			i++
			b.WriteByte(v[i])
		case (c == '#' || c == ';') && !quoted:
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

var scpRe = regexp.MustCompile(`^(?:[^@/:]+@)?([^/:]+):(.+)$`)

// NormalizeRemote turns a remote URL into a comparable form: scheme, user
// and port dropped, host lowercased, the scp form user@host:path read as
// host/path, a trailing "/" and ".git" removed.
// git@github.com:muratgozel/julienning.git and
// https://github.com/muratgozel/julienning are both
// github.com/muratgozel/julienning. A local path stays a path.
func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		host, path = strings.ToLower(u.Hostname()), u.Path
	case scpRe.MatchString(raw) && !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "."):
		m := scpRe.FindStringSubmatch(raw)
		host, path = strings.ToLower(m[1]), m[2]
	default:
		path = filepath.Clean(raw)
	}
	path = strings.TrimRight(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimRight(path, "/")
	if host == "" {
		return path
	}
	return host + "/" + strings.TrimLeft(path, "/")
}

func repoName(normalized string) string {
	return filepath.Base(normalized)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SharesRemote reports whether two remote lists name the same repository.
func SharesRemote(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}
