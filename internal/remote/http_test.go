package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const listingJSON = `{
  "generated_at": "2026-09-16T12:20:00Z",
  "tz": "Europe/Istanbul",
  "accounts": [
    {"rank":1,"email":"claude1@sixtynine.agency","nickname":"alpha",
     "added_by":{"dev":"murat","machine_id":"3fa9c2d1e07b"},"added_at":"2026-09-10T08:00:00Z",
     "session":{"used":50,"effective":50,"resets_at":"2026-09-16T14:30:00+02:00","reset_passed":false},
     "week":{"used":28,"effective":28,"resets_at":"2026-09-22T18:00:00Z","reset_passed":false},
     "collected_at":"2026-09-16T12:18:57Z",
     "reporter":{"dev":"murat","machine_id":"3fa9c2d1e07b"},
     "claims":[],"state":"free","busy_by":[],"future_field":"keep me"},
    {"rank":2,"email":"claude2@sixtynine.agency","state":"claimed","busy_by":["ali","can"],
     "claims":[{"dev":"ali","machine_id":"aaaaaaaaaaaa","at":"2026-09-16T15:10:00+03:00"},
               {"dev":"can","machine_id":"cccccccccccc","at":"2026-09-16T12:15:00Z"}]}
  ]
}`

type capture struct {
	method string
	path   string
	rawQ   string
	auth   string
	body   string
}

// newServer returns a client plus a pointer to the last request it saw.
func newServer(t *testing.T, h http.HandlerFunc) (Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.rawQ = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
		got.auth, got.body = r.Header.Get("Authorization"), string(b)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewHTTP(srv.URL+"/", "tok123", 2*time.Second), got
}

func TestListAccountsParsesAndNormalizes(t *testing.T) {
	c, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, listingJSON)
	})
	l, err := c.ListAccounts(context.Background(), "murat")
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if got.auth != "Bearer tok123" {
		t.Errorf("auth header = %q", got.auth)
	}
	if got.path != "/accounts" || got.rawQ != "dev=murat&format=json" {
		t.Errorf("request = %s?%s", got.path, got.rawQ)
	}
	if len(l.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(l.Accounts))
	}
	a := l.Accounts[0]
	if a.Nickname != "alpha" || l.Accounts[1].Nickname != "" {
		t.Errorf("nicknames = %q, %q", a.Nickname, l.Accounts[1].Nickname)
	}
	if a.Session.ResetsAt.Location() != time.UTC {
		t.Errorf("resets_at not normalized to UTC: %v", a.Session.ResetsAt)
	}
	if want := "2026-09-16T12:30:00Z"; a.Session.ResetsAt.Format(time.RFC3339) != want {
		t.Errorf("resets_at = %s, want %s", a.Session.ResetsAt.Format(time.RFC3339), want)
	}
	if len(a.BusyBy) != 0 || len(a.Claims) != 0 {
		t.Errorf("empty busy_by/claims should be empty: %v %v", a.BusyBy, a.Claims)
	}
	b := l.Accounts[1]
	if b.Session != nil {
		t.Errorf("absent window should be nil")
	}
	if strings.Join(b.BusyBy, ",") != "ali,can" {
		t.Errorf("busy_by = %v", b.BusyBy)
	}
	if len(b.Claims) != 2 || b.Claims[0].Dev != "ali" || b.Claims[1].MachineID != "cccccccccccc" {
		t.Fatalf("claims = %+v", b.Claims)
	}
	if b.Claims[0].At.Location() != time.UTC || b.Claims[0].At.Format(time.RFC3339) != "2026-09-16T12:10:00Z" {
		t.Errorf("claim at not normalized to UTC: %v", b.Claims[0].At)
	}
	obj, err := l.Accounts[0].Object()
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	if obj["future_field"] != "keep me" {
		t.Errorf("unknown field lost: %v", obj)
	}
}

func TestGetAccountEscapesEmail(t *testing.T) {
	c, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"email":"a+b@x.com","state":"free"}`)
	})
	if _, err := c.GetAccount(context.Background(), "a+b@x.com", "murat"); err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if want := "/accounts/a+b@x.com"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if want := "dev=murat"; got.rawQ != want {
		t.Errorf("query = %q, want %q", got.rawQ, want)
	}
}

func TestGetAccountPathTraversalIsEscaped(t *testing.T) {
	c, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"email":"x"}`)
	})
	_, _ = c.GetAccount(context.Background(), "../../healthz", "")
	if strings.Contains(got.path, "/healthz") {
		t.Errorf("email not escaped: %q", got.path)
	}
}

