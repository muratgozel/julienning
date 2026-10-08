package remote

import (
	"context"
	"sync"
)

// Call is one recorded request against a Fake.
type Call struct {
	Op        string // "list", "get", "share", "nickname", "unshare", "usage", "exhausted", "claim", "unclaim"
	Nickname  string
	Email     string
	Dev       string
	Usage     *UsageReport
	Exhausted *ExhaustedReport
	Identity  *Identity
}

// Fake is an in-memory Client for tests. Zero value is usable: it returns an
// empty listing and, like the Worker, a 404 "account is not shared" for an
// unknown GetAccount.
type Fake struct {
	mu sync.Mutex

	Listing  *Listing
	Accounts map[string]*Account // by email, for GetAccount

	ListErr         error
	GetErr          error
	PutUsageErr     error
	PutExhaustedErr error
	PutClaimErr     error
	DeleteClaimErr  error
	ShareErr        error
	SetNicknameErr  error
	UnshareErr      error

	// ErrFor, when set, is consulted first for every call; a non-nil result is
	// returned instead of the per-operation error above. Lets one test make a
	// single account fail while others succeed.
	ErrFor func(op, email string) error

	Calls []Call
}

var _ Client = (*Fake)(nil)

// NotSharedError is the Worker's answer for an email that is not allowlisted.
func NotSharedError() error { return &Error{Status: 404, Message: "account is not shared"} }

func (f *Fake) record(c Call, fallback error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, c)
	if f.ErrFor != nil {
		if err := f.ErrFor(c.Op, c.Email); err != nil {
			return err
		}
	}
	return fallback
}

// Ops returns the recorded operation names in order.
func (f *Fake) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ops := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		ops[i] = c.Op
	}
	return ops
}

// CallsFor returns the recorded calls of one operation, in order.
func (f *Fake) CallsFor(op string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.Calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// Reset forgets the recorded calls.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = nil
}

func (f *Fake) ListAccounts(_ context.Context, dev string) (*Listing, error) {
	if err := f.record(Call{Op: "list", Dev: dev}, f.ListErr); err != nil {
		return nil, err
	}
	if f.Listing == nil {
		return &Listing{}, nil
	}
	return f.Listing, nil
}

func (f *Fake) GetAccount(_ context.Context, email, dev string) (*Account, error) {
	if err := f.record(Call{Op: "get", Email: email, Dev: dev}, f.GetErr); err != nil {
		return nil, err
	}
	if a, ok := f.Accounts[email]; ok {
		return a, nil
	}
	return nil, NotSharedError()
}

func (f *Fake) PutUsage(_ context.Context, email string, r UsageReport) error {
	return f.record(Call{Op: "usage", Email: email, Usage: &r}, f.PutUsageErr)
}

func (f *Fake) PutExhausted(_ context.Context, email string, r ExhaustedReport) error {
	id := r.Reporter
	return f.record(Call{Op: "exhausted", Email: email, Dev: id.Dev, Exhausted: &r, Identity: &id}, f.PutExhaustedErr)
}

func (f *Fake) PutClaim(_ context.Context, email string, id Identity) error {
	return f.record(Call{Op: "claim", Email: email, Dev: id.Dev, Identity: &id}, f.PutClaimErr)
}

func (f *Fake) DeleteClaim(_ context.Context, email string, id Identity) error {
	return f.record(Call{Op: "unclaim", Email: email, Dev: id.Dev, Identity: &id}, f.DeleteClaimErr)
}

func (f *Fake) Share(_ context.Context, email, nickname string, by Identity) error {
	return f.record(Call{Op: "share", Email: email, Nickname: nickname, Dev: by.Dev, Identity: &by}, f.ShareErr)
}

func (f *Fake) SetNickname(_ context.Context, email, nickname string) error {
	return f.record(Call{Op: "nickname", Email: email, Nickname: nickname}, f.SetNicknameErr)
}

func (f *Fake) Unshare(_ context.Context, email string) error {
	return f.record(Call{Op: "unshare", Email: email}, f.UnshareErr)
}
