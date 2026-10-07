package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// ErrCanceled is returned when the user leaves the picker without choosing
// (Esc, Ctrl-C, "q", or end of input).
var ErrCanceled = errors.New("canceled")

// Options configures Pick.
type Options struct {
	Header string
	// Scope says what is listed, e.g. "100 most recent sessions in
	// ~/Code/shop" (see Model.Scope); the fallback prints it under Header.
	Scope string
	// In and Out carry the numbered-prompt fallback (normally stdin and
	// stdout); the raw-mode picker talks to /dev/tty directly.
	In  io.Reader
	Out io.Writer
	// TTY is the terminal device; empty means /dev/tty.
	TTY string
	// Getenv reads TERM and NO_COLOR; nil means os.Getenv.
	Getenv func(string) string
}

// Pick shows items and returns the chosen index. It uses a full-screen
// raw-mode picker on the terminal, and falls back to a numbered prompt on
// In/Out when there is no usable terminal (TERM=dumb, no /dev/tty, raw mode
// refused). The terminal is always restored, including on SIGINT/SIGTERM/
// SIGHUP, after which the signal is re-raised.
func Pick(items []Item, opts Options) (int, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if len(items) == 0 {
		return 0, errors.New("nothing to pick from")
	}
	if getenv("TERM") == "dumb" {
		return Fallback(items, opts)
	}
	path := opts.TTY
	if path == "" {
		path = "/dev/tty"
	}
	tty, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return Fallback(items, opts)
	}
	defer tty.Close()
	fd := int(tty.Fd())
	if !term.IsTerminal(fd) {
		return Fallback(items, opts)
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return Fallback(items, opts)
	}

	var once sync.Once
	restore := func() {
		once.Do(func() {
			// Leave the alternate screen before restoring modes so the
			// shell's screen comes back exactly as it was.
			_, _ = io.WriteString(tty, "\x1b[?1049l")
			_ = term.Restore(fd, state)
		})
	}
	defer restore()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	defer func() {
		signal.Stop(sigs)
		close(done)
	}()
	go func() {
		select {
		case sig := <-sigs:
			restore()
			// Re-raise with the default action so the exit status says
			// what happened; the terminal is already sane.
			signal.Reset(sig)
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(os.Getpid(), s)
			}
		case <-done:
		}
	}()

	if _, err := io.WriteString(tty, "\x1b[?1049h"); err != nil {
		return 0, fmt.Errorf("write to terminal: %w", err)
	}
	m := newModel(items, opts.Header, getenv)
	m.Scope = opts.Scope
	return run(m, tty, tty, func() (int, int) {
		w, h, err := term.GetSize(fd)
		if err != nil || w <= 0 || h <= 0 {
			return 80, 24
		}
		return w, h
	})
}

// newModel is the raw-mode picker's model. NO_COLOR set to any non-empty
// value (no-color.org) turns every SGR sequence off.
func newModel(items []Item, header string, getenv func(string) string) Model {
	m := New(items, header)
	m.NoColor = getenv("NO_COLOR") != ""
	return m
}

// escWait is how long a trailing ESC (or an unfinished "ESC [1;") waits for
// the rest of its sequence before a lone ESC counts as the Esc key. Local
// terminals send a whole sequence in one read; ssh, tmux and slow links can
// split it, and without the wait the first half of an arrow key cancels.
const escWait = 30 * time.Millisecond

// readResult is one Read from the terminal, done on a helper goroutine so
// the loop can stop waiting for it after escWait.
type readResult struct {
	b   []byte
	err error
}

