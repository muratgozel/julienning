package sessions

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
)

// ArchiveFormat is the version of the export header and archive (SPEC
// "Moving a session between machines").
//
// CRITICAL release rule: a change to the archive format bumps julienning's
// minor version, like a change to the agent protocol.
const ArchiveFormat = 1

// ManifestName is the archive's last entry.
const ManifestName = "MANIFEST.json"

// Refusal kinds of the cross-machine move, besides Move's (ErrLive,
// ErrMaybeOpen, ErrCollision, ErrGone); test with errors.Is.
var (
	// ErrInvalid: the archive breaks a rule (a path outside the session's
	// items, a link, a size or checksum that does not match the manifest).
	ErrInvalid = errors.New("invalid archive")
	// ErrChanged: the source changed between the pack and the release.
	ErrChanged = errors.New("session changed while it was being sent")
	// ErrNoSpace: the destination ran out of space.
	ErrNoSpace = errors.New("no space left")
)

func invalid(format string, a ...any) error {
	return &refusal{ErrInvalid, fmt.Sprintf(format, a...)}
}

// PackSource is where the session is on the source machine: what the
// destination rewrites paths from.
type PackSource struct {
	Home          string `json:"home"`
	ConfigDir     string `json:"config_dir"`
	ProjectFolder string `json:"project_folder"`
	MemoryFolder  string `json:"memory_folder,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	Checkout      string `json:"checkout,omitempty"`      // as written ("" outside git: the cwd is the project)
	CheckoutReal  string `json:"checkout_real,omitempty"` // with symlinks resolved
	GitRoot       string `json:"git_root,omitempty"`
}

// PackHeader is the line before the archive.
type PackHeader struct {
	Proto         int        `json:"proto"` // ArchiveFormat
	ID            string     `json:"id"`
	Title         string     `json:"title,omitempty"`
	Source        PackSource `json:"source"`
	ClaudeVersion string     `json:"claude_version,omitempty"`
	GitBranch     string     `json:"git_branch,omitempty"`
	// Bytes is the size of every file in the archive: the receiver's bound
	// before the manifest arrives, and the progress total.
	Bytes int64 `json:"bytes"`
}

// ManifestEntry describes one archive entry, as packed.
type ManifestEntry struct {
	Path   string    `json:"path"`
	Type   string    `json:"type"` // file, dir, symlink
	Mode   uint32    `json:"mode"`
	MTime  time.Time `json:"mtime"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256,omitempty"`
	Link   string    `json:"link,omitempty"`
}

// Manifest is the archive's last entry. Its SHA-256 identifies the pack:
// the receipt records it, and Release compares a fresh pack with it.
type Manifest struct {
	ID      string          `json:"id"`
	Entries []ManifestEntry `json:"entries"`
}

// forbiddenName reports what can never travel in an archive: Claude's
// login, credentials, live-session registry and session keys.
func forbiddenName(rel string) bool {
	base := path.Base(rel)
	return base == claudecfg.AccountFile || base == ".credentials.json" || strings.HasSuffix(base, ".key") ||
		rel == "sessions" || strings.HasPrefix(rel, "sessions/")
}

// packItem is one item root to pack, relative to the config dir.
type packItem struct{ rel, src string }

// packItems lists what moves for s: Move's items, then its project memory
// folder (merged on the destination, never moved).
func packItems(s Session, srcRoot string) ([]packItem, string, error) {
	jsonlRel := filepath.Join("projects", s.ProjectDir, s.ID+".jsonl")
	items, err := collectItems(s, srcRoot, srcRoot, jsonlRel)
	if err != nil {
		return nil, "", err
	}
	var out []packItem
	for _, it := range items {
		out = append(out, packItem{rel: filepath.ToSlash(it.rel), src: it.src})
	}
	mem, err := memoryProject(s, srcRoot)
	if err != nil {
		return nil, "", err
	}
	if mem != "" {
		dir := filepath.Join(srcRoot, "projects", mem, "memory")
		if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
			out = append(out, packItem{rel: "projects/" + mem + "/memory", src: dir})
		}
	}
	return out, mem, nil
}

// refuseIfOpenHere is Move's refusal for one machine: open in the dir's
// registry, or changed in the last two minutes without one.
func refuseIfOpenHere(s Session, root string, mtime time.Time) error {
	ls, err := livesess.List(root)
	if err != nil {
		return fmt.Errorf("cannot check whether session %s is open: %w", s.ShortID(), err)
	}
	for _, l := range ls {
		if l.SessionID == s.ID {
			return &refusal{ErrLive, fmt.Sprintf("session %q is open in another terminal; exit it there first", s.Label())}
		}
	}
	if !livesess.RegistryExists(root) && now().Sub(mtime) < RecentWindow {
		return &refusal{ErrMaybeOpen, fmt.Sprintf(
			"session %q changed less than %d minutes ago and %s has no live-session registry, so it may still be open; exit it or try again shortly",
			s.Label(), int(RecentWindow/time.Minute), s.ConfigName)}
	}
	return nil
}

