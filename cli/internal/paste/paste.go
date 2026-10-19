// Package paste implements the bracketed-paste interceptor: the client-side
// stream logic that detects a pasted local image, uploads it, and rewrites the
// paste to a VM-side path before the remote agent sees it. It is the risk
// center of the CLI and is covered by unit tests.
//
// Guarantees:
//   - Pasted text that is not a validated absolute file path passes through untouched.
//   - Paste framing (ESC[200~ … ESC[201~) and ordering are preserved.
//   - Upload holds only the affected paste; the rest of the stream keeps
//     flowing, and remote output runs on a separate goroutine so the PTY never
//     deadlocks.
//   - Fail open, visibly: on any detection/upload failure the original paste is
//     emitted unchanged and a notice is written to the local terminal.
package paste

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

var (
	startMarker = []byte("\x1b[200~")
	endMarker   = []byte("\x1b[201~")
)

// DefaultUploadTimeout bounds how long a single paste is held for upload.
const DefaultUploadTimeout = 10 * time.Minute

// DefaultPartialFlushDelay bounds how long bytes that could begin a paste
// start marker (e.g. a bare ESC keypress) are withheld while waiting for the
// rest of the marker. Without this flush, a lone ESC would not reach the
// remote TUI until the next keypress.
const DefaultPartialFlushDelay = time.Millisecond

const (
	defaultQueueChunkSize = 32 * 1024
	defaultQueueChunks    = 32
)

// Interceptor wraps the writer feeding the remote PTY. Feed stdin bytes to it
// via Write; it forwards them to dest, rewriting local file-path pastes along the way.
type Interceptor struct {
	ctx             context.Context
	cancel          context.CancelFunc
	policy          *Policy
	dest            io.Writer
	notify          io.Writer
	failureReporter func(error) string
	timeout         time.Duration
	flushDelay      time.Duration
	watchDirs       []string
	tempPatterns    []string
	queueChunkSize  int
	queueChunks     int
	input           chan []byte
	completed       chan uploadCompletion
	done            chan struct{}
	closeOnce       sync.Once
	lifecycleMu     sync.RWMutex
	closed          bool
	pressureOnce    sync.Once
	queued          atomic.Int64
	pendingUploads  atomic.Int64
	uploadSeq       atomic.Uint64
	destMu          sync.Mutex
	directInput     bool
	lifecycle       func(LifecycleEvent)
	stateMu         sync.Mutex
	errMu           sync.Mutex
	err             error
	errCh           chan error

	buf     []byte
	inPaste bool
}

// WithTimeout sets the per-paste upload timeout.

// Abort cancels any in-flight upload during terminal teardown.
func (i *Interceptor) Abort() {
	i.setError(context.Canceled)
	i.cancel()
}
func (i *Interceptor) Discard() {
	i.stateMu.Lock()
	i.buf = nil
	i.inPaste = false
	i.stateMu.Unlock()
}

// Write consumes p, forwarding processed bytes to dest. It always reports the
// full input as written (the interceptor owns buffering) unless dest errors.
func (i *Interceptor) Write(p []byte) (int, error) {
	i.lifecycleMu.RLock()
	defer i.lifecycleMu.RUnlock()
	if i.closed {
		return 0, io.ErrClosedPipe
	}
	if i.directInput && len(p) > 0 && i.queued.Load() == 0 {
		// Wait for the parser to finish the preceding batch before deciding
		// whether these bytes are ordinary terminal input. TryLock made this
		// path fall back to the async queue whenever an upload was being
		// started, which both delayed keystrokes and exposed destination writes
		// to callers concurrently reading their output buffer.
		i.stateMu.Lock()
		if !i.inPaste && len(i.buf) == 0 && !bytes.Contains(p, startMarker) && partialSuffix(p, startMarker) == 0 {
			n, err := i.writeDest(p)
			i.stateMu.Unlock()
			if errors.Is(err, tunnel.ErrWriteUncertain) {
				if discarder, ok := i.dest.(interface{ Discard() }); ok {
					discarder.Discard()
				}
				return len(p), nil
			}
			if err != nil {
				i.setError(err)
			}
			return n, err
		}
		i.stateMu.Unlock()
	}
	written := 0
	for len(p) > 0 {
		select {
		case <-i.done:
			return written, i.result()
		default:
		}
		n := len(p)
		if n > i.queueChunkSize {
			n = i.queueChunkSize
		}
		chunk := append([]byte(nil), p[:n]...)
		i.queued.Add(1)
		select {
		case i.input <- chunk:
			written += n
			p = p[n:]
			select {
			case <-i.done:
				return written, i.result()
			default:
			}
			continue
		default:
			i.pressureOnce.Do(func() {
				i.warn("local input queue is full; waiting for file upload")
			})
		}
		select {
		case i.input <- chunk:
			written += n
			p = p[n:]
			select {
			case <-i.done:
				return written, i.result()
			default:
			}
		case <-i.done:
			i.queued.Add(-1)
			return written, i.result()
		case <-i.ctx.Done():
			i.queued.Add(-1)
			return written, i.ctx.Err()
		}
	}
	return written, nil
}

