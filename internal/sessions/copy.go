package sessions

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// failpoint lets tests inject a failure at a named step ("copy", "rename",
// "verify", "delete", "memory") for a given path. Always nil in production.
var failpoint func(step, path string) error

func fail(step, path string) error {
	if failpoint == nil {
		return nil
	}
	return failpoint(step, path)
}

// tempName returns a sibling name for path that Claude Code will not mistake
// for a session: hidden, and never ending in ".jsonl".
func tempName(path string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate temp name: %w", err)
	}
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".julienning-"+hex.EncodeToString(b)+".tmp"), nil
}

// copyTree copies src (file, symlink or directory) to dst, which must not
// exist. Modes and mtimes are preserved (directories get theirs after their
// children are written) and every regular file is fsynced before returning.
// It stops with an ErrInterrupted error as soon as trap (may be nil) has
// caught a signal.
func copyTree(src, dst string, trap *sigTrap) error {
	if err := trap.check(); err != nil {
		return err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode().IsRegular():
		return copyFile(src, dst, fi, trap)
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case fi.IsDir():
		// 0700 while filling it, so a read-only source dir can still be
		// populated; the real mode is applied last.
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), trap); err != nil {
				return err
			}
		}
		if err := syncDir(dst); err != nil {
			return err
		}
		// May make the copy read-only (e.g. 0555); cleanup of a failed move
		// goes through removeOwned, which makes it writable again.
		if err := os.Chmod(dst, fi.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	default:
		return fmt.Errorf("%s: unsupported file type %s", src, fi.Mode().Type())
	}
}

func copyFile(src, dst string, fi os.FileInfo, trap *sigTrap) error {
	if err := fail("copy", src); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	var r io.Reader = in
	if trap != nil {
		// Transcripts reach 60+ MB: notice Ctrl-C between chunks, not only
		// between files.
		r = trapReader{r: in, trap: trap}
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(fi.Mode().Perm()); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// trapReader fails reads once trap has caught a signal.
type trapReader struct {
	r    io.Reader
	trap *sigTrap
}

func (t trapReader) Read(p []byte) (int, error) {
	if err := t.trap.check(); err != nil {
		return 0, err
	}
	return t.r.Read(p)
}

// syncDir fsyncs a directory so renames and new entries in it are durable.
// Some filesystems cannot fsync directories; that is not a copy failure.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EBADF) {
		return err
	}
	return nil
}

// treeStats summarises a tree for the post-rename verification.
type treeStats struct {
	files, dirs, links int
	bytes              int64
}

func statTree(root string) (treeStats, error) {
	var st treeStats
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			st.links++
		case d.IsDir():
			st.dirs++
		default:
			fi, err := d.Info()
			if err != nil {
				return err
			}
			st.files++
			st.bytes += fi.Size()
		}
		return nil
	})
	return st, err
}

// exists reports whether path exists (without following a final symlink).
func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// removeOwned removes a tree this package created in the target (a
// temporary or a renamed copy). Copies keep the source's modes, so a
// read-only directory (0555, or 0500 like some file-history dirs) would make
// os.RemoveAll fail halfway and leave a partial copy that a retry then takes
// for an existing session; every directory in the tree is made owner-
// accessible first. Symlinks are never followed.
//
// CRITICAL: only ever call this on paths the move itself created — never on
// source paths or anything that existed in the target before.
func removeOwned(path string) error {
	// Chmod errors are not returned: if one matters, RemoveAll fails on that
	// directory and its error names it.
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// WalkDir calls this before reading the directory, so a 0000 copy
		// is readable by the time its entries are listed.
		if d.IsDir() {
			if fi, err := d.Info(); err == nil && fi.Mode().Perm()&0o700 != 0o700 {
				_ = os.Chmod(p, fi.Mode().Perm()|0o700)
			}
		}
		return nil
	})
	return os.RemoveAll(path)
}

// mkdirAllTracked creates dir and missing parents, returning the directories
// it created (outermost first) so a failed move can remove them. perm comes
// from the source; the owner always keeps rwx so the move can fill the
// directory and a failed one can empty it again.
func mkdirAllTracked(dir string, perm os.FileMode) ([]string, error) {
	perm |= 0o700
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		ok, err := exists(d)
		if err != nil {
			return nil, err
		}
		if ok {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		err := os.Mkdir(missing[i], perm)
		if errors.Is(err, os.ErrExist) {
			continue // made by someone else meanwhile: not ours to remove
		}
		if err != nil {
			return created, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}