// Pack writes s to w (SPEC "Moving a session between machines", step 1):
// the header line, then a gzip'd tar of its items whose last entry is the
// manifest, computed while streaming. It is read-only. src says where the
// session is (the caller knows the checkout); its ConfigDir,
// ProjectFolder and MemoryFolder are filled in here.
func Pack(s Session, src PackSource, w io.Writer) (PackHeader, string, error) {
	h := PackHeader{Proto: ArchiveFormat, ID: s.ID, Title: s.Label(), ClaudeVersion: s.ClaudeVersion, GitBranch: s.GitBranch}
	if !ValidID(s.ID) || !validProjectDir(s.ProjectDir) {
		return h, "", fmt.Errorf("invalid session %q in %q", s.ID, s.ProjectDir)
	}
	srcRoot := filepath.Clean(s.ConfigDir)
	jsonl := filepath.Join(srcRoot, "projects", s.ProjectDir, s.ID+".jsonl")
	fi, err := os.Lstat(jsonl)
	if errors.Is(err, os.ErrNotExist) {
		return h, "", &refusal{ErrGone, fmt.Sprintf("session %s is no longer in %s (was it moved or deleted?)", s.ShortID(), s.ConfigName)}
	}
	if err != nil {
		return h, "", fmtErr("inspect", jsonl, err)
	}
	if err := refuseIfOpenHere(s, srcRoot, fi.ModTime()); err != nil {
		return h, "", err
	}
	items, mem, err := packItems(s, srcRoot)
	if err != nil {
		return h, "", err
	}
	src.ConfigDir, src.ProjectFolder, src.MemoryFolder = srcRoot, s.ProjectDir, mem
	h.Source = src
	for _, it := range items {
		st, err := statTree(it.src)
		if err != nil {
			return h, "", err
		}
		h.Bytes += st.bytes
	}

	raw, err := json.Marshal(h)
	if err != nil {
		return h, "", err
	}
	if _, err := w.Write(append(raw, '\n')); err != nil {
		return h, "", err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	var m Manifest
	m.ID = s.ID
	for _, it := range items {
		if err := packTree(tw, it.src, it.rel, &m); err != nil {
			return h, "", err
		}
	}
	mraw, err := json.Marshal(m)
	if err != nil {
		return h, "", err
	}
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(mraw)), ModTime: time.Unix(0, 0)}); err != nil {
		return h, "", err
	}
	if _, err := tw.Write(mraw); err != nil {
		return h, "", err
	}
	if err := tw.Close(); err != nil {
		return h, "", err
	}
	if err := gz.Close(); err != nil {
		return h, "", err
	}
	sum := sha256.Sum256(mraw)
	return h, hex.EncodeToString(sum[:]), nil
}

