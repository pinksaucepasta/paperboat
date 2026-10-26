//go:build darwin || linux

package pty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	creackpty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

var (
	ErrInvalidCommand    = errors.New("invalid PTY command")
	ErrInvalidCWD        = errors.New("invalid PTY cwd")
	ErrInvalidDimensions = errors.New("invalid PTY dimensions")
	ErrInvalidSignal     = errors.New("invalid PTY signal")
)

type Dimensions struct {
	Columns uint16 `json:"columns"`
	Rows    uint16 `json:"rows"`
}
type Command struct {
	Path       string
	Args       []string
	Env        []string
	CWD        string
	Dimensions Dimensions
}
type ExitResult struct {
	Code     int       `json:"code"`
	Signal   string    `json:"signal,omitempty"`
	ExitedAt time.Time `json:"exited_at"`
}
type Signal string

const (
	Interrupt Signal = "SIGINT"
	Terminate Signal = "SIGTERM"
	Hangup    Signal = "SIGHUP"
	Kill      Signal = "SIGKILL"
)

type Adapter struct {
	root             string
	shellDirectories bool
}

func NewAdapter(root string) (*Adapter, error) {
	resolved, err := resolveDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("root: %w", ErrInvalidCWD)
	}
	return &Adapter{root: resolved}, nil
}

// NewShellAdapter allows managed interactive shells to start in any existing
// absolute directory accessible to the enrolled OS user. NewAdapter retains
// workspace confinement for exec and other scoped consumers.
func NewShellAdapter(root string) (*Adapter, error) {
	adapter, err := NewAdapter(root)
	if err != nil {
		return nil, err
	}
	adapter.shellDirectories = true
	return adapter, nil
}

func (a *Adapter) Start(command Command) (*Process, error) {
	path, err := ValidateProcessPolicy(command.Path, command.Args, command.Env)
	if err != nil {
		return nil, err
	}
	cwd, err := resolveDirectory(command.CWD)
	if err != nil || !filepath.IsAbs(command.CWD) || !a.shellDirectories && !within(a.root, cwd) {
		return nil, ErrInvalidCWD
	}
	if !validDimensions(command.Dimensions) {
		return nil, ErrInvalidCommand
	}
	cmd := exec.Command(path, command.Args...)
	cmd.Args[0] = command.Path
	cmd.Dir = cwd
	cmd.Env = append([]string(nil), command.Env...)
	terminal, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{Cols: command.Dimensions.Columns, Rows: command.Dimensions.Rows})
	if err != nil {
		return nil, fmt.Errorf("start PTY: %w", err)
	}
	// The descriptor returned by creack/pty may be in blocking mode. Rewrap
	// a duplicate marked nonblocking so Go's poller provides input deadlines;
	// NewFile retains that flag when Resize obtains its descriptor later.
	descriptor, err := syscall.Dup(int(terminal.Fd()))
	if err == nil {
		err = syscall.SetNonblock(descriptor, true)
	}
	if err != nil {
		if descriptor >= 0 {
			_ = syscall.Close(descriptor)
		}
		_ = terminal.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	replacement := os.NewFile(uintptr(descriptor), terminal.Name())
	_ = terminal.Close()
	terminal = replacement
	process := &Process{file: terminal, cmd: cmd, done: make(chan struct{})}
	go process.wait()
	return process, nil
}

type Process struct {
	writeMu    sync.Mutex
	interrupts atomic.Uint64 // bumped by Interrupt; a write that sees it change stops
	file       *os.File
	cmd        *exec.Cmd
	done       chan struct{}
	mu         sync.RWMutex
	result     ExitResult
	waitErr    error
	closeOnce  sync.Once
	closeErr   error
}

func (p *Process) Read(buffer []byte) (int, error) {
	n, err := p.file.Read(buffer)
	if errors.Is(err, syscall.EIO) {
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}
	return n, err
}
func (p *Process) Write(data []byte) (int, error) { return p.WriteContext(context.Background(), data) }
func (p *Process) WriteContext(ctx context.Context, data []byte) (int, error) {
	if err := LockInput(ctx, &p.writeMu); err != nil {
		return 0, err
	}
	defer p.writeMu.Unlock()
	return p.writeInterruptible(data, time.Now().Add(inputTimeout(ctx)))
}

