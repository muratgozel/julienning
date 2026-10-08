// Package remote is the client for the julienning Worker. Business logic talks
// to the Client interface only: no HTTP status codes, URLs or headers leak out
// of this package, so the commands stay testable with Fake.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotConfigured is returned by every call when no remote URL is set.
var ErrNotConfigured = errors.New("Worker URL or token not configured (run: julienning setup, or set JULIENNING_REMOTE_URL and JULIENNING_TOKEN)")

// Error is a non-2xx answer from the Worker. Message is the Worker's `error`
// field when it sent JSON, else the HTTP status text. It never contains the
// request URL, because it ends up in errors.log which must stay email-free.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s", e.Status, e.Message) }

// IsNotShared reports whether the Worker rejected a call because the email is
// not on the team allowlist (404 with error "account is not shared").
func IsNotShared(err error) bool {
	var re *Error
	return errors.As(err, &re) && re.Status == 404 && re.Message == "account is not shared"
}

// IsConflict reports whether the Worker refused because a nickname is taken
// by another account (409).
func IsConflict(err error) bool {
	var re *Error
	return errors.As(err, &re) && re.Status == 409
}

// IsNotFound reports whether err is a 404 from the Worker.
func IsNotFound(err error) bool {
	var re *Error
	return errors.As(err, &re) && re.Status == 404
}

// Identity is who is claiming an account.
type Identity struct {
	Dev       string `json:"dev"`
	MachineID string `json:"machine_id"`
}

// UsageWindow is one rate-limit window as reported to the Worker.
type UsageWindow struct {
	Used     float64   `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
}

// UsageReport is the PUT /accounts/:email/usage body.
type UsageReport struct {
	Session     UsageWindow `json:"session"`
	Week        UsageWindow `json:"week"`
	CollectedAt time.Time   `json:"collected_at"`
	Reporter    Identity    `json:"reporter"`
}

// Exhausted window names, as the Worker spells them in ExhaustedReport and in
// the listing's exhausted_window.
const (
	WindowSession = "session"
	WindowWeek    = "week"
)

// ExhaustedReport is the PUT /accounts/:email/exhausted body: Claude refused
// a request for a usage limit. ResetsAt nil (sent as null) means the reset is
// not known.
type ExhaustedReport struct {
	Window   string     `json:"window"`
	ResetsAt *time.Time `json:"resets_at"`
	Reporter Identity   `json:"reporter"`
}

// Window is a rate-limit window as returned by the Worker, including the
// computed fields. Absent windows arrive as a nil *Window.
type Window struct {
	Used        float64    `json:"used"`
	Effective   *float64   `json:"effective,omitempty"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	ResetPassed bool       `json:"reset_passed"`
}

// Percent is the usage to show: 0 once the window's reset has passed.
func (w *Window) Percent() float64 {
	if w == nil {
		return 0
	}
	if w.ResetPassed {
		return 0
	}
	if w.Effective != nil {
		return *w.Effective
	}
	return w.Used
}

// Claim is one holder of the advisory "in use" marker. An account can have
// several holders (one per dev+machine with live sessions).
type Claim struct {
	Dev       string    `json:"dev"`
	MachineID string    `json:"machine_id"`
	At        time.Time `json:"at"`
}

// Account is one account in the Worker's JSON response.
type Account struct {
	Rank        int        `json:"rank,omitempty"`
	Email       string     `json:"email"`
	Nickname    string     `json:"nickname,omitempty"` // team-wide; "" for records created before nicknames existed
	Session     *Window    `json:"session,omitempty"`
	Week        *Window    `json:"week,omitempty"`
	CollectedAt *time.Time `json:"collected_at,omitempty"`
	Reporter    *Identity  `json:"reporter,omitempty"`
	Claims      []Claim    `json:"claims"`
	State       string     `json:"state"`
	BusyBy      []string   `json:"busy_by"` // other devs holding or actively using it, in every state

	// Exhausted: a window is at 100%, or Claude refused a request for a
	// limit, so it refuses work until a reset. ExhaustedUntil is the reset
	// after which the account is usable again (with both windows at 100%, the
	// later one); nil when not known, also while Exhausted is true.
	// ExhaustedWin is the Worker's exhausted_window ("session", "week", or
	// null); read it through ExhaustedWindow, which falls back to inferring
	// it. Workers from before these fields omit them, which reads as
	// false/nil.
	Exhausted      bool       `json:"exhausted"`
	ExhaustedUntil *time.Time `json:"exhausted_until"`
	ExhaustedWin   *string    `json:"exhausted_window"`

	// Raw is the exact JSON object the Worker sent, kept so `--json` output can
	// pass through fields this binary does not know about yet.
	Raw json.RawMessage `json:"-"`
}

// IsExhausted reports whether the account cannot take work until a reset.
// State "exhausted" counts as well, so a Worker that sets only one of the two
// still sinks the account.
func (a *Account) IsExhausted() bool {
	return a.Exhausted || a.State == "exhausted"
}