func TestPutUsageBody(t *testing.T) {
	c, got := newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	loc := time.FixedZone("IST", 3*3600)
	err := c.PutUsage(context.Background(), "claude1@sixtynine.agency", UsageReport{
		Session:     UsageWindow{Used: 23.5, ResetsAt: time.Unix(1789491600, 0).In(loc)},
		Week:        UsageWindow{Used: 41, ResetsAt: time.Unix(1793631600, 0).In(loc)},
		CollectedAt: time.Unix(1789482657, 500).In(loc),
		Reporter:    Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"},
	})
	if err != nil {
		t.Fatalf("PutUsage: %v", err)
	}
	if got.method != http.MethodPut || got.path != "/accounts/claude1@sixtynine.agency/usage" {
		t.Errorf("request = %s %s", got.method, got.path)
	}
	want := `{"session":{"used":23.5,"resets_at":"2026-09-15T17:00:00Z"},` +
		`"week":{"used":41,"resets_at":"2026-11-02T15:00:00Z"},` +
		`"collected_at":"2026-09-15T14:30:57Z",` +
		`"reporter":{"dev":"murat","machine_id":"3fa9c2d1e07b"}}`
	if got.body != want {
		t.Errorf("body =\n%s\nwant\n%s", got.body, want)
	}
}

func TestClaimRoutes(t *testing.T) {
	c, got := newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	id := Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}
	if err := c.PutClaim(context.Background(), "a+b@x.com", id); err != nil {
		t.Fatalf("PutClaim: %v", err)
	}
	if got.method != http.MethodPut || got.path != "/accounts/a+b@x.com/claim" {
		t.Errorf("put = %s %s", got.method, got.path)
	}
	if got.body != `{"dev":"murat","machine_id":"3fa9c2d1e07b"}` {
		t.Errorf("claim body = %s", got.body)
	}
	// The Worker requires both halves of the identity: removing only by dev
	// would drop the same dev's claim from another machine.
	if err := c.DeleteClaim(context.Background(), "a+b@x.com", id); err != nil {
		t.Fatalf("DeleteClaim: %v", err)
	}
	if got.method != http.MethodDelete || got.path != "/accounts/a+b@x.com/claim" {
		t.Errorf("delete = %s %s", got.method, got.path)
	}
	if got.rawQ != "dev=murat&machine_id=3fa9c2d1e07b" {
		t.Errorf("delete query = %q", got.rawQ)
	}
	if got.body != "" {
		t.Errorf("delete body = %q, want none", got.body)
	}
}

func TestShareRoutes(t *testing.T) {
	status := 201
	c, got := newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
	by := Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}
	if err := c.Share(context.Background(), "claude1@sixtynine.agency", "claude1", by); err != nil {
		t.Fatalf("Share (created): %v", err)
	}
	if got.method != http.MethodPut || got.path != "/accounts/claude1@sixtynine.agency" {
		t.Errorf("share = %s %s", got.method, got.path)
	}
	if got.body != `{"nickname":"claude1","added_by":{"dev":"murat","machine_id":"3fa9c2d1e07b"}}` {
		t.Errorf("share body = %s", got.body)
	}
	status = 204 // already shared: still a success
	if err := c.Share(context.Background(), "claude1@sixtynine.agency", "claude1", by); err != nil {
		t.Fatalf("Share (existed): %v", err)
	}
	if err := c.Unshare(context.Background(), "claude1@sixtynine.agency"); err != nil {
		t.Fatalf("Unshare: %v", err)
	}
	if got.method != http.MethodDelete || got.path != "/accounts/claude1@sixtynine.agency" || got.rawQ != "" {
		t.Errorf("unshare = %s %s?%s", got.method, got.path, got.rawQ)
	}
}

