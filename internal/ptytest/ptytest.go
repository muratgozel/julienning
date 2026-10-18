// Package ptytest opens pseudo-terminals for tests that need stdin and
// stdout to be terminals (tmux mode's launch rule, the shell helper). It is
// imported by tests only; the code mirrors scripts/e2e/ptydrive.go
// (standard library ioctls, darwin and linux).
package ptytest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// Open returns the master and the slave of a new pseudo-terminal pair. The
// request numbers are the kernel ABI on darwin and on linux amd64/arm64.
func Open() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var name string
	switch runtime.GOOS {
	case "darwin":
		if err := ioctl(m.Fd(), 0x20007454, 0); err != nil { // TIOCPTYGRANT
			m.Close()
			return nil, nil, fmt.Errorf("grant: %w", err)
		}
		if err := ioctl(m.Fd(), 0x20007452, 0); err != nil { // TIOCPTYUNLK
			m.Close()
			return nil, nil, fmt.Errorf("unlock: %w", err)
		}
		buf := make([]byte, 128)
		if err := ioctl(m.Fd(), 0x40807453, uintptr(unsafe.Pointer(&buf[0]))); err != nil { // TIOCPTYGNAME
			m.Close()
			return nil, nil, fmt.Errorf("name: %w", err)
		}
		name = string(buf[:bytes.IndexByte(buf, 0)])
	case "linux":
		var unlock int32
		if err := ioctl(m.Fd(), 0x40045431, uintptr(unsafe.Pointer(&unlock))); err != nil { // TIOCSPTLCK
			m.Close()
			return nil, nil, fmt.Errorf("unlock: %w", err)
		}
		var n uint32
		if err := ioctl(m.Fd(), 0x80045430, uintptr(unsafe.Pointer(&n))); err != nil { // TIOCGPTN
			m.Close()
			return nil, nil, fmt.Errorf("number: %w", err)
		}
		name = "/dev/pts/" + strconv.Itoa(int(n))
	default:
		m.Close()
		return nil, nil, fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

// Run starts cmd with stdin, stdout and stderr on a new pseudo-terminal and
// returns everything it wrote (carriage returns removed) once it exits, or
// an error after timeout. ok is false when no pseudo-terminal can be opened,
// so callers can skip.
func Run(cmd *exec.Cmd, timeout time.Duration) (out string, ok bool, err error) {
	master, slave, err := Open()
	if err != nil {
		return "", false, nil
	}
	defer master.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		slave.Close()
		return "", true, err
	}
	slave.Close()
	var buf lockedBuffer
	read := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, master) // ends with EOF (darwin) or EIO (linux)
		close(read)
	}()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err = <-wait:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-wait
		err = fmt.Errorf("timed out after %s", timeout)
	}
	select {
	case <-read:
	case <-time.After(2 * time.Second):
	}
	return strings.ReplaceAll(buf.String(), "\r", ""), true, err
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
