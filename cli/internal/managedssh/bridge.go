package managedssh

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	ErrSSHTargetInvalid     = errors.New("managed SSH target is invalid")
	ErrSSHTargetUnavailable = errors.New("managed SSH loopback target is unavailable")
)

type LoopbackTarget struct {
	Host string
	Port uint16
}

type SSHReadiness struct {
	IPv4   bool
	IPv6   bool
	Target LoopbackTarget
}

type BridgeResult struct {
	ToSSHD   int64
	FromSSHD int64
}

func NewLoopbackTarget(host string, port uint16) (LoopbackTarget, error) {
	if port == 0 || host != "127.0.0.1" && host != "::1" {
		return LoopbackTarget{}, ErrSSHTargetInvalid
	}
	return LoopbackTarget{Host: host, Port: port}, nil
}

func ProbeLoopbackSSH(ctx context.Context, port uint16, timeout time.Duration) (SSHReadiness, error) {
	if ctx == nil || port == 0 || timeout <= 0 || timeout > 30*time.Second {
		return SSHReadiness{}, ErrSSHTargetInvalid
	}
	type probe struct {
		host string
		err  error
	}
	results := make(chan probe, 2)
	for _, host := range []string{"127.0.0.1", "::1"} {
		go func(host string) {
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
			if err == nil {
				err = connection.Close()
			}
			results <- probe{host: host, err: err}
		}(host)
	}
	var readiness SSHReadiness
	probeFailures := make([]error, 0, 2)
	for range 2 {
		result := <-results
		if result.err != nil {
			probeFailures = append(probeFailures, result.err)
			continue
		}
		if result.host == "127.0.0.1" {
			readiness.IPv4 = true
		} else {
			readiness.IPv6 = true
		}
	}
	selected := ""
	if readiness.IPv4 {
		selected = "127.0.0.1"
	} else if readiness.IPv6 {
		selected = "::1"
	}
	if selected == "" {
		if ctx.Err() != nil {
			return readiness, managedSSHContextError(ctx)
		}
		return readiness, managedSSHFailure("target_connect", ErrSSHTargetUnavailable, errors.Join(probeFailures...))
	}
	readiness.Target = LoopbackTarget{Host: selected, Port: port}
	return readiness, nil
}