func TestSetNicknameRoute(t *testing.T) {
	c, got := newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if err := c.SetNickname(context.Background(), "a+b@x.com", "alpha"); err != nil {
		t.Fatalf("SetNickname: %v", err)
	}
	if got.method != http.MethodPut || got.path != "/accounts/a+b@x.com/nickname" || got.rawQ != "" {
		t.Errorf("rename = %s %s?%s", got.method, got.path, got.rawQ)
	}
	if got.body != `{"nickname":"alpha"}` {
		t.Errorf("rename body = %s", got.body)
	}
	// The email is a single path segment, never extra route segments.
	_ = c.SetNickname(context.Background(), "../x", "alpha")
	if want := "/accounts/..%2Fx/nickname"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
}

// A nickname held by another account is a 409 for both share and rename; the
// holder's address in the body must not end up in the error message (it can
// reach errors.log, which stays email-free).
func TestNicknameConflict(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		io.WriteString(w, `{"error":"nickname is taken","by":"claude2@sixtynine.agency"}`)
	})
	ctx := context.Background()
	by := Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}
	for name, err := range map[string]error{
		"share":    c.Share(ctx, "claude1@sixtynine.agency", "alpha", by),
		"nickname": c.SetNickname(ctx, "claude1@sixtynine.agency", "alpha"),
	} {
		if !IsConflict(err) {
			t.Errorf("%s: err = %v, want a conflict", name, err)
		}
		if IsNotShared(err) || IsNotFound(err) {
			t.Errorf("%s: a conflict must not read as not shared: %v", name, err)
		}
		if err == nil || err.Error() != "409 nickname is taken" {
			t.Errorf("%s: message = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "@") {
			t.Errorf("%s: message leaks an address: %v", name, err)
		}
	}
	if !IsConflict(fmt.Errorf("share: %w", &Error{Status: 409, Message: "nickname is taken"})) {
		t.Errorf("wrapped conflict lost")
	}
	for _, err := range []error{nil, errors.New("409 nickname is taken"), NotSharedError(), &Error{Status: 400, Message: "bad"}} {
		if IsConflict(err) {
			t.Errorf("IsConflict(%v) = true", err)
		}
	}
}

// Records shared before nicknames existed arrive without one, or with an
// explicit null; both decode to "" and re-encode without the key.
func TestNicknameDecoding(t *testing.T) {
	body := `{"generated_at":"2026-09-16T12:20:00Z","tz":"UTC","accounts":[
	  {"email":"a@x.com","nickname":"alpha","state":"free","claims":[],"busy_by":[]},
	  {"email":"b@x.com","nickname":null,"state":"free","claims":[],"busy_by":[]},
	  {"email":"c@x.com","state":"free","claims":[],"busy_by":[]}]}`
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
	l, err := c.ListAccounts(context.Background(), "murat")
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	var nicks []string
	for _, a := range l.Accounts {
		nicks = append(nicks, a.Nickname)
	}
	if strings.Join(nicks, ",") != "alpha,," {
		t.Errorf("nicknames = %q", nicks)
	}
	if raw, _ := json.Marshal(l.Accounts[1]); strings.Contains(string(raw), "nickname") {
		t.Errorf("empty nickname marshalled: %s", raw)
	}

	c, _ = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"email":"b@x.com","nickname":null,"state":"free"}`)
	})
	a, err := c.GetAccount(context.Background(), "b@x.com", "murat")
	if err != nil || a.Nickname != "" {
		t.Fatalf("GetAccount = %+v, %v", a, err)
	}
	// --json passes the Worker's bytes through, null included.
	obj, err := a.Object()
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	if v, ok := obj["nickname"]; !ok || v != nil {
		t.Errorf("object nickname = %#v (present %v), want the Worker's null", v, ok)
	}
}

func TestNotShared(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"account is not shared"}`)
	})
	ctx := context.Background()
	id := Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}
	for name, err := range map[string]error{
		"usage":   c.PutUsage(ctx, "p@x.com", UsageReport{}),
		"claim":   c.PutClaim(ctx, "p@x.com", id),
		"unclaim": c.DeleteClaim(ctx, "p@x.com", id),
	} {
		if !IsNotShared(err) || !IsNotFound(err) {
			t.Errorf("%s: err = %v, want not shared", name, err)
		}
	}
	if _, err := c.GetAccount(ctx, "p@x.com", "murat"); !IsNotShared(err) {
		t.Errorf("get: err = %v, want not shared", err)
	}
	// Any other 404 (an unknown route on an old Worker) is not "not shared".
	if IsNotShared(&Error{Status: 404, Message: "not found"}) {
		t.Errorf("a plain 404 must not read as not shared")
	}
	if IsNotShared(errors.New("account is not shared")) {
		t.Errorf("only a Worker answer can say not shared")
	}
	if !IsNotShared(fmt.Errorf("claim: %w", NotSharedError())) {
		t.Errorf("wrapped not-shared error lost")
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", 401, `{"error":"unauthorized"}`, "401 unauthorized"},
		{"not found", 404, `{"error":"not found"}`, "404 not found"},
		{"validation", 400, `{"error":"used must be a number in [0,100]","field":"session.used"}`, "400 used must be a number in [0,100] (field: session.used)"},
		{"server error, no json", 503, `<html>oops</html>`, "503 service unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			_, err := c.GetAccount(context.Background(), "a@b.com", "murat")
			var re *Error
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if re.Status != tc.status || re.Error() != tc.want {
				t.Errorf("err = %q (status %d), want %q", re.Error(), re.Status, tc.want)
			}
			if tc.status == 404 && !IsNotFound(err) {
				t.Errorf("IsNotFound = false")
			}
		})
	}
}

