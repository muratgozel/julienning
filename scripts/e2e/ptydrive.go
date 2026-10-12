//go:build ignore

// ptydrive runs a command on a pseudo-terminal and drives it like a user, so
// scripts/e2e.sh can exercise the session picker and the move confirmation,
// which only run when stdin and stdout are terminals. Standard library only
// (the repo's rule); macOS and Linux.
//
//	go build -o ptydrive scripts/e2e/ptydrive.go
//	ptydrive [-timeout 20s] STEP... -- CMD [ARG...]
//
// Steps run in order:
//
//	expect=TEXT  wait until the output, with terminal escape sequences
//	             removed, contains TEXT after the previous match
//	send=TEXT    type TEXT; Go escapes work (\r Enter, \x1b Esc, \x1b[B down)
//	sleep=DUR    wait, e.g. 400ms (the picker's lone-Esc timer, the move
//	             confirmation's guard against an early Enter)
//
// The command's whole output, escapes removed, goes to stdout. The exit code
// is the command's; 124 when a step or the command times out (the output so
// far and the failing step go to stderr).
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func main() {
	timeout := flag.Duration("timeout", 20*time.Second, "limit for the whole run")
	flag.Parse()
	args := flag.Args()
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep == len(args)-1 {
		fmt.Fprintln(os.Stderr, "usage: ptydrive [-timeout D] STEP... -- CMD [ARG...]")
		os.Exit(2)
	}
	steps, argv := args[:sep], args[sep+1:]

	master, slave, err := openPty()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ptydrive: open pty: %v\n", err)
		os.Exit(2)
	}
	if err := setSize(slave, 40, 120); err != nil {
		fmt.Fprintf(os.Stderr, "ptydrive: set window size: %v\n", err)
		os.Exit(2)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "ptydrive: start %s: %v\n", argv[0], err)
		os.Exit(2)
	}
	slave.Close() // the child holds its own copy; EOF/EIO on master means it is gone

	var mu sync.Mutex
	var raw bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			mu.Lock()
			raw.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return // EOF on macOS, EIO on Linux once the child is gone
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	deadline := time.Now().Add(*timeout)
	screen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return clean(raw.String())
	}
	fail := func(why string) {
		_ = cmd.Process.Kill()
		fmt.Fprintf(os.Stderr, "ptydrive: %s\n--- output so far ---\n%s\n", why, screen())
		os.Exit(124)
	}

	pos := 0
	for _, step := range steps {
		kind, val, ok := strings.Cut(step, "=")
		if !ok {
			fail(fmt.Sprintf("bad step %q (want expect=, send= or sleep=)", step))
		}
		switch kind {
		case "expect":
			for {
				s := screen()
				if i := strings.Index(s[min(pos, len(s)):], val); i >= 0 {
					pos += i + len(val)
					break
				}
				if time.Now().After(deadline) {
					fail(fmt.Sprintf("timed out waiting for %q", val))
				}
				select {
				case <-readDone:
					if !strings.Contains(screen()[min(pos, len(screen())):], val) {
						fail(fmt.Sprintf("the command ended before %q appeared", val))
					}
				case <-time.After(20 * time.Millisecond):
				}
			}
		case "send":
			text, err := strconv.Unquote(`"` + strings.ReplaceAll(val, `"`, `\"`) + `"`)
			if err != nil {
				fail(fmt.Sprintf("bad escape in %q: %v", step, err))
			}
			if _, err := master.Write([]byte(text)); err != nil {
				fail(fmt.Sprintf("send %q: %v", val, err))
			}
		case "sleep":
			d, err := time.ParseDuration(val)
			if err != nil {
				fail(fmt.Sprintf("bad duration in %q", step))
			}
			time.Sleep(d)
		default:
			fail(fmt.Sprintf("unknown step %q", step))
		}
	}

	var werr error
	select {
	case werr = <-waitDone:
	case <-time.After(time.Until(deadline)):
		fail("timed out waiting for the command to exit")
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
	}
	fmt.Print(screen())
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		os.Exit(ee.ExitCode())
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "ptydrive: %v\n", werr)
		os.Exit(2)
	}
}

// escapes matches what a terminal interprets instead of printing: CSI
// sequences (cursor moves, colors, the alternate screen), OSC strings and
// two-byte escapes. Carriage returns go too, so lines read naturally.
var escapes = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>78DEHMc]|\r`)

func clean(s string) string { return escapes.ReplaceAllString(s, "") }

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// openPty returns the master and the slave of a new pseudo-terminal pair.
// The request numbers are the kernel ABI on darwin and on linux amd64/arm64.
func openPty() (*os.File, *os.File, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var name string
	switch runtime.GOOS {
	case "darwin":
		if err := ioctl(m.Fd(), 0x20007454, 0); err != nil { // TIOCPTYGRANT
			return nil, nil, fmt.Errorf("grant: %w", err)
		}
		if err := ioctl(m.Fd(), 0x20007452, 0); err != nil { // TIOCPTYUNLK
			return nil, nil, fmt.Errorf("unlock: %w", err)
		}
		buf := make([]byte, 128)
		if err := ioctl(m.Fd(), 0x40807453, uintptr(unsafe.Pointer(&buf[0]))); err != nil { // TIOCPTYGNAME
			return nil, nil, fmt.Errorf("name: %w", err)
		}
		name = string(buf[:bytes.IndexByte(buf, 0)])
	case "linux":
		var unlock int32
		if err := ioctl(m.Fd(), 0x40045431, uintptr(unsafe.Pointer(&unlock))); err != nil { // TIOCSPTLCK
			return nil, nil, fmt.Errorf("unlock: %w", err)
		}
		var n uint32
		if err := ioctl(m.Fd(), 0x80045430, uintptr(unsafe.Pointer(&n))); err != nil { // TIOCGPTN
			return nil, nil, fmt.Errorf("number: %w", err)
		}
		name = "/dev/pts/" + strconv.Itoa(int(n))
	default:
		return nil, nil, fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	return m, s, nil
}

func setSize(f *os.File, rows, cols uint16) error {
	ws := struct{ rows, cols, x, y uint16 }{rows, cols, 0, 0}
	req := uintptr(0x5414) // TIOCSWINSZ, linux
	if runtime.GOOS == "darwin" {
		req = 0x80087467
	}
	return ioctl(f.Fd(), req, uintptr(unsafe.Pointer(&ws)))
}