func BridgeSSH(ctx context.Context, stream io.ReadWriteCloser, target LoopbackTarget, dialTimeout time.Duration) (BridgeResult, error) {
	if ctx == nil || stream == nil || dialTimeout <= 0 || dialTimeout > 30*time.Second {
		return BridgeResult{}, ErrSSHTargetInvalid
	}
	if _, err := NewLoopbackTarget(target.Host, target.Port); err != nil {
		return BridgeResult{}, err
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	sshd, err := dialSSHD(dialCtx, net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))))
	cancelDial()
	if err != nil {
		if ctx.Err() != nil {
			return BridgeResult{}, managedSSHContextError(ctx)
		}
		return BridgeResult{}, managedSSHFailure("target_connect", ErrSSHTargetUnavailable, err)
	}
	var closeOnce sync.Once
	var closeErr error
	// sshdClosed is set only after sshd.Close() has returned, and before the stream is closed: the to_sshd copier
	// wakes up on that stream close, so by then a CloseWrite failure on the closed sshd socket is our own doing.
	var sshdClosed atomic.Bool
	finish := func() error {
		closeOnce.Do(func() {
			sshdErr := sshd.Close()
			sshdClosed.Store(true)
			closeErr = errors.Join(sshdErr, stream.Close())
		})
		return closeErr
	}
	stop := context.AfterFunc(ctx, func() {
		_ = finish()
	})
	defer stop()
	defer finish()
	type copyResult struct {
		direction string
		bytes     int64
		err       error
	}
	results := make(chan copyResult, 2)
	// sshdDone is set once sshd's output is drained and the bridge closes both sides itself.
	var sshdDone atomic.Bool
	// clientEOF / sshdEOF close when that direction's copy ended with a plain EOF: the peer finished sending. A reset
	// read from sshd counts as sshd's EOF (sshdReadReset).
	clientEOF, sshdEOF := make(chan struct{}), make(chan struct{})
	go func() {
		count, copyErr := io.Copy(sshd, stream)
		if copyErr == nil {
			close(clientEOF)
		}
		if sshdDone.Load() {
			copyErr = nil // the read failed because the bridge closed the stream after sshd's end
		}
		copyErr = normalizeBridgeError(copyErr)
		if half, ok := sshd.(interface{ CloseWrite() error }); ok {
			closedBefore := sshdClosed.Load() // read before the call: a close that races it is not assumed ours
			copyErr = errors.Join(copyErr, closeWriteFailure(half.CloseWrite(), closedBefore))
		}
		copyErr = excuseReset(ctx, copyErr, sshdEOF)
		if copyErr != nil {
			_ = finish()
		}
		results <- copyResult{direction: "to_sshd", bytes: count, err: labelStep(copyErr, func(err error) error { return &toSSHDError{err} })}
	}()
	go func() {
		// onlyWriter keeps the stream's own ReadFrom out of it, so a failed read from sshd is reported as that read
		// (Op "read", what sshdReadReset looks for) and not folded into one "readfrom" error with the write.
		count, copyErr := io.Copy(onlyWriter{stream}, sshd)
		// Windows OpenSSH ends a connection with an RST where a Unix sshd sends a FIN: a reset READ from sshd is its
		// end of output, recorded exactly like the Unix EOF. A reset while writing to the client, or anything else,
		// stays an error.
		if sshdReadReset(copyErr) {
			copyErr = nil
		}
		if copyErr == nil {
			close(sshdEOF)
		}
		copyErr = normalizeBridgeError(copyErr)
		if half, ok := stream.(interface{ CloseWrite() error }); ok {
			closeErr := normalizeBridgeError(half.CloseWrite())
			if copyErr != nil {
				copyErr = errors.Join(copyErr, closeErr)
			} else {
				// After sshd's end the client may already have torn its side down, which is its normal exit: that
				// shows as its stream ending. A failed FIN to a client that never left is a failure.
				copyErr = waitClientLeft(ctx, closeErr, clientEOF)
			}
		}
		// Target EOF is terminal for an SSH connection. Some OpenSSH clients
		// keep ProxyCommand input open until the proxy exits, so waiting for the
		// opposite copier here deadlocks after a remote command has completed.
		// Drain target output, publish its FIN, then close both read sides to
		// unblock the client-to-target copier.
		if copyErr == nil {
			sshdDone.Store(true)
		}
		_ = finish()
		results <- copyResult{direction: "from_sshd", bytes: count, err: labelStep(copyErr, func(err error) error { return &fromSSHDError{err} })}
	}()
	var result BridgeResult
	var resultErr error
	for range 2 {
		copied := <-results
		if copied.direction == "to_sshd" {
			result.ToSSHD = copied.bytes
		} else {
			result.FromSSHD = copied.bytes
		}
		resultErr = errors.Join(resultErr, copied.err)
	}
	cleanupErr := normalizeBridgeError(finish())
	if ctx.Err() != nil {
		if resultErr == nil && cleanupErr == nil || managedSSHLeavesMatch(errors.Join(resultErr, cleanupErr), io.EOF, io.ErrClosedPipe, net.ErrClosed, context.Canceled) {
			return result, managedSSHContextError(ctx)
		}
		return result, managedSSHBoundary("command", errors.Join(ctx.Err(), context.Cause(ctx), resultErr, cleanupErr))
	}
	return result, managedSSHBoundary("command", errors.Join(resultErr, cleanupErr))
}

// toSSHDError and fromSSHDError name the direction an error came from. The diagnostics record error types only (the
// texts of network errors are redacted), so without them a failed Windows session shows leaves but not which
// copier produced them. They wrap, never replace: leaf matching sees through them.
type toSSHDError struct{ err error }
type fromSSHDError struct{ err error }