func TestMalformedJSON(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"accounts": [`)
	})
	_, err := c.ListAccounts(context.Background(), "murat")
	if err == nil || !strings.Contains(err.Error(), "malformed response") {
		t.Fatalf("err = %v, want malformed response", err)
	}
}

func TestTimeout(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done
	}))
	defer srv.Close()
	defer close(done)
	c := NewHTTP(srv.URL, "tok", 50*time.Millisecond)
	_, err := c.ListAccounts(context.Background(), "murat")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks the URL: %v", err)
	}
}

func TestErrorNeverLeaksEmail(t *testing.T) {
	// Connection refused: the message must not carry the request path.
	c := NewHTTP("http://127.0.0.1:1/", "tok", time.Second)
	err := c.PutUsage(context.Background(), "secret@example.com", UsageReport{})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret@example.com") {
		t.Errorf("email leaked: %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	c := NewHTTP("", "tok", time.Second)
	ctx := context.Background()
	if _, err := c.ListAccounts(ctx, "m"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("ListAccounts err = %v", err)
	}
	if _, err := c.GetAccount(ctx, "a@b.com", "murat"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("GetAccount err = %v", err)
	}
	if err := c.PutUsage(ctx, "a@b.com", UsageReport{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("PutUsage err = %v", err)
	}
	if err := c.PutClaim(ctx, "a@b.com", Identity{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("PutClaim err = %v", err)
	}
	if err := c.DeleteClaim(ctx, "a@b.com", Identity{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("DeleteClaim err = %v", err)
	}
	if err := c.Share(ctx, "a@b.com", "a", Identity{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Share err = %v", err)
	}
	if err := c.SetNickname(ctx, "a@b.com", "a"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("SetNickname err = %v", err)
	}
	if err := c.Unshare(ctx, "a@b.com"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Unshare err = %v", err)
	}
	// A URL without a token is just as unusable.
	if _, err := NewHTTP("https://worker.test", "", time.Second).ListAccounts(ctx, "m"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no token: err = %v", err)
	}
	if want := "Worker URL or token not configured (run: julienning setup, or set JULIENNING_REMOTE_URL and JULIENNING_TOKEN)"; ErrNotConfigured.Error() != want {
		t.Errorf("message = %q", ErrNotConfigured.Error())
	}
}

// A caller's overall budget expiring must not be reported as the per-request
// timeout, which would send the reader looking at the wrong knob.
func TestParentDeadline(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done
	}))
	defer srv.Close()
	defer close(done)
	c := NewHTTP(srv.URL, "tok", 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ListAccounts(ctx, "murat")
	if err == nil || !strings.Contains(err.Error(), "time budget") || strings.Contains(err.Error(), "10s") {
		t.Fatalf("err = %v, want the budget message", err)
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := c.ListAccounts(canceled, "murat"); err == nil || err.Error() != "request canceled" {
		t.Errorf("canceled: err = %v", err)
	}
}

func TestWindowPercent(t *testing.T) {
	eff := 12.5
	cases := []struct {
		w    *Window
		want float64
	}{
		{nil, 0},
		{&Window{Used: 50, Effective: &eff}, 12.5},
		{&Window{Used: 50, ResetPassed: true}, 0},
		{&Window{Used: 50}, 50},
	}
	for _, tc := range cases {
		if got := tc.w.Percent(); got != tc.want {
			t.Errorf("Percent(%+v) = %v, want %v", tc.w, got, tc.want)
		}
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	f := &Fake{}
	ctx := context.Background()
	id := Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}
	_, _ = f.ListAccounts(ctx, "murat")
	_ = f.PutClaim(ctx, "a@b.com", id)
	_ = f.DeleteClaim(ctx, "a@b.com", id)
	_ = f.Share(ctx, "c@d.com", "cee", id)
	_ = f.SetNickname(ctx, "c@d.com", "dee")
	_ = f.Unshare(ctx, "c@d.com")
	if got := strings.Join(f.Ops(), ","); got != "list,claim,unclaim,share,nickname,unshare" {
		t.Errorf("ops = %s", got)
	}
	if c := f.CallsFor("share"); len(c) != 1 || c[0].Nickname != "cee" || c[0].Identity == nil || *c[0].Identity != id {
		t.Errorf("share not recorded with nickname and identity: %+v", c)
	}
	if c := f.CallsFor("nickname"); len(c) != 1 || c[0].Email != "c@d.com" || c[0].Nickname != "dee" {
		t.Errorf("rename not recorded: %+v", c)
	}
	if c := f.Calls[2]; c.Identity == nil || *c.Identity != id || c.Dev != "murat" {
		t.Errorf("unclaim identity not recorded: %+v", c)
	}
	if _, err := f.GetAccount(ctx, "nope@b.com", "murat"); !IsNotShared(err) {
		t.Errorf("GetAccount err = %v, want not shared", err)
	}
	if got := f.CallsFor("get"); len(got) != 1 || got[0].Dev != "murat" {
		t.Errorf("get not recorded with dev: %+v", got)
	}

	f.Reset()
	f.PutClaimErr = errors.New("fallback")
	f.ErrFor = func(op, email string) error {
		if op == "claim" && email == "bad@b.com" {
			return NotSharedError()
		}
		return nil
	}
	if err := f.PutClaim(ctx, "bad@b.com", id); !IsNotShared(err) {
		t.Errorf("ErrFor not consulted: %v", err)
	}
	if err := f.PutClaim(ctx, "ok@b.com", id); err == nil || err.Error() != "fallback" {
		t.Errorf("per-op error not used when ErrFor passes: %v", err)
	}
	if len(f.Calls) != 2 {
		t.Errorf("calls after Reset = %d", len(f.Calls))
	}
}

// Guards the Account struct against silent JSON tag drift, and the Worker
// contract that claims and busy_by are always arrays.
func TestAccountTags(t *testing.T) {
	raw, _ := json.Marshal(Account{Email: "a@b.com", State: "free"})
	if string(raw) != `{"email":"a@b.com","claims":[],"state":"free","busy_by":[]}` {
		t.Errorf("marshal = %s", raw)
	}
	a := &Account{Email: "a@b.com", State: "claimed", BusyBy: []string{"ali"},
		Claims: []Claim{{Dev: "ali", MachineID: "aaaaaaaaaaaa", At: time.Unix(0, 0).UTC()}}}
	obj, err := a.Object()
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	claims, _ := obj["claims"].([]any)
	busy, _ := obj["busy_by"].([]any)
	if len(claims) != 1 || len(busy) != 1 || busy[0] != "ali" {
		t.Errorf("object = %v", obj)
	}
	l := &Listing{Accounts: []Account{{Email: "x@y.com"}}}
	lobj, err := l.Object()
	if err != nil {
		t.Fatalf("Listing.Object: %v", err)
	}
	first := lobj["accounts"].([]any)[0].(map[string]any)
	if _, ok := first["claims"].([]any); !ok {
		t.Errorf("listing account claims = %#v, want an array", first["claims"])
	}
}
