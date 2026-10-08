package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxBody caps how much of a response we read; the Worker's answers are tiny
// and a runaway body must never stall the status line path.
const maxBody = 1 << 20

type httpClient struct {
	base    string
	token   string
	timeout time.Duration
	hc      *http.Client
}

// NewHTTP returns a Client talking to the Worker at url. An empty url yields a
// client whose every call fails with ErrNotConfigured, so callers can report
// "not set up" without special-casing the constructor.
func NewHTTP(url, token string, timeout time.Duration) Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &httpClient{
		base:    strings.TrimRight(url, "/"),
		token:   token,
		timeout: timeout,
		hc:      &http.Client{Timeout: timeout},
	}
}

func (c *httpClient) ListAccounts(ctx context.Context, dev string) (*Listing, error) {
	q := url.Values{"format": {"json"}}
	if dev != "" {
		q.Set("dev", dev)
	}
	body, err := c.do(ctx, http.MethodGet, "/accounts", q, nil)
	if err != nil {
		return nil, err
	}
	var l Listing
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, malformed(err)
	}
	var rawDoc struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(body, &rawDoc); err == nil {
		for i := range l.Accounts {
			if i < len(rawDoc.Accounts) {
				l.Accounts[i].Raw = rawDoc.Accounts[i]
			}
		}
	}
	l.Raw = body
	l.normalize()
	return &l, nil
}

// GetAccount sends dev so the Worker can tell the caller's own activity from
// someone else's: without it `current` sees its own report as "in use".
func (c *httpClient) GetAccount(ctx context.Context, email, dev string) (*Account, error) {
	var q url.Values
	if dev != "" {
		q = url.Values{"dev": {dev}}
	}
	body, err := c.do(ctx, http.MethodGet, accountPath(email), q, nil)
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, malformed(err)
	}
	a.Raw = body
	a.normalize()
	return &a, nil
}

func (c *httpClient) PutUsage(ctx context.Context, email string, r UsageReport) error {
	r.Session.ResetsAt = r.Session.ResetsAt.UTC().Truncate(time.Second)
	r.Week.ResetsAt = r.Week.ResetsAt.UTC().Truncate(time.Second)
	r.CollectedAt = r.CollectedAt.UTC().Truncate(time.Second)
	_, err := c.do(ctx, http.MethodPut, accountPath(email)+"/usage", nil, r)
	return err
}

func (c *httpClient) PutExhausted(ctx context.Context, email string, r ExhaustedReport) error {
	if r.ResetsAt != nil {
		t := r.ResetsAt.UTC().Truncate(time.Second)
		r.ResetsAt = &t
	}
	_, err := c.do(ctx, http.MethodPut, accountPath(email)+"/exhausted", nil, r)
	return err
}

func (c *httpClient) PutClaim(ctx context.Context, email string, id Identity) error {
	_, err := c.do(ctx, http.MethodPut, accountPath(email)+"/claim", nil, id)
	return err
}

func (c *httpClient) DeleteClaim(ctx context.Context, email string, id Identity) error {
	q := url.Values{"dev": {id.Dev}, "machine_id": {id.MachineID}}
	_, err := c.do(ctx, http.MethodDelete, accountPath(email)+"/claim", q, nil)
	return err
}

func (c *httpClient) Share(ctx context.Context, email, nickname string, by Identity) error {
	_, err := c.do(ctx, http.MethodPut, accountPath(email), nil, struct {
		Nickname string   `json:"nickname"`
		AddedBy  Identity `json:"added_by"`
	}{nickname, by})
	return err
}

func (c *httpClient) SetNickname(ctx context.Context, email, nickname string) error {
	_, err := c.do(ctx, http.MethodPut, accountPath(email)+"/nickname", nil, struct {
		Nickname string `json:"nickname"`
	}{nickname})
	return err
}

func (c *httpClient) Unshare(ctx context.Context, email string) error {
	_, err := c.do(ctx, http.MethodDelete, accountPath(email), nil, nil)
	return err
}

// accountPath escapes the email so an address with odd characters cannot
// change the route it hits.
func accountPath(email string) string { return "/accounts/" + url.PathEscape(email) }

func (c *httpClient) do(ctx context.Context, method, path string, q url.Values, body any) ([]byte, error) {
	// Without a token every call is a guaranteed 401; say what to fix instead.
	if c.base == "" || c.token == "" {
		return nil, ErrNotConfigured
	}
	parent := ctx
	target := c.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, transportError(parent, err, c.timeout)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, transportError(parent, err, c.timeout)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(resp.StatusCode, raw)
	}
	return raw, nil
}

// transportError strips the URL (it carries the account email) and collapses
// timeouts into one actionable message. When the caller's own deadline or
// cancellation fired first, the per-request timeout is not what expired, so
// the message does not name it.
func transportError(parent context.Context, err error, timeout time.Duration) error {
	switch perr := parent.Err(); {
	case errors.Is(perr, context.DeadlineExceeded):
		return errors.New("request timed out (time budget for this command used up)")
	case errors.Is(perr, context.Canceled):
		return errors.New("request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return fmt.Errorf("request timed out after %s", timeout)
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("request canceled")
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("request failed: %w", ue.Err)
	}
	return fmt.Errorf("request failed: %w", err)
}

func statusError(status int, raw []byte) error {
	msg := strings.ToLower(http.StatusText(status))
	if msg == "" {
		msg = "unexpected status"
	}
	var doc struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}
	if json.Unmarshal(raw, &doc) == nil && doc.Error != "" {
		msg = doc.Error
		if doc.Field != "" {
			msg += " (field: " + doc.Field + ")"
		}
	}
	return &Error{Status: status, Message: msg}
}

func malformed(err error) error {
	return fmt.Errorf("malformed response from remote: %w", err)
}
