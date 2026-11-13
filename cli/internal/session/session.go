// Package session runs the transparent terminal wrapper: it puts the local
// terminal into raw mode, streams bytes to/from the remote PTY, propagates
// window resizes, and passes the remote exit code back. Correct raw-mode
// handling and clean teardown are the UX bar.
package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"io"
	"os"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"golang.org/x/term"
)

const terminalOutputBufferBytes = 128 * 1024

const terminalCleanupSequence = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1015l\x1b[?2004l\x1b[?1049l\x1b[?25h"
const bracketedPasteEnableSequence = "\x1b[?2004h"

type RunOption func(*runOptions)

type runOptions struct {
	outputBufferBytes int
	output            io.Writer
	remoteSize        func() (uint16, uint16)
	readOnly          func() bool
	bracketedPaste    bool
}

// WithReadOnly keeps input and resize local. Ctrl-C detaches without signaling the shell.
func WithReadOnly() RunOption {
	return WithReadOnlyWhile(func() bool { return true })
}

// WithReadOnlyWhile is WithReadOnly for as long as readOnly reports true, so a shared terminal
// whose role changes while it is joined starts or stops sending input without a rejoin.
func WithReadOnlyWhile(readOnly func() bool) RunOption {
	return func(options *runOptions) { options.readOnly = readOnly }
}

func (o runOptions) isReadOnly() bool { return o.readOnly != nil && o.readOnly() }

// readOnlyResize drops local resizes while the session is read-only.
type readOnlyResize struct {
	tunnel.Conn
	readOnly func() bool
}

func (c readOnlyResize) Resize(rows, cols uint16) error {
	if c.readOnly() {
		return nil
	}
	return c.Conn.Resize(rows, cols)
}

var errViewerDetached = errors.New("viewer detached")

func WithOutputBufferBytes(size int) RunOption {
	return func(options *runOptions) {
		if size > 0 {
			options.outputBufferBytes = size
		}
	}
}

// WithOutput sends remote terminal bytes through output. A status renderer can
// use this to serialize remote output with local redraws.
func WithOutput(output io.Writer) RunOption {
	return func(options *runOptions) {
		if output != nil {
			options.output = output
		}
	}
}

// WithRemoteSize supplies the remote PTY viewport for initial and resize
// propagation.
func WithRemoteSize(size func() (uint16, uint16)) RunOption {
	return func(options *runOptions) { options.remoteSize = size }
}

// WithBracketedPaste enables xterm's standard bracketed-paste mode while the
// local terminal is attached. This lets the caller safely distinguish paste
// input from ordinary typing. It is only meaningful for interactive terminals.
func WithBracketedPaste() RunOption {
	return func(options *runOptions) { options.bracketedPaste = true }
}

// Run wires the local terminal to conn. stdinSink is the writer local input is
// copied into — typically the paste interceptor wrapping conn. Run blocks until
// the remote session ends (or ctx is cancelled) and returns the remote exit
// code.
func Run(ctx context.Context, conn tunnel.Conn, stdinSink io.WriteCloser, opts ...RunOption) (int, error) {
	options := runOptions{outputBufferBytes: terminalOutputBufferBytes, output: os.Stdout}
	for _, option := range opts {
		option(&options)
	}
	inputCtx, cancelInput := context.WithCancel(ctx)
	defer cancelInput()
	stdinFd := int(os.Stdin.Fd())
	restore := func() {}
	isTerminal := term.IsTerminal(stdinFd)
	if isTerminal {
		oldState, err := term.MakeRaw(stdinFd)
		if err != nil {
			return 1, err
		}
		restore = func() { _ = term.Restore(stdinFd, oldState) }
	}
	defer restore()
	if isTerminal {
		resetTerminalEmulator := func() { _, _ = options.output.Write([]byte(terminalCleanupSequence)) }
		resetTerminalEmulator()
		defer resetTerminalEmulator()
		if options.bracketedPaste {
			_, _ = options.output.Write([]byte(bracketedPasteEnableSequence))
		}
	}

	// Propagate the initial size and subsequent resizes.
	stopResize := watchResize(readOnlyResize{Conn: conn, readOnly: options.isReadOnly}, options.remoteSize)
	defer stopResize()

	// Remote -> local. Ends when the remote PTY closes; that is normal EOF, not
	// an error, so it does not by itself end the session — conn.Wait does.
	outputDone := make(chan struct{})
	streamErr := make(chan error, 2)
	go func() {
		defer close(outputDone)
		buf := make([]byte, options.outputBufferBytes)
		var outputOffset uint64
		// Terminal.app and ConPTY obey the host's in-band resize broadcast by resizing the window: see resizeFilter
		resizes := &resizeWriter{out: options.output}
		defer resizes.Flush()
		var sink io.Writer = resizes
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				diagnostics.TerminalHop("client_terminal_read", "local", outputOffset, outputOffset+uint64(n), time.Now())
				diagnostics.TerminalHop("client_terminal_write_start", "local", outputOffset, outputOffset+uint64(n), time.Now())
				started := time.Now()
				if _, writeErr := sink.Write(buf[:n]); writeErr != nil {
					streamErr <- fmt.Errorf("write terminal output: %w", writeErr)
					return
				}
				diagnostics.TerminalHop("client_terminal_write_end", "local", outputOffset, outputOffset+uint64(n), time.Now())
				outputOffset += uint64(n)
				if observer, ok := conn.(interface{ ObserveLocalWrite(int, time.Duration) }); ok {
					observer.ObserveLocalWrite(n, time.Since(started))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Local -> remote (through the paste interceptor). Stdin keeps being read
	// while the sink is blocked on input the remote does not take (a paste into
	// a program that never reads), so a later Ctrl-C is still seen: it drops the
	// unsent backlog, which the host would discard anyway, and goes next (F3-11).
	// Reads made meanwhile are joined, so small tty reads (macOS) do not each
	// take a slot of the transport's input window. Like mosh, which reads stdin
	// on every select (mosh/src/frontend/stmclient.cc:490).
	inputDone := make(chan struct{})
	input := &pendingInput{}
	input.ready = sync.NewCond(&input.mu)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		detached := false
		defer func() { input.finish(detached) }()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := readLocalInput(inputCtx, os.Stdin, buf)
			if options.isReadOnly() {
				if bytes.IndexByte(buf[:n], 3) >= 0 {
					detached = true
					streamErr <- errViewerDetached
					return
				}
			} else if n > 0 && !input.push(buf[:n]) {
				return
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		defer close(inputDone)
		defer func() { input.stop(); <-readerDone }()
		for {
			data, detached, ok := input.take()
			if detached {
				return
			}
			if !ok {
				break
			}
			started := time.Now()
			if _, writeErr := stdinSink.Write(data); writeErr != nil {
				if errors.Is(writeErr, tunnel.ErrWriteUncertain) {
					if discarder, ok := stdinSink.(interface{ Discard() }); ok {
						discarder.Discard()
					}
					continue
				}
				streamErr <- fmt.Errorf("send terminal input: %w", writeErr)
				return
			}
			if observer, ok := conn.(interface{ ObserveInputWrite(int, time.Duration) }); ok {
				observer.ObserveInputWrite(len(data), time.Since(started))
			}
		}
		_ = stdinSink.Close()
		if halfCloser, ok := conn.(tunnel.InputHalfCloser); ok {
			if err := halfCloser.CloseWrite(); err != nil && !errors.Is(err, tunnel.ErrInputEOFUnsupported) {
				streamErr <- fmt.Errorf("close terminal input: %w", err)
			}
		}
	}()
	stopInput := func() {
		if aborter, ok := stdinSink.(interface{ Abort() }); ok {
			aborter.Abort()
		}
		cancelInput()
	}

	// The remote exit code is the source of truth for when we're done.
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := conn.Wait()
		done <- result{code, err}
	}()
	var sinkErrors <-chan error
	if source, ok := stdinSink.(interface{ Errors() <-chan error }); ok {
		sinkErrors = source.Errors()
	}

	select {
	case <-ctx.Done():
		// External cancellation (e.g. parent signalled): tear down and report.
		// A remote exit that is already in is the true result; a user's Ctrl-C returns at once.
		select {
		case r := <-done:
			stopInput()
			_ = conn.Close()
			<-outputDone
			<-inputDone
			return r.code, r.err
		default:
		}
		stopInput()
		_ = conn.Close()
		<-outputDone
		<-inputDone
		<-done
		return 130, nil
	case r := <-done:
		stopInput()
		_ = conn.Close()
		<-outputDone
		<-inputDone
		return r.code, r.err
	case streamError := <-streamErr:
		stopInput()
		_ = conn.Close()
		<-outputDone
		<-inputDone
		<-done
		if errors.Is(streamError, errViewerDetached) {
			return 0, nil
		}
		return 1, streamError
	case sinkError := <-sinkErrors:
		stopInput()
		_ = conn.Close()
		<-outputDone
		<-inputDone
		<-done
		return 1, fmt.Errorf("send terminal input: %w", sinkError)
	}
}

// watchResize sends the current terminal size to conn immediately and on every
// local resize (SIGWINCH; a size poll on Windows), returning a stop function.
func watchResize(conn tunnel.Conn, remoteSize func() (uint16, uint16)) (stop func()) {
	pushSize(conn, remoteSize)
	ch := make(chan os.Signal, 1)
	stopWinch := notifyWinch(ch)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				pushSize(conn, remoteSize)
			case <-done:
				return
			}
		}
	}()
	return func() {
		stopWinch()
		close(done)
	}
}

func pushSize(conn tunnel.Conn, remoteSize func() (uint16, uint16)) {
	if remoteSize != nil {
		cols, rows := remoteSize()
		if cols > 0 && rows > 0 {
			_ = conn.Resize(rows, cols)
		}
		return
	}
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	_ = conn.Resize(uint16(h), uint16(w))
}

// ErrNotTerminal is returned by callers that require an interactive terminal.
var ErrNotTerminal = errors.New("stdin is not a terminal")

// maxPendingInputBytes bounds input read but not yet taken by the sink; past it
// the reader waits for the sink, except for a Ctrl-C.
const maxPendingInputBytes = 4 << 20

// pendingInput hands stdin bytes from the reader to the sink writer.
type pendingInput struct {
	mu       sync.Mutex
	ready    *sync.Cond
	data     []byte
	eof      bool
	detached bool
	stopped  bool
}

// push queues p; a Ctrl-C behind a backlog drops it. It returns false
// once the writer has stopped.
func (q *pendingInput) push(p []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	interrupt := bytes.IndexByte(p, 3) >= 0
	for !q.stopped && !interrupt && len(q.data) >= maxPendingInputBytes {
		q.ready.Wait()
	}
	if interrupt && len(q.data) > 0 {
		// The sink is stuck: what was read before the Ctrl-C, in this read too, is the backlog it drops.
		q.data = q.data[:0]
		p = p[bytes.LastIndexByte(p, 3):]
	}
	q.data = append(q.data, p...)
	q.ready.Broadcast()
	return !q.stopped
}

func (q *pendingInput) finish(detached bool) {
	q.mu.Lock()
	q.eof, q.detached = true, detached
	q.ready.Broadcast()
	q.mu.Unlock()
}

func (q *pendingInput) stop() {
	q.mu.Lock()
	q.stopped = true
	q.ready.Broadcast()
	q.mu.Unlock()
}

// take waits for input; ok is false at end of input once everything was taken.
func (q *pendingInput) take() (data []byte, detached, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.data) == 0 && !q.eof {
		q.ready.Wait()
	}
	if q.detached {
		return nil, true, false
	}
	data, q.data = q.data, nil
	q.ready.Broadcast()
	return data, false, len(data) > 0
}
