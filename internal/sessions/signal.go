package sessions

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// ErrInterrupted: a signal (Ctrl-C, SIGTERM, a closed terminal) stopped a
// move before it completed. The partial copy was removed and the source was
// never touched; test with errors.Is.
var ErrInterrupted = errors.New("interrupted")

// sigTrap holds SIGINT/SIGTERM/SIGHUP while a move copies, so a Ctrl-C
// removes the temporaries instead of killing the process and leaving
// half-written copies in the target. It is polled (check) from the copying
// goroutine only; no other goroutine touches sig.
type sigTrap struct {
	ch  chan os.Signal
	sig os.Signal
}

// trapSignals starts holding the signals; callers must stop it. Signals the
// process was started with ignored (e.g. SIGHUP under nohup) stay ignored.
func trapSignals() *sigTrap {
	t := &sigTrap{ch: make(chan os.Signal, 1)}
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			signal.Notify(t.ch, s)
		}
	}
	return t
}

// check returns an ErrInterrupted error once a signal has arrived. A nil
// trap never fires.
func (t *sigTrap) check() error {
	if t == nil {
		return nil
	}
	if t.sig == nil {
		select {
		case t.sig = <-t.ch:
		default:
		}
	}
	if t.sig == nil {
		return nil
	}
	return fmt.Errorf("%w by %s", ErrInterrupted, signalName(t.sig))
}

// stop restores the default handling; a signal caught before it returns is
// still reported by check.
func (t *sigTrap) stop() {
	if t != nil {
		signal.Stop(t.ch)
	}
}

func signalName(s os.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "Ctrl-C"
	case syscall.SIGHUP:
		return "the terminal closing"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return s.String()
}