// packTree writes src (file, symlink or directory, never followed) as rel.
func packTree(tw *tar.Writer, src, rel string, m *Manifest) error {
	if forbiddenName(rel) {
		return fmt.Errorf("refusing to send %s", rel)
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	e := ManifestEntry{Path: rel, Mode: uint32(fi.Mode().Perm()), MTime: fi.ModTime().UTC()}
	hdr := &tar.Header{Name: rel, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime(), Format: tar.FormatPAX}
	switch {
	case fi.Mode().IsRegular():
		e.Type, e.Size = "file", fi.Size()
		hdr.Typeflag, hdr.Size = tar.TypeReg, fi.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, fi.Size()))
		f.Close()
		if err != nil {
			return err
		}
		if n != fi.Size() {
			return &refusal{ErrChanged, fmt.Sprintf("%s changed while it was being packed", rel)}
		}
		e.SHA256 = hex.EncodeToString(h.Sum(nil))
		m.Entries = append(m.Entries, e)
		return nil
	case fi.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		e.Type, e.Link = "symlink", link
		hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, link
		m.Entries = append(m.Entries, e)
		return tw.WriteHeader(hdr)
	case fi.IsDir():
		e.Type = "dir"
		hdr.Typeflag, hdr.Name = tar.TypeDir, rel+"/"
		m.Entries = append(m.Entries, e)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, c := range entries {
			if err := packTree(tw, filepath.Join(src, c.Name()), rel+"/"+c.Name(), m); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%s: unsupported file type %s", src, fi.Mode().Type())
}

// --- receive ---

// UnpackOptions are the destination's side of a move.
type UnpackOptions struct {
	Dir   string // the account dir the session goes into
	Email string // it must still be logged into this account
	Cwd   string // the session's new working directory (real path)
	// Checkout is the destination's checkout of the project ("" outside
	// git, where Cwd is the project); GitRoot its main worktree (the memory
	// key, "" outside git).
	Checkout, GitRoot string
	Home              string
	ReceivedDir       string // receipts of received sessions
	From              string // the sending machine, for the receipt
	Now               time.Time
}

// UnpackResult is what the destination reports.
type UnpackResult struct {
	Committed bool `json:"committed"`
	// Already: the id is here with a receipt of this very pack (a transfer
	// whose answer was lost after the commit); nothing was written.
	Already bool `json:"already"`
	// ManifestSHA256 identifies what was received; the source releases
	// against it.
	ManifestSHA256 string       `json:"manifest_sha256"`
	Header         PackHeader   `json:"-"`
	ProjectFolder  string       `json:"project_folder"`
	Rewrites       int          `json:"rewrites"`
	Memory         MemoryReport `json:"memory"`
	Warnings       []string     `json:"warnings,omitempty"`
}

// Receipt records a received session (received/<id>.json).
type Receipt struct {
	ManifestSHA256   string    `json:"manifest_sha256"`
	TranscriptSHA256 string    `json:"transcript_sha256"`
	From             string    `json:"from"`
	At               time.Time `json:"at"`
}

// ReceiptTTL is how long receipts are kept.
const ReceiptTTL = 30 * 24 * time.Hour

// Limits of what a receiver accepts (vars for tests).
var (
	maxHeader       = 1 << 20
	maxManifest     = int64(32 << 20)
	maxEntries      = 200000
	maxArchiveBytes = int64(4 << 30)
)

// ReadHeader reads the header line before an archive.
func ReadHeader(r *bufio.Reader) (PackHeader, error) {
	var h PackHeader
	line, err := readLine(r, maxHeader)
	if err != nil {
		return h, invalid("no archive header: %v", err)
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return h, invalid("the archive header is not JSON: %v", err)
	}
	switch {
	case h.Proto != ArchiveFormat:
		return h, invalid("archive format %d, this julienning reads %d (update the older julienning)", h.Proto, ArchiveFormat)
	case !ValidID(h.ID):
		return h, invalid("invalid session id %q", h.ID)
	case !validProjectDir(h.Source.ProjectFolder) || (h.Source.MemoryFolder != "" && !validProjectDir(h.Source.MemoryFolder)):
		return h, invalid("invalid project folder in the header")
	case h.Bytes < 0 || h.Bytes > maxArchiveBytes:
		return h, invalid("the archive claims %d bytes", h.Bytes)
	}
	for _, p := range []string{h.Source.Home, h.Source.ConfigDir} {
		if !filepath.IsAbs(p) {
			return h, invalid("the header's paths must be absolute")
		}
	}
	return h, nil
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, fmt.Errorf("line longer than %d bytes", max)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// receivedItem is one item root being written on the destination.
type receivedItem struct {
	dst, tmp string // final path and temporary sibling
	memory   bool   // the memory staging dir: merged, never renamed
}

// receiver writes an archive's entries under temporary names.
type receiver struct {
	o       UnpackOptions
	h       PackHeader
	dryRun  bool // checking a possible retry: hash, never write
	roots   map[string]*receivedItem
	order   []string
	created []string // parent dirs made, outermost first
	renamed []string
	links   map[string]bool // archive paths that are symlinks
	got     map[string]ManifestEntry
	files   []string // written regular files, relative archive paths
	total   int64
	trap    *sigTrap

	dstFolder, memFolder string
}

// Unpack receives a session (SPEC "Moving a session between machines",
// step 2): checks the destination, validates every entry before it is
// written, writes under temporary names, checks the bytes against the
// manifest, rewrites paths, renames into place, verifies, then commits (a
// receipt) and merges project memory. Anything before the commit is undone.
func Unpack(r io.Reader, o UnpackOptions) (UnpackResult, error) {
	var res UnpackResult
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 64<<10)
	}
	h, err := ReadHeader(br)
	if err != nil {
		return res, err
	}
	res.Header = h
	o.Dir = filepath.Clean(o.Dir)
	if email, err := claudecfg.ReadEmail(o.Dir); err != nil || !strings.EqualFold(email, o.Email) {
		return res, fmt.Errorf("%s is no longer logged into %s; nothing was received", o.Dir, o.Email)
	}
	if fi, err := os.Stat(o.Cwd); err != nil || !fi.IsDir() {
		return res, fmt.Errorf("the session's directory %s does not exist here", o.Cwd)
	}
	pruneReceipts(o.ReceivedDir, o.Now)
	ls, err := livesess.List(o.Dir)
	if err != nil {
		return res, fmt.Errorf("cannot check whether session %s is open here: %w", h.ID, err)
	}
	for _, l := range ls {
		if l.SessionID == h.ID {
			return res, &refusal{ErrLive, fmt.Sprintf("session %s is open here; exit it first", h.ID)}
		}
	}

	rv := &receiver{o: o, h: h, roots: map[string]*receivedItem{}, links: map[string]bool{}, got: map[string]ManifestEntry{}}
	existing, _ := filepath.Glob(filepath.Join(o.Dir, "projects", "*", h.ID+".jsonl"))
	var receipt Receipt
	if len(existing) > 0 {
		// The id is here: a retry after an answer lost following the
		// commit, when a receipt of this pack says so (checked once the
		// manifest is in), else a collision.
		rc, ok := loadReceipt(o.ReceivedDir, h.ID)
		sum, err := fileSHA256(existing[0])
		if !ok || err != nil || sum != rc.TranscriptSHA256 {
			return res, &refusal{ErrCollision, fmt.Sprintf("this machine already has a session with id %s in %s; resume it here, or remove it first", h.ID, o.Dir)}
		}
		receipt, rv.dryRun = rc, true
	}
	if !rv.dryRun {
		if rv.dstFolder, err = projectFolderFor(o.Dir, o.Cwd); err != nil {
			return res, err
		}
		rv.memFolder = rv.dstFolder
		if o.GitRoot != "" {
			rv.memFolder = memoryFolderFor(o.Dir, o.GitRoot)
		}
		res.ProjectFolder = rv.dstFolder
		rv.trap = trapSignals()
		defer rv.trap.stop()
	}

	manifest, mraw, err := rv.read(br)
	if err == nil {
		err = rv.check(manifest)
	}
	if err != nil {
		return res, rv.fail(err)
	}
	sum := sha256.Sum256(mraw)
	msha := hex.EncodeToString(sum[:])
	res.ManifestSHA256 = msha
	if rv.dryRun {
		if msha != receipt.ManifestSHA256 {
			return res, &refusal{ErrCollision, fmt.Sprintf("this machine already has a session with id %s in %s; resume it here, or remove it first", h.ID, o.Dir)}
		}
		res.Already = true
		return res, nil
	}
	rw := NewRewriter(rv.pairs())
	if res.Rewrites, err = rv.rewrite(rw); err != nil {
		return res, rv.fail(err)
	}
	if err := rv.commitRenames(); err != nil {
		return res, rv.fail(err)
	}
	if err := rv.trap.check(); err != nil {
		return res, rv.fail(err)
	}

	// Committed: the session is in place. Only now the receipt, and only
	// after it does the source release.
	transcript := filepath.Join(o.Dir, "projects", rv.dstFolder, h.ID+".jsonl")
	tsum, err := fileSHA256(transcript)
	if err != nil {
		return res, rv.fail(err)
	}
	if err := saveReceipt(o.ReceivedDir, h.ID, Receipt{ManifestSHA256: msha, TranscriptSHA256: tsum, From: o.From, At: o.Now.UTC()}); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not write the receipt (%v); a retry after a lost connection will report a collision", err))
	}
	res.Committed = true
	if it := rv.roots["memory"]; it != nil {
		dst := filepath.Join(o.Dir, "projects", rv.memFolder, "memory")
		mem, err := mergeMemory(it.tmp, dst)
		res.Memory = mem
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("project memory not fully merged: %v", err))
		}
		if err := removeOwned(it.tmp); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not remove %s: %v", it.tmp, err))
		}
	}
	return res, nil
}

