package claudecfg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/muratgozel/julienning/internal/jsonedit"
)

// SettingsFile is Claude Code's per-config-dir settings file. The default dir
// uses ~/.claude/settings.json like any other dir (only the account file is
// special, see AccountFilePath).
const SettingsFile = "settings.json"

// utf8BOM is the byte-order mark some editors prepend. encoding/json rejects
// it ("invalid character 'ï'"), so it is stripped on read and never written.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// SettingsPath returns <dir>/settings.json.
func SettingsPath(dir string) string { return filepath.Join(dir, SettingsFile) }

// ReadSettings returns the raw bytes of <dir>/settings.json, or "{}" when the
// file is missing.
func ReadSettings(dir string) ([]byte, error) {
	p := SettingsPath(dir)
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return []byte("{}\n"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	raw = bytes.TrimPrefix(raw, utf8BOM)
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}\n"), nil
	}
	return raw, nil
}

// WriteSettings writes <dir>/settings.json (0644), creating nothing else.
func WriteSettings(dir string, data []byte) error {
	if !strings.HasSuffix(string(data), "\n") {
		data = append(data, '\n')
	}
	return writeAtomic(SettingsPath(dir), data, 0o644)
}

// ResolvedSettingsPath returns the real file <dir>/settings.json stands for:
// symlinks are followed (also when the file does not exist yet), so config
// dirs that share one settings.json through symlinks return the same path.
// It is the key patches.json records the file under.
func ResolvedSettingsPath(dir string) (string, error) {
	target, err := resolveTarget(SettingsPath(dir))
	if err != nil {
		return "", err
	}
	return recordKey(target), nil
}

// settingsDoc is a settings.json opened for editing.
type settingsDoc struct {
	path   string // <dir>/settings.json as given, for messages
	key    string // resolved path; the patches.json key
	exists bool
	mode   os.FileMode
	before []byte // content as parsed (BOM stripped, "{}\n" when missing/blank)
	obj    *jsonedit.Object
}

// openSettings reads and parses a settings file. Non-object or invalid files
// are errors naming the path, and are never modified.
func openSettings(path string) (*settingsDoc, error) {
	target, err := resolveTarget(path)
	if err != nil {
		return nil, err
	}
	doc := &settingsDoc{path: path, key: recordKey(target), mode: 0o644}
	raw, err := os.ReadFile(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	default:
		doc.exists = true
		if fi, err := os.Stat(target); err == nil {
			doc.mode = fi.Mode().Perm()
		}
	}
	raw = bytes.TrimPrefix(raw, utf8BOM)
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}\n")
	}
	obj, err := jsonedit.Parse(raw)
	if errors.Is(err, jsonedit.ErrNotObject) {
		return nil, fmt.Errorf("%s is not a JSON object; leaving it alone", path)
	}
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); leaving it alone", path, err)
	}
	doc.before = raw
	doc.obj = obj
	return doc, nil
}

// changed reports whether edits altered the document.
func (d *settingsDoc) changed() bool { return !bytes.Equal(d.before, d.obj.Bytes()) }

// save writes the edited document when it changed.
func (d *settingsDoc) save() error {
	if !d.changed() {
		return nil
	}
	return writeAtomic(d.path, d.obj.Bytes(), d.mode)
}

// recordKey makes a stable absolute key for a (possibly not yet existing)
// file: symlinks in the file or its directory are resolved, so the same file
// reached through a symlinked config dir maps to one patches.json record.
func recordKey(target string) string {
	if abs, err := filepath.Abs(target); err == nil {
		target = abs
	}
	if r, err := filepath.EvalSymlinks(target); err == nil {
		return r
	}
	if d, err := filepath.EvalSymlinks(filepath.Dir(target)); err == nil {
		return filepath.Join(d, filepath.Base(target))
	}
	return filepath.Clean(target)
}

// resolveTarget follows symlinks to the real file writeAtomic must replace.
// settings.json is often a symlink into a dotfiles repo or shared between
// config dirs; renaming onto the link path would silently break that sharing.
func resolveTarget(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	switch {
	case err == nil:
		return resolved, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	// Either the path does not exist (write it directly) or it is a dangling
	// symlink (write where it points, so the link becomes valid).
	fi, lerr := os.Lstat(path)
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	dest, lerr := os.Readlink(path)
	if lerr != nil {
		return "", fmt.Errorf("resolve %s: %w", path, lerr)
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(path), dest)
	}
	return dest, nil
}

// writeAtomic writes via a temp file in the target's directory, fsyncs it,
// then renames it over the target.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	path, err := resolveTarget(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		return fail(fmt.Errorf("write %s: %w", path, err))
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(fmt.Errorf("chmod %s: %w", path, err))
	}
	// Flush before the rename: otherwise a crash can leave a renamed but empty
	// settings.json, losing every user setting in it.
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("sync %s: %w", path, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}