// ExhaustedWindow names the window the account waits on: "session" or
// "week". The Worker's exhausted_window wins; a Worker without it (or with a
// value this binary does not know) leaves the window inferred: the one at
// 100% whose reset is ExhaustedUntil (week on a tie, as the Worker decides).
// "" when the account is not exhausted or the window cannot be told.
func (a *Account) ExhaustedWindow() string {
	if !a.IsExhausted() {
		return ""
	}
	if w := a.ExhaustedWin; w != nil && (*w == WindowSession || *w == WindowWeek) {
		return *w
	}
	if a.ExhaustedUntil == nil {
		return ""
	}
	blocks := func(w *Window) bool {
		return w != nil && w.ResetsAt != nil && w.Percent() >= 100 && w.ResetsAt.Equal(*a.ExhaustedUntil)
	}
	switch {
	case blocks(a.Week):
		return WindowWeek
	case blocks(a.Session):
		return WindowSession
	default:
		return ""
	}
}

// Listing is GET /accounts?format=json.
type Listing struct {
	GeneratedAt time.Time `json:"generated_at"`
	TZ          string    `json:"tz"`
	Accounts    []Account `json:"accounts"`

	// Raw is the whole response body; see Account.Raw.
	Raw json.RawMessage `json:"-"`
}

// Client is the Worker API as the rest of the program sees it. Every account
// the Worker knows is on the team allowlist ("shared").
type Client interface {
	// ListAccounts returns all shared accounts, ranked for dev.
	ListAccounts(ctx context.Context, dev string) (*Listing, error)
	GetAccount(ctx context.Context, email, dev string) (*Account, error)
	// Share adds email to the team allowlist with its team-wide nickname
	// (idempotent for an existing email; a nickname held by another email
	// is a conflict, see IsConflict).
	Share(ctx context.Context, email, nickname string, by Identity) error
	// SetNickname renames a shared account's nickname (IsConflict when taken).
	SetNickname(ctx context.Context, email, nickname string) error
	// Unshare removes email and all its data from the allowlist.
	Unshare(ctx context.Context, email string) error
	PutUsage(ctx context.Context, email string, r UsageReport) error
	// PutExhausted marks a shared account exhausted after Claude refused a
	// request for a usage limit (IsNotShared for an unshared email).
	PutExhausted(ctx context.Context, email string, r ExhaustedReport) error
	// PutClaim adds or refreshes this dev+machine's claim.
	PutClaim(ctx context.Context, email string, id Identity) error
	// DeleteClaim removes only this dev+machine's claim (idempotent).
	DeleteClaim(ctx context.Context, email string, id Identity) error
}

// MarshalJSON keeps the Worker's contract that `claims` and `busy_by` are
// always arrays, so `--json` output built from a Fake (or an account decoded
// from `null`) never prints null for them.
func (a Account) MarshalJSON() ([]byte, error) {
	type plain Account // drops this method, so json.Marshal cannot recurse
	p := plain(a)
	if p.Claims == nil {
		p.Claims = []Claim{}
	}
	if p.BusyBy == nil {
		p.BusyBy = []string{}
	}
	return json.Marshal(p)
}

// Object returns the account as a generic map, preferring the bytes the Worker
// sent so unknown fields survive a decode/encode round trip. Numbers are
// json.Number so they re-marshal with their original literal.
func (a *Account) Object() (map[string]any, error) {
	raw := []byte(a.Raw)
	if len(raw) == 0 {
		var err error
		if raw, err = json.Marshal(a); err != nil {
			return nil, fmt.Errorf("encode account: %w", err)
		}
	}
	return decodeObject(raw)
}

// Object returns the listing as a generic map; see Account.Object.
func (l *Listing) Object() (map[string]any, error) {
	raw := []byte(l.Raw)
	if len(raw) == 0 {
		var err error
		if raw, err = json.Marshal(l); err != nil {
			return nil, fmt.Errorf("encode listing: %w", err)
		}
	}
	return decodeObject(raw)
}

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode remote JSON: %w", err)
	}
	return m, nil
}

// normalize forces every timestamp to UTC so callers can render in any zone
// without wondering which offset the Worker happened to send.
func (l *Listing) normalize() {
	l.GeneratedAt = l.GeneratedAt.UTC()
	for i := range l.Accounts {
		l.Accounts[i].normalize()
	}
}

func (a *Account) normalize() {
	utcPtr(&a.CollectedAt)
	utcPtr(&a.ExhaustedUntil)
	a.Session.normalize()
	a.Week.normalize()
	for i := range a.Claims {
		a.Claims[i].At = a.Claims[i].At.UTC()
	}
}

func (w *Window) normalize() {
	if w != nil {
		utcPtr(&w.ResetsAt)
	}
}

func utcPtr(p **time.Time) {
	if *p != nil {
		t := (*p).UTC()
		*p = &t
	}
}
