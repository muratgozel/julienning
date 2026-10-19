// Package usageprobe reads an account's plan usage through Claude Code
// itself: `claude -p "/usage"` run against the account's config dir. It is
// the second source of usage next to the status line, for accounts nobody
// uses through julienning right now (a teammate on plain Claude Code, or an
// account idle past its reset).
//
// CRITICAL for future agents: julienning never reads Claude credentials. The
// probe is Claude Code fetching its own usage endpoint with its own login;
// julienning only reads the structured `usage_report` Claude Code prints
// next to the /usage text (and throws the text away: it describes local
// sessions). /usage is a local command, so no model request is sent and no
// quota is used. All of this is undocumented Claude Code behaviour, observed
// in 2.1.301-2.1.303; see CLAUDE.md "Claude Code internals we depend on".
package usageprobe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/livesess"
)

// MinVersion is the oldest Claude Code known to attach usage_report to the
// /usage result (checked in the 2.1.301 binary). Older versions are not
// probed into: their missing report would read as "signed out".
var MinVersion = [3]int{2, 1, 301}

const (
	// Timeout bounds one probe (claude starts and answers in about 1.5 s).
	Timeout = 10 * time.Second
	// Parallel is how many probes run at once; each is a full Claude Code
	// process. Claude Code serializes token refreshes across processes, so
	// probes of the same dir (or of a dir with a live session) are safe.
	Parallel = 4
	// maxOutput bounds what is read from claude; the real answer is ~10 KB.
	maxOutput = 1 << 20
)

// Outcome is what a probe found.
type Outcome string

const (
	// OK: Claude Code reported the session and weekly windows.
	OK Outcome = "ok"
	// SignedOut: Claude Code has no usable login in the dir (no usage
	// report, and `claude auth status` says not logged in), whatever the
	// dir's .claude.json still names.
	SignedOut Outcome = "signed_out"
	// Failed: anything else (claude missing or too old, a timeout, the
	// usage endpoint unreachable, an answer of an unexpected shape).
	Failed Outcome = "failed"
)

// Window is one rate-limit window. ResetsAt nil means the window is not
// running (nothing used since it last reset); Used is then 0.
type Window struct {
	Used     float64    `json:"used"`
	ResetsAt *time.Time `json:"resets_at"`
}

// Result is one probe of one config dir.
type Result struct {
	Dir     string
	Outcome Outcome
	Session Window // OK only
	Week    Window // OK only
	At      time.Time
	// Err says what went wrong (Failed only). It never quotes claude's
	// output, which may describe the user's sessions.
	Err error
	// Shape is a type signature of an unexpected usage report (field names
	// and JSON types, never values) for errors.log; "" otherwise.
	Shape string
}

// Seams for tests: they point findClaude at a fake claude and shorten the
// timeout. Tests never run the real claude.
var (
	findClaude = launch.ClaudePath
	timeout    = Timeout
)

// Args is the probe's command line after the binary. --safe-mode keeps the
// dir's hooks (julienning's own claim hooks included), plugins, MCP servers
// and CLAUDE.md out while auth works normally; --no-session-persistence
// keeps the run out of projects/ and the session picker; --name marks its
// live-session registry entry so claims ignore it (livesess.CountsAsSession).
var Args = []string{
	"-p", "/usage",
	"--safe-mode",
	"--no-session-persistence",
	"--name", livesess.ProbeSessionName,
	"--output-format", "stream-json",
	"--verbose",
}

// Env is environ for a probe of dir. Every CLAUDE* and ANTHROPIC_* variable
// is dropped first: a probe started from a hook or the status line inherits
// Claude Code's session variables (CLAUDECODE, CLAUDE_CODE_SESSION_ID, its
// messaging socket), and CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY or a
// gateway's ANTHROPIC_BASE_URL would make Claude Code answer for another
// login than the dir's. launch.Env then selects dir (unset for the default
// dir, see claudecfg.DefaultDir).
func Env(environ []string, dir string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "CLAUDE") || strings.HasPrefix(name, "ANTHROPIC_") {
			continue
		}
		out = append(out, kv)
	}
	return launch.Env(out, dir)
}

// RunAll probes dirs, at most Parallel at a time, in the order given.
func RunAll(ctx context.Context, dirs []string, now time.Time) []Result {
	out := make([]Result, len(dirs))
	sem := make(chan struct{}, Parallel)
	var wg sync.WaitGroup
	for i, d := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = Run(ctx, d, now)
		}()
	}
	wg.Wait()
	return out
}

// ErrNoTime marks a probe that did not run, or was cut short, because the
// caller's time budget ran out (not claude's own timeout). It is not cached:
// nothing was learned about the dir.
var ErrNoTime = errors.New("the time for usage probes ran out")

// timeoutError is claude's own Timeout. A dir that hangs is not asked again
// within the same probe (auth status would hang the same way).
type timeoutError struct{ after time.Duration }

