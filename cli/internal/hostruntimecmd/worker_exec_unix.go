//go:build darwin || linux

package hostruntimecmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

// nativeWorkerStarter starts only the fixed Paperboat worker entry point. The runtime
// artifact, socket, user identity, API range, and sealed capability all come
// from native hostd; there is no user-controlled command, argument, or environment
// surface.
type nativeWorkerStarter struct{}

func (nativeWorkerStarter) Start(ctx context.Context, request workerStartRequest) (workerProcess, error) {
	if request.Executable == "" || request.WorkerID == "" || !validNativeWorkerIdentity(request.UID, request.GID) || request.HostdEndpoint == "" || len(request.Capability) != 32 || !request.MutationsDisabled {
		return nil, hostdproto.ErrInvalidConfig
	}
	if os.Geteuid() != 0 && request.UID != os.Geteuid() {
		return nil, hostdproto.ErrInvalidConfig
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}
	command := exec.CommandContext(ctx, request.Executable,
		"daemon", "__runtime-worker", "--socket", request.HostdEndpoint, "--token-fd", "3",
		"--worker-id", request.WorkerID, "--version", request.Version,
		"--api-min", strconv.FormatUint(uint64(request.APIMin), 10),
		"--api-max", strconv.FormatUint(uint64(request.APIMax), 10), "--wait-activation")
	command.ExtraFiles = []*os.File{reader}
	command.Stdin = controlReader
	stdout, err := command.StdoutPipe()
	if err != nil {
		reader.Close()
		writer.Close()
		controlReader.Close()
		controlWriter.Close()
		return nil, err
	}
	stderr := &tailBuffer{limit: 2048}
	command.Stderr = stderr
	if os.Geteuid() == 0 {
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(request.UID), Gid: uint32(request.GID)}}
	}
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		controlReader.Close()
		controlWriter.Close()
		return nil, err
	}
	reader.Close()
	controlReader.Close()
	if _, err := writer.Write(request.Capability); err != nil {
		writer.Close()
		controlWriter.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		controlWriter.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	worker := &execWorker{command: command, control: controlWriter, lines: bufio.NewReader(io.LimitReader(stdout, 128)), workerID: request.WorkerID, stderr: stderr, exited: make(chan struct{})}
	go func() {
		worker.waitErr = command.Wait()
		close(worker.exited)
	}()
	return worker, nil
}

// tailBuffer keeps the last bytes a worker wrote to stderr for the exit report.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

// execWorker uses a private stdin pipe for exactly one fixed activation word.
// Capability bytes travel only through the inherited descriptor, so they can
// never be confused with an activation request.
type execWorker struct {
	command  *exec.Cmd
	control  io.WriteCloser
	lines    *bufio.Reader
	workerID string
	mu       sync.Mutex
	ready    hostdproto.Status
	stderr   *tailBuffer
	exited   chan struct{}
	waitErr  error
}

func (w *execWorker) Exited() <-chan struct{} { return w.exited }

// ExitReason reports the exit status and the last stderr lines; the worker holds
// no secrets in its diagnostics.
func (w *execWorker) ExitReason() string {
	<-w.exited
	reason := "exit status unknown"
	if w.waitErr == nil {
		reason = "exit status 0"
	} else {
		reason = w.waitErr.Error()
	}
	if tail := w.stderr.String(); tail != "" {
		reason += "; last output: " + tail
	}
	return reason
}

func (w *execWorker) Ready(ctx context.Context) (hostdproto.Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready.Epoch != 0 {
		return w.ready, nil
	}
	line, err := readWorkerLineContext(ctx, w.lines)
	if err != nil {
		return hostdproto.Status{}, err
	}
	status, err := parseWorkerStatus(line, "ready", w.workerID)
	if err == nil {
		w.ready = status
	}
	return status, err
}

func (w *execWorker) Activate(ctx context.Context) (hostdproto.Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready.Epoch == 0 || w.control == nil {
		return hostdproto.Status{}, errors.New("worker is not ready")
	}
	if _, err := io.WriteString(w.control, "activate\n"); err != nil {
		return hostdproto.Status{}, err
	}
	if err := w.control.Close(); err != nil {
		return hostdproto.Status{}, err
	}
	w.control = nil
	line, err := readWorkerLineContext(ctx, w.lines)
	if err != nil {
		return hostdproto.Status{}, err
	}
	status, err := parseWorkerStatus(line, "active", w.workerID)
	if err != nil || status.Epoch != w.ready.Epoch || status.APIVersion != w.ready.APIVersion {
		return hostdproto.Status{}, hostdproto.ErrInvalidFrame
	}
	return status, nil
}
func (w *execWorker) Stop(ctx context.Context) error {
	if w.command == nil || w.command.Process == nil {
		return nil
	}
	_ = w.command.Process.Signal(os.Interrupt)
	select {
	case <-ctx.Done():
		_ = w.command.Process.Kill()
		<-w.exited
		return ctx.Err()
	case <-w.exited:
		var exited *exec.ExitError
		if w.waitErr != nil && !errors.As(w.waitErr, &exited) {
			return w.waitErr
		}
		return nil
	case <-time.After(5 * time.Second):
		_ = w.command.Process.Kill()
		<-w.exited
		return nil
	}
}

func parseWorkerStatus(line, state, workerID string) (hostdproto.Status, error) {
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != state || workerID == "" {
		return hostdproto.Status{}, hostdproto.ErrInvalidFrame
	}
	epoch, epochErr := strconv.ParseUint(parts[1], 10, 64)
	api, apiErr := strconv.ParseUint(parts[2], 10, 16)
	if epochErr != nil || apiErr != nil || epoch == 0 || api == 0 {
		return hostdproto.Status{}, hostdproto.ErrInvalidFrame
	}
	hostState := hostdproto.StateCandidate
	if state == "active" {
		hostState = hostdproto.StateActive
	}
	return hostdproto.Status{State: hostState, WorkerID: workerID, Epoch: epoch, APIVersion: uint16(api)}, nil
}

func readWorkerLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > 64 {
		return "", fmt.Errorf("worker lifecycle response exceeds limit")
	}
	return line, nil
}

func readWorkerLineContext(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() { line, err := readWorkerLine(reader); done <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		return r.line, r.err
	}
}

func validNativeWorkerIdentity(uid, gid int) bool { return uid > 0 && gid > 0 || uid == 0 && gid == 0 }