// fail undoes what was written and returns err, keeping the cleanup's own
// problems in the message: leftovers would make a retry look like a
// collision.
func (rv *receiver) fail(err error) error {
	if rv.dryRun {
		return err
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		err = &refusal{ErrNoSpace, fmt.Sprintf("no space left on this machine for session %s (%v); nothing was received", rv.h.ID, err)}
	}
	if cerr := rv.undo(); cerr != nil {
		return fmt.Errorf("%w; the partial copy could not be fully removed (%v); delete those leftovers by hand before trying again", err, cerr)
	}
	return err
}

// destination maps an archive path to its item root and the path inside
// it. Only this session's items are accepted.
func (rv *receiver) destination(rel string) (root, dst string, memory bool, err error) {
	h := rv.h
	id := h.ID
	segs := strings.Split(rel, "/")
	switch {
	case len(segs) == 3 && segs[0] == "projects" && segs[1] == h.Source.ProjectFolder && segs[2] == id+".jsonl":
		return rel, filepath.Join(rv.o.Dir, "projects", rv.dstFolder, id+".jsonl"), false, nil
	case len(segs) >= 3 && segs[0] == "projects" && segs[1] == h.Source.ProjectFolder && segs[2] == id:
		root := "projects/" + segs[1] + "/" + id
		return root, filepath.Join(rv.o.Dir, "projects", rv.dstFolder, filepath.FromSlash(strings.Join(segs[2:], "/"))), false, nil
	case len(segs) >= 4 && segs[0] == "projects" && h.Source.MemoryFolder != "" && segs[1] == h.Source.MemoryFolder && segs[2] == "memory":
		return "memory", filepath.FromSlash(strings.Join(segs[3:], "/")), true, nil
	case len(segs) == 3 && segs[0] == "projects" && h.Source.MemoryFolder != "" && segs[1] == h.Source.MemoryFolder && segs[2] == "memory":
		return "memory", "", true, nil
	case len(segs) >= 2 && (segs[0] == "file-history" || segs[0] == "session-env" || segs[0] == "tasks") && segs[1] == id:
		return segs[0] + "/" + id, filepath.Join(rv.o.Dir, filepath.FromSlash(rel)), false, nil
	case len(segs) == 2 && segs[0] == "todos" && strings.HasPrefix(segs[1], id+"-") && strings.HasSuffix(segs[1], ".json") && !strings.Contains(segs[1], "/"):
		return rel, filepath.Join(rv.o.Dir, "todos", segs[1]), false, nil
	case len(segs) == 2 && segs[0] == "debug" && segs[1] == id+".txt":
		return rel, filepath.Join(rv.o.Dir, "debug", segs[1]), false, nil
	}
	return "", "", false, invalid("the archive holds %s, which is not part of session %s", rel, id)
}

