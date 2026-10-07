package usage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muratgozel/julienning/internal/config"
)

const (
	// logMaxBytes / logKeepLines mirror the bash status line: trim only once the
	// file is big, and keep the newest lines.
	logMaxBytes  = 1 << 20
	logKeepLines = 500

	// rateLimitEvery throttles per-code logging. The status line runs on every
	// Claude Code render, so an unregistered config dir would otherwise append a
	// NOT_SHARED line several times a second.
	rateLimitEvery = time.Hour

	// SendFailedEvery throttles SEND_FAILED per account, so a Worker outage
	// leaves a breadcrumb every 10 minutes instead of flooding errors.log.
	SendFailedEvery = 10 * time.Minute
)

// logTimeLayout is RFC 3339 with a numeric offset. It deliberately avoids
// Go's "Z07:00" form so UTC prints as +00:00, exactly like the bash predecessor.
const logTimeLayout = "2006-01-02T15:04:05-07:00"

// Logger appends diagnostics to $JULIENNING_HOME/errors.log. Messages must
// never contain an email address: this file is shared in bug reports.
type Logger struct {
	Dir       string // julienning home
	ConfigDir string // CLAUDE_CONFIG_DIR, "" renders as <unset>
	Now       time.Time
	Loc       *time.Location
}

// NewLogger resolves the julienning home for a Logger.
func NewLogger(now time.Time, loc *time.Location, configDir string) (Logger, error) {
	dir, err := config.Dir()
	if err != nil {
		return Logger{}, err
	}
	if loc == nil {
		loc = time.Local
	}
	return Logger{Dir: dir, ConfigDir: configDir, Now: now, Loc: loc}, nil
}

// Path is the errors.log path.
func (l Logger) Path() string { return filepath.Join(l.Dir, config.ErrorLog) }

// Line renders one log line (without the trailing newline).
func (l Logger) Line(code, message string) string {
	dir := l.ConfigDir
	if dir == "" {
		dir = "<unset>"
	}
	return fmt.Sprintf("%s %s config_dir=%s %s", l.Now.In(l.Loc).Format(logTimeLayout), code, dir, message)
}

// Log appends one line, trimming the file when it has grown past 1 MB.
func (l Logger) Log(code, message string) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", l.Dir, err)
	}
	p := l.Path()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write %s", p)
	}
	if _, err := f.WriteString(l.Line(code, message) + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("cannot write %s", p)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write %s", p)
	}
	return l.trim()
}

// LogThrottled logs at most once per hour per code, keeping the status line
// from flooding errors.log on every render.
func (l Logger) LogThrottled(code, message string) error {
	return l.LogThrottledEvery(code, "", message, rateLimitEvery)
}

// LogThrottledEvery logs at most once per `every` for one (code, key) pair.
// key is hashed, never stored verbatim: it is usually an email, and neither
// errors.log nor the marker file names may carry an address.
func (l Logger) LogThrottledEvery(code, key, message string, every time.Duration) error {
	name := ".last-" + code
	if key != "" {
		name += "-" + KeyHash(key)
	}
	marker := filepath.Join(l.Dir, name)
	if fi, err := os.Stat(marker); err == nil && l.Now.Sub(fi.ModTime()) < every {
		return nil
	}
	if err := l.Log(code, message); err != nil {
		return err
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return nil // the log line is written; a missing marker only costs repetition
	}
	// The marker's mtime is the throttle clock, so a frozen JULIENNING_NOW_EPOCH
	// must move it too or tests could never observe the throttle.
	_ = os.Chtimes(marker, l.Now, l.Now)
	return nil
}

// KeyHash is the short, stable, non-reversible-in-practice name for a
// throttling key (an email) used in marker file names.
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

func (l Logger) trim() error {
	p := l.Path()
	fi, err := os.Stat(p)
	if err != nil || fi.Size() <= logMaxBytes {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("cannot trim %s", p)
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) > logKeepLines {
		lines = lines[len(lines)-logKeepLines:]
	}
	out := append(bytes.Join(lines, []byte("\n")), '\n')
	tmp := p + ".trim"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("cannot trim %s", p)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot trim %s", p)
	}
	return nil
}
