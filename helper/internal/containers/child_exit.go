package containers

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var errContainerExited = errors.New("runtime container exited")

// childTailBytes bounds how much of a runtime container's output is kept to
// explain its exit.
const childTailBytes = 2 << 10

// ChildExit reports that a runtime container stopped. LastLine is its last
// non-empty output line, bounded to childTailBytes, so the service journal
// shows why the container exited even after the follow stream has stopped.
type ChildExit struct {
	Err      error
	LastLine string
}

func (e *ChildExit) Error() string { return e.Err.Error() }
func (e *ChildExit) Unwrap() error { return e.Err }

// exitError attaches the stopped container's last output line to an exit. The
// container still exists here: removal happens after the caller returns.
func exitError(name string, err error) error {
	if !errors.Is(err, errContainerExited) {
		return err
	}
	return &ChildExit{Err: err, LastLine: lastOutputLine(name)}
}

// lastOutputLine is best effort. A failed log read leaves the line empty, and
// the exit is still reported, so the cleanup error never hides the exit.
func lastOutputLine(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	tail := &lineTail{}
	logs := exec.CommandContext(ctx, "docker", "logs", "--tail", "50", name)
	logs.Stdout, logs.Stderr = tail, tail
	logs.WaitDelay = 2 * time.Second
	_ = logs.Run()
	return tail.lastLine()
}

// lineTail keeps only the last childTailBytes written to it.
type lineTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *lineTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if extra := len(t.buf) - childTailBytes; extra > 0 {
		t.buf = t.buf[:copy(t.buf, t.buf[extra:])]
	}
	return len(p), nil
}

func (t *lineTail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := strings.TrimRight(string(t.buf), " \t\r\n")
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return text
}
