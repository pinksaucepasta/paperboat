package containers

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errContainerExited = errors.New("runtime container exited")

// childTailBytes bounds how much of a runtime container's output is kept to
// explain its exit.
const childTailBytes = 8 << 10

// ChildExit reports that a runtime container stopped. LastLine is the line that
// says why (a runtime's "service failure" line, else its last non-empty output
// line), bounded to childTailBytes, so the service journal
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
	return tail.explainingLine()
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

// explainingLine is the runtime's own "service failure ..." line when the tail holds one: a runtime
// that fails to start prints it before its shutdown records, which would otherwise be the last lines.
func (t *lineTail) explainingLine() string {
	t.mu.Lock()
	text := string(t.buf)
	t.mu.Unlock()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "service failure") {
			return strings.TrimSpace(line)
		}
	}
	return t.lastLine()
}

// handoverLogLines is how much of a failed replacement's output reaches the journal.
const (
	handoverLogLines = 50
	handoverLogBytes = 64 << 10
)

// writeLogTail writes the last handoverLogLines lines of a container's output (timestamped,
// stdout and stderr merged) to w before the container is removed. A failed handover, an exit or a
// deadline, must never leave no evidence (oracle, 2026-11-05: three deadline failures and the
// replacement's output was gone). Best effort: a failed log read writes nothing.
func writeLogTail(w io.Writer, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	logs := exec.CommandContext(ctx, "docker", "logs", "--timestamps", "--tail", strconv.Itoa(handoverLogLines), name)
	logs.WaitDelay = 2 * time.Second
	out, _ := logs.CombinedOutput()
	if len(out) > handoverLogBytes {
		out = out[len(out)-handoverLogBytes:]
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		_, _ = io.WriteString(w, "pbh: handover replacement output: "+line+"\n")
	}
}
