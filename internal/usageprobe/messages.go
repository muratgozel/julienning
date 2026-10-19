package usageprobe

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/paths"
	"github.com/muratgozel/julienning/internal/usage"
)

// Message is one line an interactive command prints about a refresh.
type Message struct {
	Note bool // a note (julienning: note:) rather than a warning
	Text string
}

// Messages renders a refresh for a person at a terminal: accounts by
// nickname and email, dirs as home-shortened paths. now dates the last
// numbers a failed account is shown with.
func (r Report) Messages(now time.Time) []Message {
	nicks := map[string]string{}
	numbersAt := map[string]*time.Time{}
	if r.Listing != nil {
		for _, a := range r.Listing.Accounts {
			e := strings.ToLower(a.Email)
			nicks[e] = a.Nickname
			numbersAt[e] = a.UsageAt()
		}
	}
	label := func(email string) string {
		if n := nicks[email]; n != "" {
			return n + " (" + email + ")"
		}
		return email
	}
	name := func(email string) string {
		if n := nicks[email]; n != "" {
			return n
		}
		return email
	}
	var out []Message
	warn := func(format string, a ...any) { out = append(out, Message{Text: fmt.Sprintf(format, a...)}) }
	if r.RecordErr != nil {
		warn("could not record which account each dir is logged into (%v)", r.RecordErr)
	}
	for _, c := range r.Changes {
		out = append(out, Message{Note: true, Text: fmt.Sprintf(
			"%s was logged into %s and is now logged into %s; the sessions in it stay there, under %s",
			shortenHome(c.Dir), label(c.From), label(c.To), name(c.To))})
	}
	for _, s := range r.SignedOut {
		warn("%s is signed out in %s; sign in again with: julienning login %s", label(s.Email), shortenHome(s.Dir.Dir), name(s.Email))
	}
	for _, p := range r.Problems {
		last := "it has no reported numbers yet"
		if at := numbersAt[p.Email]; at != nil {
			last = "its numbers are from " + usage.Ago(now.Sub(*at))
		}
		warn("could not refresh the usage of %s in %s (%v); %s", label(p.Email), shortenHome(p.Dir.Dir), p.Err, last)
	}
	if r.OldWorker {
		warn("the Worker does not take usage probes yet, so the numbers were not refreshed; " +
			"it needs a redeploy (GitHub Actions, workflow deploy-worker)")
	}
	if r.PostErr != nil {
		warn("could not send the refreshed usage to the Worker (%v); showing the last reported numbers", r.PostErr)
	}
	return out
}

// Print writes Messages to w the way every command prints warnings and
// notes (`julienning: warning: …`, `julienning: note: …`).
func (r Report) Print(w io.Writer, now time.Time) {
	for _, m := range r.Messages(now) {
		kind := "warning"
		if m.Note {
			kind = "note"
		}
		fmt.Fprintf(w, "julienning: %s: %s\n", kind, m.Text)
	}
}

// Log records a background refresh in errors.log: codes and dir names only,
// never an email (errors.log must stay free of them), throttled per dir so an
// outage leaves a breadcrumb, not a flood.
func (r Report) Log(l usage.Logger) {
	if r.RecordErr != nil {
		_ = l.LogThrottledEvery(CodeProbeFailed, "record", "could not record the dirs' logins: "+scrubEmails(r.RecordErr.Error()), usage.BackgroundFailedEvery)
	}
	for _, p := range r.Problems {
		msg := fmt.Sprintf("usage probe of config %s failed: %s", p.Dir.Name, scrubEmails(p.Err.Error()))
		code := CodeProbeFailed
		if p.Shape != "" {
			code, msg = CodeProbeInvalid, msg+" "+p.Shape
		}
		_ = l.LogThrottledEvery(code, p.Dir.Dir, msg, usage.BackgroundFailedEvery)
	}
	for _, s := range r.SignedOut {
		_ = l.LogThrottledEvery(CodeSignedOut, s.Dir.Dir, "config "+s.Dir.Name+" is signed out (Claude Code has no login there); sign in again with julienning login", usage.BackgroundFailedEvery)
	}
	if r.OldWorker {
		_ = l.LogThrottledEvery(CodeProbeFailed, "worker", "the Worker does not know POST /probes; redeploy it", usage.BackgroundFailedEvery)
	}
	if r.PostErr != nil {
		_ = l.LogThrottledEvery(CodeProbeFailed, "post", "could not send usage probes: "+scrubEmails(r.PostErr.Error()), usage.BackgroundFailedEvery)
	}
}

// errors.log codes of the background round.
const (
	CodeProbeFailed  = "PROBE_FAILED"
	CodeProbeInvalid = "PROBE_INVALID"
	CodeSignedOut    = "SIGNED_OUT"
)

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// scrubEmails is a last line of defence for errors.log: no address, whatever
// an error decides to include.
func scrubEmails(s string) string {
	return emailRe.ReplaceAllString(s, "<email>")
}

func shortenHome(path string) string { return paths.ShortenHome(path) }