// inputChunk bounds how much of a paste can still land after an interrupt: none, because the queue is flushed after
// the write stops, but a chunked write lets Interrupt stop a long paste between chunks as well as inside one.
const inputChunk = 4096

// writeInterruptible sets its deadline before checking for an interrupt, so an Interrupt that bumps the counter after
// the check also moves the deadline after this write's own and aborts it.
func (p *Process) writeInterruptible(data []byte, deadline time.Time) (int, error) {
	defer p.file.SetWriteDeadline(time.Time{})
	start := p.interrupts.Load()
	written := 0
	for written < len(data) {
		if err := p.file.SetWriteDeadline(deadline); err != nil {
			return written, err
		}
		if p.interrupts.Load() != start {
			return written, ErrInterrupted
		}
		n, err := p.file.Write(data[written:min(written+inputChunk, len(data))])
		written += n
		if err != nil {
			if p.interrupts.Load() != start {
				return written, ErrInterrupted
			}
			return written, err
		}
	}
	return written, nil
}

// Interrupt acts as the line discipline would on key when input is stuck: a paste the foreground program never
// reads fills the tty queue and the interrupt key would wait behind it forever. With ISIG on and key the VINTR
// character it stops the pending write, discards the queued input (unless NOFLSH) and sends SIGINT to the
// foreground process group. It reports false, changing nothing, when the terminal would not treat key as an
// interrupt (raw mode, another VINTR), so the caller writes the byte normally. accepted runs once the key is known
// to be an interrupt, before the pending write is stopped, so the caller can mark later queued input as discarded.
func (p *Process) Interrupt(key byte, accepted func()) (bool, error) {
	raw, err := p.file.SyscallConn()
	if err != nil {
		return false, err
	}
	var termios *unix.Termios
	var termiosErr error
	if err := raw.Control(func(fd uintptr) { termios, termiosErr = unix.IoctlGetTermios(int(fd), ioctlGetTermios) }); err != nil {
		return false, err
	}
	if termiosErr != nil {
		return false, termiosErr
	}
	if termios.Lflag&unix.ISIG == 0 || termios.Cc[unix.VINTR] != key {
		return false, nil
	}
	if accepted != nil {
		accepted()
	}
	p.interrupts.Add(1)
	if err := p.file.SetWriteDeadline(time.Now()); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := LockInput(ctx, &p.writeMu); err != nil {
		return false, err
	}
	defer p.writeMu.Unlock()
	var signalErr error
	if err := raw.Control(func(fd uintptr) {
		if termios.Lflag&unix.NOFLSH == 0 {
			flushPendingInput(int(fd))
		}
		group, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
		if err != nil || group <= 0 {
			signalErr = fmt.Errorf("read foreground process group: %w", err)
			return
		}
		signalErr = syscall.Kill(-group, syscall.SIGINT)
	}); err != nil {
		return true, err
	}
	return true, signalErr
}