func (e timeoutError) Error() string { return fmt.Sprintf("claude did not answer within %s", e.after) }

// Run probes one config dir. now is when the numbers are taken to be read.
func Run(ctx context.Context, dir string, now time.Time) Result {
	r := Result{Dir: dir, At: now.UTC().Truncate(time.Second)}
	fail := func(err error) Result {
		r.Outcome, r.Err = Failed, err
		return r
	}
	if ctx.Err() != nil {
		return fail(ErrNoTime)
	}
	path, err := findClaude()
	if err != nil {
		return fail(err)
	}
	out, err := runClaude(ctx, path, dir, Args)
	ans := parseStream(out)
	var te timeoutError
	if errors.Is(err, ErrNoTime) || errors.As(err, &te) {
		return fail(err)
	}
	version := ans.version
	if version == "" && ans.report == nil {
		// No init line: a claude too old for one of the flags exits before
		// printing anything. Its version says so better than its exit code.
		version = claudeVersion(ctx, path, dir)
	}
	if version != "" && !newEnough(version) {
		return fail(fmt.Errorf("Claude Code %s cannot report usage; julienning needs %d.%d.%d or newer (run: claude update)",
			version, MinVersion[0], MinVersion[1], MinVersion[2]))
	}
	if ans.report == nil {
		// No report at all, whatever the exit status: either nobody is
		// signed in here, or the login is not a Claude subscription. Only
		// Claude Code can tell, and an account wrongly called signed out
		// would vanish from `next`.
		in, aerr := signedIn(ctx, path, dir)
		switch {
		case errors.Is(aerr, ErrNoTime):
			return fail(aerr)
		case aerr != nil && err != nil:
			return fail(err)
		case aerr != nil:
			return fail(fmt.Errorf("Claude Code reported no usage, and its login could not be checked (%v)", aerr))
		case !in:
			r.Outcome = SignedOut
			return r
		case err != nil:
			return fail(err)
		default:
			return fail(errors.New("Claude Code is signed in but reported no plan usage (not a Claude subscription login?)"))
		}
	}
	session, week, shape, err := parseReport(ans.report)
	if err != nil {
		r.Shape = shape
		return fail(err)
	}
	r.Outcome, r.Session, r.Week = OK, session, week
	return r
}

// runClaude runs claude with args against dir and returns its stdout. The
// process gets its own process group, killed whole on timeout, and stdin
// from /dev/null; stderr is discarded (it can quote the user's setup).
func runClaude(parent context.Context, path, dir string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = Env(os.Environ(), dir)
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stdout limitedBuffer
	stdout.max = maxOutput
	cmd.Stdout = &stdout
	err := cmd.Run()
	if parent.Err() != nil {
		return stdout.Bytes(), ErrNoTime
	}
	if ctx.Err() != nil {
		return stdout.Bytes(), timeoutError{timeout}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout.Bytes(), fmt.Errorf("claude exited with status %d", exit.ExitCode())
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("run claude: %w", err)
	}
	return stdout.Bytes(), nil
}

// limitedBuffer keeps the first max bytes and drops the rest, so a runaway
// claude cannot fill memory; the parser then finds no report and says so.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// SignedIn asks Claude Code whether dir has a login (`claude auth status`),
// with the probe's environment and timeout. `julienning login` uses it so it
// never signs a dir in over a login that still works.
func SignedIn(ctx context.Context, dir string) (bool, error) {
	path, err := findClaude()
	if err != nil {
		return false, err
	}
	return signedIn(ctx, path, dir)
}

// signedIn asks `claude auth status --json` whether dir has a login. It
// exits 1 when logged out (2.1.303), so the JSON is read whatever the
// status; only `loggedIn` is looked at.
func signedIn(ctx context.Context, path, dir string) (bool, error) {
	out, err := runClaude(ctx, path, dir, []string{"auth", "status", "--json"})
	var doc struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &doc); jerr != nil || doc.LoggedIn == nil {
		if err != nil {
			return false, err
		}
		return false, errors.New("unexpected answer from claude auth status")
	}
	return *doc.LoggedIn, nil
}

// claudeVersion is `claude --version`'s version ("2.1.303"), or "" when it
// cannot be told.
func claudeVersion(ctx context.Context, path, dir string) string {
	out, err := runClaude(ctx, path, dir, []string{"--version"})
	if err != nil {
		return ""
	}
	if m := versionRe.FindStringSubmatch(strings.TrimSpace(string(out))); m != nil {
		return m[0]
	}
	return ""
}

// answer is what the probe needs from claude's stream-json output.
type answer struct {
	version string                     // system/init claude_code_version
	report  map[string]json.RawMessage // the assistant message's usage_report
}