// read consumes the archive: every entry validated, then written (or, in
// dryRun, only hashed); the manifest must be the last entry.
func (rv *receiver) read(br *bufio.Reader) (Manifest, []byte, error) {
	var m Manifest
	var in io.Reader = br
	if rv.trap != nil {
		in = trapReader{r: br, trap: rv.trap}
	}
	gz, err := gzip.NewReader(in)
	if err != nil {
		return m, nil, invalid("the archive is not gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var mraw []byte
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, nil, fmt.Errorf("the archive ended early (%v): the connection may have dropped", err)
		}
		if mraw != nil {
			return m, nil, invalid("the archive has entries after its manifest")
		}
		if count++; count > maxEntries {
			return m, nil, invalid("the archive has more than %d entries", maxEntries)
		}
		if hdr.Name == ManifestName {
			if hdr.Typeflag != tar.TypeReg || hdr.Size > maxManifest {
				return m, nil, invalid("the manifest is not a regular file under %d bytes", maxManifest)
			}
			if mraw, err = io.ReadAll(io.LimitReader(tr, hdr.Size)); err != nil {
				return m, nil, fmt.Errorf("read the manifest: %w", err)
			}
			continue
		}
		if err := rv.entry(hdr, tr); err != nil {
			return m, nil, err
		}
	}
	if mraw == nil {
		return m, nil, fmt.Errorf("the archive has no manifest: the connection may have dropped")
	}
	if err := json.Unmarshal(mraw, &m); err != nil {
		return m, nil, invalid("the manifest is not JSON: %v", err)
	}
	return m, mraw, nil
}

func (rv *receiver) entry(hdr *tar.Header, tr *tar.Reader) error {
	rel := strings.TrimSuffix(hdr.Name, "/")
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return invalid("unsafe path %q in the archive", hdr.Name)
	}
	if forbiddenName(rel) {
		return invalid("the archive holds %s, which never travels", rel)
	}
	for p := path.Dir(rel); p != "." && p != "/"; p = path.Dir(p) {
		if rv.links[p] {
			return invalid("the archive writes %s through its own symlink %s", rel, p)
		}
	}
	rootKey, dst, memory, err := rv.destination(rel)
	if err != nil {
		return err
	}
	e := ManifestEntry{Path: rel, Mode: uint32(hdr.Mode) & 0o777, MTime: hdr.ModTime.UTC()}
	switch hdr.Typeflag {
	case tar.TypeDir:
		e.Type = "dir"
	case tar.TypeReg:
		e.Type, e.Size = "file", hdr.Size
	case tar.TypeSymlink:
		// The memory merge reads what it copies: a link there would copy
		// whatever it points to on this machine into the memory folder.
		if memory {
			return invalid("%s is a symlink; project memory travels as plain files only", rel)
		}
		e.Type, e.Link = "symlink", hdr.Linkname
		rv.links[rel] = true
	default:
		return invalid("%s is a %s; only files, directories and symlinks travel", rel, typeName(hdr.Typeflag))
	}
	if _, dup := rv.got[rel]; dup {
		return invalid("the archive holds %s twice", rel)
	}

	var written string // the file on disk, when one is written
	if !rv.dryRun {
		it, err := rv.root(rootKey, dst, rel, memory, e.Type)
		if err != nil {
			return err
		}
		target := rv.inTemp(it, rootKey, rel, dst, memory)
		switch e.Type {
		case "dir":
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case "symlink":
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case "file":
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			written = target
		}
	}
	if e.Type == "file" {
		if rv.total+hdr.Size > rv.h.Bytes {
			return invalid("the archive sends more than the %d bytes its header announced", rv.h.Bytes)
		}
		rv.total += hdr.Size
		sum, err := writeEntry(tr, written, hdr.Size, os.FileMode(e.Mode))
		if err != nil {
			return err
		}
		e.SHA256 = sum
		if written != "" {
			rv.files = append(rv.files, rel)
		}
	}
	rv.got[rel] = e
	return nil
}