// Close flushes any buffered normal bytes. A partial (unterminated) paste is
// flushed verbatim so nothing is lost on disconnect.
func (i *Interceptor) Close() error {
	i.closeOnce.Do(func() {
		i.lifecycleMu.Lock()
		i.closed = true
		close(i.input)
		i.lifecycleMu.Unlock()
	})
	<-i.done
	return i.result()
}

// Errors reports fatal asynchronous destination failures to the session loop.
func (i *Interceptor) Errors() <-chan error { return i.errCh }

func (i *Interceptor) run() {
	defer close(i.done)
	// flushTimer forwards a withheld partial start-marker prefix (e.g. a bare
	// ESC keypress) when the rest of the marker does not arrive in time. Real
	// paste markers arrive in a single terminal write, so the timer firing
	// means the bytes were keyboard input, not a paste.
	flushTimer := time.NewTimer(time.Hour)
	if !flushTimer.Stop() {
		<-flushTimer.C
	}
	defer flushTimer.Stop()
	timerArmed := false
	inputClosed := false
	input := i.input
	nextCompletion := uint64(0)
	completionBuffer := make(map[uint64]uploadCompletion)
	rearmFlush := func() {
		if timerArmed {
			if !flushTimer.Stop() {
				<-flushTimer.C
			}
			timerArmed = false
		}
		if i.flushDelay <= 0 {
			return
		}
		i.stateMu.Lock()
		pending := !i.inPaste && len(i.buf) > 0
		i.stateMu.Unlock()
		if pending {
			flushTimer.Reset(i.flushDelay)
			timerArmed = true
		}
	}
	// An upload completion may race with ordinary bytes that were already
	// accepted by Write. Drain that accepted input before publishing the
	// completion so a later paste cannot overtake bytes that followed it in
	// the terminal stream.
	drainAcceptedInput := func() error {
		for input != nil {
			select {
			case p, ok := <-input:
				if !ok {
					inputClosed = true
					input = nil
					return nil
				}
				i.queued.Add(-1)
				i.stateMu.Lock()
				i.buf = append(i.buf, p...)
				err := i.drain()
				i.stateMu.Unlock()
				if err != nil {
					return err
				}
			default:
				return nil
			}
		}
		return nil
	}
	handleWriteErr := func(err error) (fatal bool) {
		if errors.Is(err, tunnel.ErrWriteUncertain) {
			i.Discard()
			if discarder, ok := i.dest.(interface{ Discard() }); ok {
				discarder.Discard()
			}
			return false
		}
		i.setError(err)
		return true
	}
	for {
		select {
		case <-i.ctx.Done():
			i.setError(i.ctx.Err())
			return
		case <-flushTimer.C:
			timerArmed = false
			i.stateMu.Lock()
			var err error
			if !i.inPaste && len(i.buf) > 0 {
				out := i.buf
				i.buf = nil
				_, err = i.writeDest(out)
			}
			i.stateMu.Unlock()
			if err != nil && handleWriteErr(err) {
				return
			}
		case completion := <-i.completed:
			if err := drainAcceptedInput(); err != nil && handleWriteErr(err) {
				return
			}
			completionBuffer[completion.seq] = completion
			for {
				ready, ok := completionBuffer[nextCompletion]
				if !ok {
					break
				}
				delete(completionBuffer, nextCompletion)
				nextCompletion++
				_, writeErr := i.writeDest(ready.framed)
				if errors.Is(writeErr, tunnel.ErrWriteUncertain) {
					handleWriteErr(writeErr)
					writeErr = nil
				}
				if ready.err != nil {
					i.report(FileFailed)
					i.reportUploadFailure(ready.err, ready.uploaded, ready.closeFailed)
				} else if writeErr == nil {
					i.report(FileComplete)
				}
				i.pendingUploads.Add(-1)
				if writeErr != nil && handleWriteErr(writeErr) {
					return
				}
			}
			if inputClosed && i.pendingUploads.Load() == 0 {
				return
			}
		case p, ok := <-input:
			if !ok {
				inputClosed = true
				input = nil
				i.stateMu.Lock()
				err := i.flush()
				i.stateMu.Unlock()
				i.setError(err)
				if i.pendingUploads.Load() == 0 {
					return
				}
				continue
			}
			i.stateMu.Lock()
			i.buf = append(i.buf, p...)
			processed := int64(1)
			// Coalesce input that queued up while this worker was busy (e.g.
			// behind a slow destination write) so the backlog is forwarded in
			// one destination write instead of one write per chunk.
			batchInputClosed := false
		coalesce:
			for {
				select {
				case q, more := <-i.input:
					if !more {
						batchInputClosed = true
						break coalesce
					}
					i.buf = append(i.buf, q...)
					processed++
				default:
					break coalesce
				}
			}
			// These chunks are no longer queued once this worker owns them. In
			// particular, ordinary input must be able to take the direct path
			// while an upload started by this batch is still in flight.
			i.queued.Add(-processed)
			err := i.drain()
			if err == nil && batchInputClosed {
				err = i.flush()
			}
			i.stateMu.Unlock()
			if err != nil && handleWriteErr(err) {
				return
			}
			if batchInputClosed {
				inputClosed = true
				input = nil
			}
			if inputClosed && i.pendingUploads.Load() == 0 {
				return
			}
			rearmFlush()
		}
	}
}