// run is the key loop, separated from terminal setup so tests can drive it
// with pipes.
//
// Reads run on a goroutine, one at a time and only when the loop needs
// input, so that after a selection nothing keeps reading the terminal (type-
// ahead meant for claude is not swallowed). The one exception is the Esc
// timeout: the read in flight is abandoned when run returns; the caller is
// about to exit, and closing the tty ends it.
func run(m Model, in io.Reader, out io.Writer, size func() (int, int)) (int, error) {
	var pending []byte          // undecoded tail of earlier reads
	var reading chan readResult // non-nil while a Read is in flight
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	// apply feeds keys to the model; done reports that run should return.
	apply := func(keys []Key) (idx int, done bool, err error) {
		for _, k := range keys {
			m = m.Update(k)
			if m.Canceled {
				return 0, true, ErrCanceled
			}
			if m.Done {
				i, _ := m.Selected()
				return i, true, nil
			}
		}
		return 0, false, nil
	}
	for {
		w, h := size()
		m = m.Layout(h)
		if err := draw(out, m, w, h); err != nil {
			return 0, err
		}
		if reading == nil {
			ch := make(chan readResult, 1)
			go func() {
				buf := make([]byte, 1024)
				n, err := in.Read(buf)
				ch <- readResult{b: buf[:n], err: err}
			}()
			reading = ch
		}

		var r readResult
		if PendingEscape(pending) {
			if timer == nil {
				timer = time.NewTimer(escWait)
			} else {
				timer.Reset(escWait)
			}
			select {
			case r = <-reading:
				reading = nil
				timer.Stop()
			case <-timer.C:
				// Nothing followed: a lone ESC is the Esc key, a cut-off
				// sequence is dropped. The read stays in flight.
				keys := FlushKeys(pending)
				pending = nil
				if i, done, err := apply(keys); done {
					return i, err
				}
				continue
			}
		} else {
			r = <-reading
			reading = nil
		}

		if len(r.b) > 0 {
			data := append(pending, r.b...)
			var keys []Key
			keys, pending = DecodeKeys(data)
			if i, done, err := apply(keys); done {
				return i, err
			}
		}
		if r.err != nil {
			// Input ended: whatever is pending is all there will be.
			if i, done, err := apply(FlushKeys(pending)); done {
				return i, err
			}
			if errors.Is(r.err, io.EOF) {
				return 0, ErrCanceled
			}
			return 0, fmt.Errorf("read from terminal: %w", r.err)
		}
	}
}

// draw repaints the whole screen in place: home, each line followed by
// erase-to-end-of-line, erase below, then park the cursor on the selection.
func draw(out io.Writer, m Model, w, h int) error {
	lines, at := m.render(w, h)
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l)
		b.WriteString("\x1b[K")
	}
	b.WriteString("\x1b[J")
	if at >= 0 {
		fmt.Fprintf(&b, "\x1b[%d;1H", at+1)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// Fallback is the numbered prompt used without a raw-mode terminal, on
// opts.In and opts.Out. It is line based and screen-reader friendly:
// selectable rows are numbered, Enter picks the first one, text that is not
// a number filters the list, and "q" or end of input cancels.
func Fallback(items []Item, opts Options) (int, error) {
	in, out := opts.In, opts.Out
	if in == nil || out == nil {
		return 0, errors.New("no terminal available for the picker")
	}
	if len(items) == 0 {
		return 0, errors.New("nothing to pick from")
	}
	// number[i] is the 1-based choice number of items[i], 0 when disabled.
	number := make([]int, len(items))
	byNumber := []int{}
	for i, it := range items {
		if !it.Disabled {
			byNumber = append(byNumber, i)
			number[i] = len(byNumber)
		}
	}
	if len(byNumber) == 0 {
		return 0, errors.New("nothing can be picked")
	}
	width := len(strconv.Itoa(len(byNumber)))

	list := func(filter string) {
		m := Model{Items: items, Filter: filter}
		shown := 0
		for _, i := range m.Visible() {
			it := items[i]
			label := strings.Repeat(" ", width) + "  "
			if number[i] > 0 {
				label = fmt.Sprintf("%*d) ", width, number[i])
			}
			line := label + sanitize(it.Title)
			if it.Meta != "" {
				line += "  " + sanitize(it.Meta)
			}
			if it.Note != "" {
				line += " " + sanitize(it.Note)
			}
			fmt.Fprintln(out, line)
			if it.Detail != "" {
				fmt.Fprintln(out, strings.Repeat(" ", width+2)+"  "+Truncate(sanitize(it.Detail), 100))
			}
			if !it.Pinned {
				shown++
			}
		}
		if filter != "" && shown == 0 {
			fmt.Fprintf(out, "No sessions match %q.\n", filter)
		}
	}

	if opts.Header != "" {
		fmt.Fprintln(out, sanitize(opts.Header))
	}
	if opts.Scope != "" {
		fmt.Fprintln(out, sanitize(opts.Scope))
	}
	list("")
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprintf(out, "Choose 1-%d (Enter = 1, text = filter, q = cancel): ", len(byNumber))
		if !sc.Scan() {
			fmt.Fprintln(out)
			if err := sc.Err(); err != nil {
				return 0, fmt.Errorf("read choice: %w", err)
			}
			return 0, ErrCanceled
		}
		ans := strings.TrimSpace(sc.Text())
		switch {
		case ans == "":
			return byNumber[0], nil
		case strings.EqualFold(ans, "q") || strings.EqualFold(ans, "quit"):
			return 0, ErrCanceled
		}
		if n, err := strconv.Atoi(ans); err == nil {
			if n >= 1 && n <= len(byNumber) {
				return byNumber[n-1], nil
			}
			fmt.Fprintf(out, "Enter a number from 1 to %d.\n", len(byNumber))
			continue
		}
		list(ans)
	}
}