func (p *Process) Resize(dimensions Dimensions) error {
	if !validDimensions(dimensions) {
		return ErrInvalidDimensions
	}
	// The ioctl runs inside the file's own reference (Control), so a concurrent CloseIO cannot close the
	// descriptor under it; File.Fd would read it unsynchronized and also switch the file to blocking.
	raw, err := p.file.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: dimensions.Columns, Row: dimensions.Rows})
	}); err != nil {
		return err
	}
	return ioctlErr
}
func (p *Process) Signal(signal Signal) error {
	native, ok := nativeSignal(signal)
	if !ok {
		return ErrInvalidSignal
	}
	if p.cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, native); err != nil {
		return fmt.Errorf("signal process group: %w", err)
	}
	return nil
}
func (p *Process) Wait(ctx context.Context) (ExitResult, error) {
	select {
	case <-p.done:
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.result, p.waitErr
	case <-ctx.Done():
		return ExitResult{}, ctx.Err()
	}
}
func (p *Process) CloseIO() error {
	p.closeOnce.Do(func() { p.closeErr = p.file.Close() })
	return p.closeErr
}
func (p *Process) Terminate(ctx context.Context, grace time.Duration) (ExitResult, error) {
	if grace < 0 {
		return ExitResult{}, ErrInvalidCommand
	}
	select {
	case <-p.done:
		result, err := p.Wait(context.Background())
		if closeErr := p.CloseIO(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return result, err
	default:
	}
	if err := p.Signal(Terminate); err != nil && !processGone(err) {
		return ExitResult{}, err
	}
	graceCtx, cancel := context.WithTimeout(ctx, grace)
	result, err := p.Wait(graceCtx)
	cancel()
	if err == nil {
		return result, p.CloseIO()
	}
	if ctx.Err() != nil {
		return ExitResult{}, ctx.Err()
	}
	if err := p.Signal(Kill); err != nil && !processGone(err) {
		return ExitResult{}, err
	}
	result, err = p.Wait(ctx)
	if closeErr := p.CloseIO(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return result, err
}

// processGone reports a signal that found no process: it has exited (its group may already be gone
// before Wait observes the exit), so terminating it only has to wait for the result.
func processGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

func (p *Process) wait() {
	err := p.cmd.Wait()
	result := ExitResult{ExitedAt: time.Now().UTC()}
	if status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			result.Code = 128 + int(status.Signal())
			result.Signal = status.Signal().String()
		} else {
			result.Code = status.ExitStatus()
		}
	} else if p.cmd.ProcessState.Success() {
		result.Code = 0
	} else {
		result.Code = -1
	}
	p.mu.Lock()
	p.result = result
	if _, ok := err.(*exec.ExitError); !ok {
		p.waitErr = err
	}
	p.mu.Unlock()
	close(p.done)
}

func nativeSignal(signal Signal) (syscall.Signal, bool) {
	switch signal {
	case Interrupt:
		return syscall.SIGINT, true
	case Terminate:
		return syscall.SIGTERM, true
	case Hangup:
		return syscall.SIGHUP, true
	case Kill:
		return syscall.SIGKILL, true
	default:
		return 0, false
	}
}
func validDimensions(dimensions Dimensions) bool {
	return dimensions.Columns >= 1 && dimensions.Columns <= 1000 && dimensions.Rows >= 1 && dimensions.Rows <= 1000
}
func validEnvironment(environment []string) bool {
	if len(environment) > maximumProcessEnvironmentEntries {
		return false
	}
	seen := make(map[string]bool, len(environment))
	total := 0
	for _, entry := range environment {
		total += len(entry)
		key, value, ok := strings.Cut(entry, "=")
		folded := strings.ToUpper(key)
		if !ok || key == "" || len(entry) > maximumProcessEnvironmentEntryBytes || total > maximumProcessEnvironmentBytes || strings.ContainsAny(key, "\x00\r\n") || strings.ContainsRune(value, '\x00') || seen[folded] {
			return false
		}
		seen[folded] = true
	}
	return true
}

// ValidateProcessPolicy resolves and validates the static executable, arguments,
// and environment before a runtime advertises terminal readiness.
func ValidateProcessPolicy(path string, arguments, environment []string) (string, error) {
	resolved, err := validateExecutable(path)
	if err != nil || !validArguments(arguments) || !validEnvironment(environment) {
		return "", ErrInvalidCommand
	}
	return resolved, nil
}

func validArguments(arguments []string) bool {
	if len(arguments) > 64 {
		return false
	}
	total := 0
	for _, argument := range arguments {
		total += len(argument)
		if len(argument) > 4096 || total > 64<<10 || strings.ContainsRune(argument, '\x00') {
			return false
		}
	}
	return true
}

func validateExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrInvalidCommand
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", ErrInvalidCommand
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", ErrInvalidCommand
	}
	return resolved, nil
}
func resolveDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrInvalidCWD
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", ErrInvalidCWD
	}
	return filepath.Clean(resolved), nil
}
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