// writeEntry copies size bytes from r to a new file at dst ("" only
// hashes), fsynced, and returns their SHA-256.
func writeEntry(r io.Reader, dst string, size int64, mode os.FileMode) (string, error) {
	h := sha256.New()
	var w io.Writer = h
	var f *os.File
	if dst != "" {
		var err error
		if f, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return "", err
		}
		defer f.Close()
		w = io.MultiWriter(f, h)
	}
	n, err := io.Copy(w, io.LimitReader(r, size))
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("the archive ended early: the connection may have dropped")
	}
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("the archive ended early: the connection may have dropped")
	}
	if f != nil {
		if err := f.Chmod(mode | 0o600); err != nil {
			return "", err
		}
		if err := f.Sync(); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func typeName(t byte) string {
	switch t {
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "fifo"
	}
	return fmt.Sprintf("tar entry of type %q", t)
}

// root returns the item root rootKey, creating its temporary sibling (or
// the memory staging dir) the first time.
func (rv *receiver) root(rootKey, dst, rel string, memory bool, typ string) (*receivedItem, error) {
	if it, ok := rv.roots[rootKey]; ok {
		return it, nil
	}
	var it receivedItem
	switch {
	case memory:
		parent := filepath.Join(rv.o.Dir, "projects")
		made, err := mkdirAllTracked(parent, 0o700)
		rv.created = append(rv.created, made...)
		if err != nil {
			return nil, err
		}
		tmp, err := tempName(filepath.Join(parent, ".memory-"+rv.h.ID))
		if err != nil {
			return nil, err
		}
		if err := os.Mkdir(tmp, 0o700); err != nil {
			return nil, err
		}
		it = receivedItem{tmp: tmp, memory: true}
	default:
		final := dst
		if rootKey != rel { // an entry below the root: the root is its dir
			final = rootPath(rv, rootKey)
		}
		if ok, err := exists(final); err != nil || ok {
			if err == nil {
				err = &refusal{ErrCollision, fmt.Sprintf("%s already exists here", final)}
			}
			return nil, err
		}
		made, err := mkdirAllTracked(filepath.Dir(final), 0o700)
		rv.created = append(rv.created, made...)
		if err != nil {
			return nil, err
		}
		tmp, err := tempName(final)
		if err != nil {
			return nil, err
		}
		it = receivedItem{dst: final, tmp: tmp}
		if rootKey != rel || typ == "dir" {
			if err := os.Mkdir(tmp, 0o700); err != nil {
				return nil, err
			}
		}
	}
	rv.roots[rootKey] = &it
	rv.order = append(rv.order, rootKey)
	return &it, nil
}

// rootPath is the final path of an item root that holds entries.
func rootPath(rv *receiver, rootKey string) string {
	segs := strings.Split(rootKey, "/")
	if segs[0] == "projects" {
		return filepath.Join(rv.o.Dir, "projects", rv.dstFolder, segs[2])
	}
	return filepath.Join(rv.o.Dir, filepath.FromSlash(rootKey))
}

// inTemp is where an entry is written: inside its root's temporary.
func (rv *receiver) inTemp(it *receivedItem, rootKey, rel, dst string, memory bool) string {
	if memory {
		if dst == "" {
			return it.tmp
		}
		return filepath.Join(it.tmp, dst)
	}
	if rootKey == rel {
		return it.tmp
	}
	return filepath.Join(it.tmp, filepath.FromSlash(strings.TrimPrefix(rel, rootKey+"/")))
}

// check compares what arrived with the manifest: the same entries, types,
// sizes, links and checksums. Modes and mtimes come from the manifest.
func (rv *receiver) check(m Manifest) error {
	if m.ID != rv.h.ID {
		return invalid("the manifest is for session %s, not %s", m.ID, rv.h.ID)
	}
	if len(m.Entries) != len(rv.got) {
		return invalid("the manifest lists %d entries, %d arrived", len(m.Entries), len(rv.got))
	}
	for _, want := range m.Entries {
		got, ok := rv.got[want.Path]
		if !ok || got.Type != want.Type || got.Size != want.Size || got.SHA256 != want.SHA256 || got.Link != want.Link {
			return invalid("%s does not match the manifest (checksum or size); nothing was received", want.Path)
		}
	}
	if rv.dryRun {
		return nil
	}
	// Modes and mtimes, deepest first so a directory's mtime survives its
	// children being written.
	entries := append([]ManifestEntry(nil), m.Entries...)
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].Path) > len(entries[j].Path) })
	for _, e := range entries {
		rootKey, dst, memory, err := rv.destination(e.Path)
		if err != nil {
			return err
		}
		it := rv.roots[rootKey]
		if it == nil {
			continue
		}
		p := rv.inTemp(it, rootKey, e.Path, dst, memory)
		if e.Type == "symlink" {
			continue
		}
		mode := os.FileMode(e.Mode) | 0o600
		if e.Type == "dir" {
			mode |= 0o700
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
		if err := os.Chtimes(p, e.MTime, e.MTime); err != nil {
			return err
		}
	}
	return nil
}

