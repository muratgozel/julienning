package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SettingsValues returns the raw JSON of each of keys present at the top
// level of <dir>/settings.json (absent keys are left out). A missing file
// has none; a file that is not a JSON object is an error naming it.
func SettingsValues(dir string, keys []string) (map[string]json.RawMessage, error) {
	doc, err := openSettings(SettingsPath(dir))
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, k := range keys {
		if raw, ok := doc.obj.Get(k); ok {
			out[k] = raw
		}
	}
	return out, nil
}

// SettingEdit is one top-level settings.json change: Value set (valid JSON),
// or the key removed when Value is nil.
type SettingEdit struct {
	Key   string
	Value json.RawMessage
}

// EditSettings applies edits to <dir>/settings.json with the same guarantees
// as Patch: every other byte is preserved, new members follow the file's
// indentation, the file is written atomically through symlinks and only when
// it changes. It re-reads the file, so edits land on what is there now.
func EditSettings(dir string, edits []SettingEdit) (changed bool, err error) {
	doc, err := openSettings(SettingsPath(dir))
	if err != nil {
		return false, err
	}
	for _, e := range edits {
		if e.Value == nil {
			if _, err := doc.obj.Delete(e.Key); err != nil {
				return false, fmt.Errorf("%s: remove %q: %w", doc.path, e.Key, err)
			}
			continue
		}
		if !json.Valid(e.Value) {
			return false, fmt.Errorf("%s: value for %q is not valid JSON", doc.path, e.Key)
		}
		if cur, ok := doc.obj.Get(e.Key); ok && bytes.Equal(cur, e.Value) {
			continue
		}
		if err := doc.obj.SetValue(e.Key, e.Value); err != nil {
			return false, fmt.Errorf("%s: %w", doc.path, err)
		}
	}
	if !doc.changed() {
		return false, nil
	}
	return true, doc.save()
}

// ClaudeMDFile is the user-level memory file Claude Code loads from every
// config dir (<dir>/CLAUDE.md, the default dir's too).
const ClaudeMDFile = "CLAUDE.md"

// maxClaudeMD bounds how much of a CLAUDE.md is read; Claude Code itself
// warns long before this.
const maxClaudeMD = 4 << 20

// ClaudeMD is a config dir's CLAUDE.md as read by ReadClaudeMD.
type ClaudeMD struct {
	Path    string // <dir>/CLAUDE.md
	Exists  bool
	Content []byte
	ModTime time.Time
	Mode    os.FileMode
}

// ErrClaudeMDChanged means the file is no longer what ReadClaudeMD returned.
var ErrClaudeMDChanged = errors.New("it changed since it was read")

// ReadClaudeMD reads <dir>/CLAUDE.md (following symlinks). A missing file
// is not an error: Exists is false.
func ReadClaudeMD(dir string) (ClaudeMD, error) {
	md := ClaudeMD{Path: filepath.Join(dir, ClaudeMDFile), Mode: 0o644}
	f, err := os.Open(md.Path)
	if errors.Is(err, os.ErrNotExist) {
		return md, nil
	}
	if err != nil {
		return md, fmt.Errorf("read %s: %w", md.Path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return md, fmt.Errorf("read %s: %w", md.Path, err)
	}
	if !fi.Mode().IsRegular() {
		return md, fmt.Errorf("%s is not a regular file", md.Path)
	}
	content, err := io.ReadAll(io.LimitReader(f, maxClaudeMD+1))
	if err != nil {
		return md, fmt.Errorf("read %s: %w", md.Path, err)
	}
	if len(content) > maxClaudeMD {
		return md, fmt.Errorf("%s is larger than %d MB", md.Path, maxClaudeMD>>20)
	}
	md.Exists, md.Content, md.ModTime, md.Mode = true, content, fi.ModTime(), fi.Mode().Perm()
	return md, nil
}

// WriteClaudeMD replaces <dir>/CLAUDE.md with content, atomically and
// through symlinks, keeping the file's mode. prev is what the caller read
// earlier; when the file is no longer that (created, deleted or edited
// since), nothing is written and the error wraps ErrClaudeMDChanged, so an
// edit made while the user was choosing is never lost.
func WriteClaudeMD(dir string, content []byte, prev ClaudeMD) error {
	now, err := ReadClaudeMD(dir)
	if err != nil {
		return err
	}
	if now.Exists != prev.Exists || !bytes.Equal(now.Content, prev.Content) {
		return fmt.Errorf("%s: %w", now.Path, ErrClaudeMDChanged)
	}
	return writeAtomic(now.Path, content, now.Mode)
}