// parseStream reads one JSON object per line and picks the init message's
// version and the first usage_report. Anything else (the /usage text, the
// result message) is ignored without being kept.
func parseStream(out []byte) answer {
	var a answer
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), maxOutput)
	for sc.Scan() {
		var msg struct {
			Type        string                     `json:"type"`
			Subtype     string                     `json:"subtype"`
			Version     string                     `json:"claude_code_version"`
			UsageReport map[string]json.RawMessage `json:"usage_report"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		switch {
		case msg.Type == "system" && msg.Subtype == "init" && a.version == "":
			a.version = msg.Version
		case msg.Type == "assistant" && msg.UsageReport != nil && a.report == nil:
			a.report = msg.UsageReport
		}
	}
	return a
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// newEnough compares a Claude Code version with MinVersion. An unparseable
// version is let through: the report itself is checked anyway.
func newEnough(v string) bool {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return true
	}
	for i := 0; i < 3; i++ {
		n, _ := strconv.Atoi(m[i+1])
		if n != MinVersion[i] {
			return n > MinVersion[i]
		}
	}
	return true
}

// parseReport reads usage_report.rate_limits.limits: the row of kind
// "session" is the five-hour window, "weekly_all" the weekly one. Rows are
// classified on `kind`, never on their label (the schema says so). Model
// scoped rows (weekly_scoped) are ignored. A row missing, or a shape not
// seen before, is an error with a signature for errors.log, never a guess:
// a wrong 0% would rank the account as the emptiest.
func parseReport(report map[string]json.RawMessage) (session, week Window, shape string, err error) {
	var rl struct {
		Limits json.RawMessage `json:"limits"`
	}
	raw, ok := report["rate_limits"]
	if !ok || string(raw) == "null" || json.Unmarshal(raw, &rl) != nil || len(rl.Limits) == 0 || string(rl.Limits) == "null" {
		// rate_limits is null while Claude Code cannot fetch the usage.
		return session, week, "", errors.New("Claude Code could not fetch the usage (offline, or the usage endpoint refused this login)")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(rl.Limits, &rows); err != nil {
		return session, week, "limits=" + jsonType(rl.Limits), errors.New("unexpected usage report from Claude Code")
	}
	byKind := map[string]map[string]json.RawMessage{}
	var kinds []string
	for _, row := range rows {
		var kind string
		if json.Unmarshal(row["kind"], &kind) != nil {
			continue
		}
		kinds = append(kinds, kind)
		if _, dup := byKind[kind]; !dup {
			byKind[kind] = row
		}
	}
	sort.Strings(kinds)
	sig := func() string {
		parts := []string{"kinds=[" + strings.Join(kinds, " ") + "]"}
		for _, k := range []string{"session", "weekly_all"} {
			if row, ok := byKind[k]; ok {
				parts = append(parts, k+"={percent:"+jsonType(row["percent"])+",resets_at:"+jsonType(row["resets_at"])+"}")
			}
		}
		return strings.Join(parts, " ")
	}
	var ok1, ok2 bool
	session, ok1 = parseRow(byKind["session"])
	week, ok2 = parseRow(byKind["weekly_all"])
	if !ok1 || !ok2 {
		return Window{}, Window{}, sig(), errors.New("unexpected usage report from Claude Code")
	}
	return session, week, "", nil
}

// parseRow reads one limits row. percent is 0-100 (more once a limit is
// exceeded, clamped like the status line's used_percentage); resets_at is
// ISO 8601 with fractional seconds, or null for a window that is not
// running, which only makes sense at 0%. The server's reset jitters around
// the real boundary (13:40:00.345 on one machine, 13:39:59.707 on another,
// for a reset the status line gives as 13:40:00), so it is rounded to the
// nearest second, never floored.
func parseRow(row map[string]json.RawMessage) (Window, bool) {
	if row == nil {
		return Window{}, false
	}
	var pct float64
	if json.Unmarshal(row["percent"], &pct) != nil || math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
		return Window{}, false
	}
	used := math.Round(min(pct, 100)*10) / 10
	raw, present := row["resets_at"]
	if !present {
		return Window{}, false
	}
	if string(raw) == "null" {
		if used != 0 {
			return Window{}, false
		}
		return Window{Used: 0}, true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return Window{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() < 2020 {
		return Window{}, false
	}
	t = t.UTC().Round(time.Second)
	return Window{Used: used, ResetsAt: &t}, true
}

func jsonType(raw json.RawMessage) string {
	if raw == nil {
		return "absent"
	}
	switch t := bytes.TrimSpace(raw); {
	case len(t) == 0:
		return "absent"
	case t[0] == '"':
		return "string"
	case t[0] == '{':
		return "object"
	case t[0] == '[':
		return "array"
	case string(t) == "null":
		return "null"
	case string(t) == "true" || string(t) == "false":
		return "boolean"
	default:
		return "number"
	}
}