func (e *toSSHDError) Error() string   { return "client to sshd: " + e.err.Error() }
func (e *toSSHDError) Unwrap() error   { return e.err }
func (e *fromSSHDError) Error() string { return "sshd to client: " + e.err.Error() }
func (e *fromSSHDError) Unwrap() error { return e.err }

func labelStep(err error, wrap func(error) error) error {
	if err == nil {
		return nil
	}
	return wrap(err)
}

// closeWriteFailure is what a failed CloseWrite (the FIN to sshd) counts as. It is ignored only when the bridge had
// already closed the sshd socket before the call and the error is what a closed socket reports (resultOfOwnClose):
// the session is over and the FIN is moot. Any other failure, or one with sshd still open, means the FIN may never
// have landed and is kept.
func closeWriteFailure(err error, closedBefore bool) error {
	if closedBefore && resultOfOwnClose(err) {
		return nil
	}
	return err
}

// resultOfOwnClose reports an error that a closed socket gives and nothing else: net.ErrClosed, or the net.OpError
// Go wraps around a plain closed-socket value (on Windows a fresh error, not net.ErrClosed, so identity cannot name
// it). An operating-system error inside it (refused, reset, aborted) is a real network failure.
func resultOfOwnClose(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "close" {
		return false
	}
	var errno syscall.Errno
	var system *os.SyscallError
	return !errors.As(err, &errno) && !errors.As(err, &system)
}

// waitClientLeft keeps err (a failed CloseWrite towards the client) only if the client does not show its EOF.
func waitClientLeft(ctx context.Context, err error, clientLeft <-chan struct{}) error {
	if err == nil {
		return nil
	}
	timer := time.NewTimer(resetGrace())
	defer timer.Stop()
	select {
	case <-clientLeft:
		return nil
	case <-timer.C:
	case <-ctx.Done():
	}
	return err
}

func normalizeBridgeError(err error) error {
	if err == nil || managedSSHLeavesMatch(err, io.EOF, io.ErrClosedPipe, net.ErrClosed) {
		return nil
	}
	return err
}

type onlyWriter struct{ io.Writer }

// sshdReadReset reports a connection reset read from sshd (Op "read" with only reset leaves): on Windows the way a
// sshd connection ends, where a Unix sshd ends with EOF. closeResets is empty on Unix, so this never fires there.
func sshdReadReset(err error) bool {
	if err == nil || len(resetErrors()) == 0 || !managedSSHLeavesMatch(err, resetErrors()...) {
		return false
	}
	// Go's WriteTo wraps the failed read ("writeto") around the read's own OpError: look for the read in the chain.
	for e := err; e != nil; e = errors.Unwrap(e) {
		if op, ok := e.(*net.OpError); ok && op.Op == "read" {
			return true
		}
	}
	return false
}

// excuseReset forgives a connection reset while writing to sshd (the client kept sending as sshd's end came): sshd's
// own output must have ended too, within resetGrace, which sshdReadReset records as its EOF. A reset with sshd still
// sending is a failure.
func excuseReset(ctx context.Context, err error, otherEOF <-chan struct{}) error {
	if err == nil || len(resetErrors()) == 0 || !managedSSHLeavesMatch(err, resetErrors()...) {
		return err
	}
	timer := time.NewTimer(resetGrace())
	defer timer.Stop()
	select {
	case <-otherEOF:
		return nil
	case <-timer.C:
		err = errors.Join(err, &resetNotExcusedError{reason: "the other direction did not end in time"})
	case <-ctx.Done():
	}
	return err
}

// resetNotExcusedError is added to a connection reset that was not excused, so the diagnostics show that the rule ran
// and why it did not excuse the reset (the other direction never ended within resetGrace).
type resetNotExcusedError struct{ reason string }

func (e *resetNotExcusedError) Error() string { return "connection reset not excused: " + e.reason }