func (i *Interceptor) writeDest(p []byte) (int, error) {
	i.destMu.Lock()
	defer i.destMu.Unlock()
	return i.dest.Write(p)
}

func (i *Interceptor) flush() error {
	if len(i.buf) == 0 && !i.inPaste {
		return nil
	}
	// Emit whatever remains: if mid-paste, re-add the start marker so framing
	// stays well-formed for the remote.
	var out []byte
	if i.inPaste {
		out = append(out, startMarker...)
	}
	out = append(out, i.buf...)
	i.buf = nil
	i.inPaste = false
	_, err := i.writeDest(out)
	return err
}

func (i *Interceptor) setError(err error) {
	if err == nil {
		return
	}
	i.errMu.Lock()
	if i.err == nil {
		i.err = err
		select {
		case i.errCh <- err:
		default:
		}
	}
	i.errMu.Unlock()
}

func (i *Interceptor) result() error {
	i.errMu.Lock()
	defer i.errMu.Unlock()
	return i.err
}

// drain processes as much of buf as possible without blocking on partial
// markers that may complete in a later Write.
func (i *Interceptor) drain() error {
	for {
		if !i.inPaste {
			done, err := i.drainNormal()
			if err != nil || !done {
				return err
			}
			continue
		}
		done, err := i.drainPaste()
		if err != nil || !done {
			return err
		}
	}
}

// drainNormal emits non-paste bytes up to the next start marker. It returns
// done=true when it consumed a full start marker (more work may remain).
func (i *Interceptor) drainNormal() (done bool, err error) {
	idx := bytes.Index(i.buf, startMarker)
	if idx < 0 {
		// No complete start marker. Flush everything except a suffix that could
		// be the beginning of a start marker split across Writes.
		keep := partialSuffix(i.buf, startMarker)
		flush := i.buf[:len(i.buf)-keep]
		if len(flush) > 0 {
			if _, err := i.writeDest(flush); err != nil {
				return false, err
			}
		}
		i.buf = append(i.buf[:0], i.buf[len(i.buf)-keep:]...)
		return false, nil
	}
	// Emit the normal bytes before the marker, then enter paste mode. The start
	// marker itself is consumed here and re-emitted when the paste is flushed.
	if idx > 0 {
		if _, err := i.writeDest(i.buf[:idx]); err != nil {
			return false, err
		}
	}
	i.buf = append(i.buf[:0], i.buf[idx+len(startMarker):]...)
	i.inPaste = true
	return true, nil
}

// drainPaste waits for a complete paste, then processes and emits it. It
// returns done=true when a full paste was handled.
func (i *Interceptor) drainPaste() (done bool, err error) {
	idx := bytes.Index(i.buf, endMarker)
	if idx < 0 {
		// Whole (possibly large) paste body not yet complete; wait for more.
		return false, nil
	}
	body := append([]byte(nil), i.buf[:idx]...)
	i.buf = append(i.buf[:0], i.buf[idx+len(endMarker):]...)
	i.inPaste = false

	out, pending := i.rewrite(body)
	if pending {
		return true, nil
	}
	framed := make([]byte, 0, len(startMarker)+len(out)+len(endMarker))
	framed = append(framed, startMarker...)
	framed = append(framed, out...)
	framed = append(framed, endMarker...)
	if _, err := i.writeDest(framed); err != nil {
		return false, err
	}
	return true, nil
}
