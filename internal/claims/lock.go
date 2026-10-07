package claims

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muratgozel/julienning/internal/config"
)

const (
	// LockFile serializes claim reconciliation (and the background maintenance
	// that rides along with it) to one process per machine.
	LockFile = "claim-sync.lock"

	// LockStale is how long a lock is trusted. It must exceed the longest
	// holder (claim-sync bounds its work well below this) so only a crashed
	// process's lock is ever broken.
	LockStale = 60 * time.Second

	// ReconcileEvery is how often the send-usage child reconciles claims, to
	// clean up after sessions whose SessionEnd hook never ran.
	ReconcileEvery = 10 * time.Minute

	// reconciledFile's mtime is when claims were last reconciled.
	reconciledFile = ".claims-reconciled"
)

// AcquireLock takes the per-machine claim-sync lock with O_EXCL. ok=false
// means another process holds it (younger than LockStale). The returned
// release is always safe to defer.
func AcquireLock(now time.Time) (release func(), ok bool, err error) {
	p, err := config.Path(LockFile)
	if err != nil {
		return func() {}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return func() {}, false, fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		switch {
		case err == nil:
			fmt.Fprintf(f, "%d\n", os.Getpid()) // for humans debugging a stuck lock
			f.Close()
			// The lock's mtime is its clock, so a frozen JULIENNING_NOW_EPOCH
			// must move it too or tests could never observe staleness.
			_ = os.Chtimes(p, now, now)
			return func() { os.Remove(p) }, true, nil
		case !errors.Is(err, os.ErrExist):
			return func() {}, false, fmt.Errorf("create %s: %w", p, err)
		}
		fi, statErr := os.Stat(p)
		if statErr != nil {
			continue // released between the two calls: retry once
		}
		if age := now.Sub(fi.ModTime()); age >= 0 && age < LockStale {
			return func() {}, false, nil
		}
		// Stale (or from the future after a clock change): the holder died.
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return func() {}, false, fmt.Errorf("remove stale %s: %w", p, err)
		}
	}
	// Lost both races: someone else is reconciling, which is the point.
	return func() {}, false, nil
}

// ReconcileDue reports whether ReconcileEvery has passed since
// MarkReconciled. A missing marker, or one from the future, means due.
func ReconcileDue(now time.Time) bool {
	p, err := config.Path(reconciledFile)
	if err != nil {
		return true
	}
	fi, err := os.Stat(p)
	if err != nil {
		return true
	}
	age := now.Sub(fi.ModTime())
	return age < 0 || age >= ReconcileEvery
}

// MarkReconciled records a reconciliation attempt at now. It is stamped
// whatever the outcome, so an unreachable Worker is retried every
// ReconcileEvery rather than on every status line render.
func MarkReconciled(now time.Time) error {
	p, err := config.Path(reconciledFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Chtimes(p, now, now); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}