// pairs are the rewrite pairs, longest source first (NewRewriter sorts).
func (rv *receiver) pairs() []Pair {
	s, o := rv.h.Source, rv.o
	checkout := o.Checkout
	if checkout == "" {
		checkout = o.Cwd
	}
	srcCheckout, srcReal := s.Checkout, s.CheckoutReal
	if srcCheckout == "" {
		srcCheckout, srcReal = s.Cwd, s.Cwd
	}
	pairs := []Pair{
		{From: srcCheckout, To: checkout},
		{From: srcReal, To: checkout},
	}
	if s.GitRoot != "" && o.GitRoot != "" {
		pairs = append(pairs, Pair{From: s.GitRoot, To: o.GitRoot})
	}
	pairs = append(pairs, Pair{
		From: filepath.Join(s.ConfigDir, "projects", s.ProjectFolder),
		To:   filepath.Join(o.Dir, "projects", rv.dstFolder),
	})
	if s.MemoryFolder != "" {
		pairs = append(pairs, Pair{
			From: filepath.Join(s.ConfigDir, "projects", s.MemoryFolder),
			To:   filepath.Join(o.Dir, "projects", rv.memFolder),
		})
	}
	pairs = append(pairs, Pair{From: s.ConfigDir, To: o.Dir}, Pair{From: s.Home, To: o.Home})
	return pairs
}

// rewrite applies the path rewrite to every .json and .jsonl file but
// file-history's (copies of the user's files), and to the memory files.
func (rv *receiver) rewrite(rw *Rewriter) (int, error) {
	total := 0
	for _, rel := range rv.files {
		rootKey, dst, memory, _ := rv.destination(rel)
		if strings.HasPrefix(rootKey, "file-history/") {
			continue
		}
		isJSON := strings.HasSuffix(rel, ".json") || strings.HasSuffix(rel, ".jsonl")
		if !isJSON && !memory {
			continue
		}
		p := rv.inTemp(rv.roots[rootKey], rootKey, rel, dst, memory)
		data, err := os.ReadFile(p)
		if err != nil {
			return total, err
		}
		var out []byte
		var n int
		if isJSON {
			out, n = rw.JSON(data)
		} else {
			out, n = rw.Text(data)
		}
		if n == 0 {
			continue
		}
		total += n
		if err := rewriteInPlace(p, out); err != nil {
			return total, err
		}
	}
	return total, nil
}

func rewriteInPlace(p string, data []byte) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(p, fi.ModTime(), fi.ModTime())
}

// commitRenames renames every item into place and verifies it against
// what was written, like Move.
func (rv *receiver) commitRenames() error {
	stats := map[string]treeStats{}
	for _, key := range rv.order {
		it := rv.roots[key]
		if it.memory {
			continue
		}
		st, err := statTree(it.tmp)
		if err != nil {
			return err
		}
		stats[key] = st
	}
	parents := map[string]bool{}
	for _, key := range rv.order {
		it := rv.roots[key]
		if it.memory {
			continue
		}
		if err := rv.trap.check(); err != nil {
			return err
		}
		if ok, err := exists(it.dst); err != nil || ok {
			if err == nil {
				err = &refusal{ErrCollision, fmt.Sprintf("%s appeared here while receiving", it.dst)}
			}
			return err
		}
		if err := os.Rename(it.tmp, it.dst); err != nil {
			return err
		}
		rv.renamed = append(rv.renamed, it.dst)
		it.tmp = ""
		parents[filepath.Dir(it.dst)] = true
	}
	for p := range parents {
		if err := syncDir(p); err != nil {
			return err
		}
	}
	for _, key := range rv.order {
		it := rv.roots[key]
		if it.memory {
			continue
		}
		got, err := statTree(it.dst)
		if err != nil {
			return err
		}
		if got != stats[key] {
			return fmt.Errorf("verification failed for %s (%s written, %s in place)", key, describe(stats[key]), describe(got))
		}
	}
	return nil
}

// undo removes what the receiver created; never anything that was there.
func (rv *receiver) undo() error {
	var problems []error
	for _, p := range rv.renamed {
		if err := removeOwned(p); err != nil {
			problems = append(problems, err)
		}
	}
	rv.renamed = nil
	for _, key := range rv.order {
		if it := rv.roots[key]; it.tmp != "" {
			if err := removeOwned(it.tmp); err != nil {
				problems = append(problems, err)
			}
			it.tmp = ""
		}
	}
	for i := len(rv.created) - 1; i >= 0; i-- {
		err := os.Remove(rv.created[i])
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			problems = append(problems, err)
		}
	}
	rv.created = nil
	return errors.Join(problems...)
}

// projectFolderFor is the projects/ folder of cwd in dir: its encoded name,
// or, when that exceeds Claude's 200 characters (it then hashes the
// rest), an existing folder with that prefix whose sessions confirm cwd.
func projectFolderFor(dir, cwd string) (string, error) {
	enc := EncodeCwd(cwd)
	if len(enc) <= maxEncodedLen {
		return enc, nil
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "projects"))
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || len(name) <= maxEncodedLen || !strings.HasPrefix(name, enc[:maxEncodedLen]) {
			continue
		}
		if newest := newestSession(filepath.Join(dir, "projects", name)); newest != "" && filepath.Clean(headCwd(newest)) == filepath.Clean(cwd) {
			return name, nil
		}
	}
	return "", fmt.Errorf("the path %s is too long for a new project folder here; open claude there once, then transfer again", cwd)
}

// memoryFolderFor is the projects/ folder whose memory/ belongs to gitRoot.
func memoryFolderFor(dir, gitRoot string) string {
	enc := EncodeCwd(gitRoot)
	if len(enc) <= maxEncodedLen {
		return enc
	}
	if name, err := projectFolderFor(dir, gitRoot); err == nil {
		return name
	}
	return enc[:maxEncodedLen]
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadReceipt(dir, id string) (Receipt, bool) {
	var r Receipt
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil || json.Unmarshal(raw, &r) != nil || r.ManifestSHA256 == "" {
		return r, false
	}
	return r, true
}

func saveReceipt(dir, id string, r Receipt) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+id+".json.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, id+".json"))
}

// pruneReceipts drops receipts older than ReceiptTTL (by their own "at").
func pruneReceipts(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidID(id) {
			continue
		}
		if r, ok := loadReceipt(dir, id); ok && now.Sub(r.At) > ReceiptTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// --- release ---

// Release deletes the source items of s after the destination committed
// (SPEC step 3): only when a fresh pack of it has the manifest the
// destination received (so the transcript and its side files are unchanged
// since they were sent) and it is not open; otherwise both copies stay
// (ErrChanged). Project memory stays: it was merged, never moved. The
// returned warnings are items that could not be deleted.
func Release(s Session, src PackSource, manifestSHA string) ([]string, error) {
	_, sum, err := Pack(s, src, io.Discard)
	if err != nil {
		if errors.Is(err, ErrLive) || errors.Is(err, ErrMaybeOpen) || errors.Is(err, ErrChanged) {
			return nil, &refusal{ErrChanged, fmt.Sprintf("%q is open or changed on this machine while it was being sent; both copies are kept (%v)", s.Label(), err)}
		}
		return nil, err
	}
	if sum != manifestSHA {
		return nil, &refusal{ErrChanged, fmt.Sprintf("%q changed on this machine while it was being sent; both copies are kept", s.Label())}
	}
	srcRoot := filepath.Clean(s.ConfigDir)
	items, err := collectItems(s, srcRoot, srcRoot, filepath.Join("projects", s.ProjectDir, s.ID+".jsonl"))
	if err != nil {
		return nil, err
	}
	var warnings []string
	for _, it := range items {
		if err := removeSource(it.src); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not remove %s (%v); delete it by hand", it.rel, err))
		}
	}
	return warnings, nil
}

// FindSession returns the session with id in cd (any project folder).
func FindSession(cd config.ConfigDir, id string) (Session, error) {
	if !ValidID(id) {
		return Session{}, fmt.Errorf("invalid session id %q", id)
	}
	matches, err := filepath.Glob(filepath.Join(cd.Dir, "projects", "*", id+".jsonl"))
	if err != nil {
		return Session{}, err
	}
	if len(matches) == 0 {
		return Session{}, &refusal{ErrGone, fmt.Sprintf("session %s is no longer in %s", shortID(id), cd.Name)}
	}
	p := matches[0]
	fi, err := os.Stat(p)
	if err != nil {
		return Session{}, err
	}
	m, err := readMeta(p)
	if err != nil {
		return Session{}, err
	}
	return Session{
		ID: id, ConfigName: cd.Name, ConfigDir: cd.Dir, ProjectDir: filepath.Base(filepath.Dir(p)),
		Cwd: m.cwd, Title: m.title, FirstPrompt: m.firstPrompt, LastActive: m.lastActive, ModTime: fi.ModTime(), Size: fi.Size(),
		ClaudeVersion: m.version, GitBranch: m.gitBranch,
	}, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ErrorKind maps a move error to the agent's error code name, "" for none.
func ErrorKind(err error) string {
	switch {
	case errors.Is(err, ErrLive):
		return "LIVE"
	case errors.Is(err, ErrMaybeOpen):
		return "MAYBE_OPEN"
	case errors.Is(err, ErrCollision):
		return "COLLISION"
	case errors.Is(err, ErrGone):
		return "GONE"
	case errors.Is(err, ErrChanged):
		return "CHANGED"
	case errors.Is(err, ErrNoSpace), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "NO_SPACE"
	case errors.Is(err, ErrInvalid):
		return "INVALID"
	}
	return ""
}
