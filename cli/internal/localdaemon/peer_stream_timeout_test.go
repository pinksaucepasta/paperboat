package localdaemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// On a 30% loss link a terminal stream open hung ~110 s (ltc-j1). It is now aborted after the
// per-attempt bound and reported as a retryable transport failure, so pb connect dials again.
func TestNonTerminalStreamOpenIsBoundedAndRetryable(t *testing.T) {
	ctx, abort := context.WithCancel(context.Background())
	started := time.Now()
	_, err := dialWithin(20*time.Millisecond, "ssh", abort, func() (tunnel.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var retryable interface{ TransportRetryable() bool }
	if !errors.As(err, &retryable) || !retryable.TransportRetryable() || !errors.Is(err, context.Canceled) || time.Since(started) > 5*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(started))
	}
	if _, err := dialWithin(time.Minute, "ssh", func() { t.Fatal("aborted a quick dial") }, func() (tunnel.Conn, error) { return nil, errors.New("refused") }); err == nil || errors.As(err, &retryable) {
		t.Fatalf("quick failure changed: %v", err)
	}
	execCtx, execAbort := context.WithCancel(context.Background())
	_, err = dialWithin(20*time.Millisecond, "exec", execAbort, func() (tunnel.Conn, error) { <-execCtx.Done(); return nil, execCtx.Err() })
	var coded interface{ LocalAPICode() string }
	if !errors.As(err, &coded) || coded.LocalAPICode() != "exec_start_uncertain" {
		t.Fatalf("a timed-out exec open must be start-uncertain: %v", err)
	}
	// Timed out before the start request was sent (no meow ack): nothing started, a plain transport failure.
	execCtx, execAbort = context.WithCancel(context.Background())
	_, err = dialWithin(20*time.Millisecond, "exec", execAbort, func() (tunnel.Conn, error) {
		<-execCtx.Done()
		return nil, &tunnel.ExecNotStartedError{Cause: execCtx.Err()}
	})
	if errors.As(err, &coded) && coded.LocalAPICode() != "" || !errors.As(err, &retryable) || !retryable.TransportRetryable() {
		t.Fatalf("an exec open that never sent its request must be a retryable transport failure, not uncertain: %v", err)
	}
}

// Terminal setup already has the staged credential/caller deadline and native
// one-second handshake retries. A second watchdog must not cut that loop off.
func TestTerminalSetupPreservesTheRealDialOutcome(t *testing.T) {
	ctx, abort := context.WithCancel(t.Context())
	defer abort()
	realFailure := errors.New("native rejected")
	_, err := dialWithin(20*time.Millisecond, "terminal", abort, func() (tunnel.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return nil, realFailure
		}
	})
	if err != realFailure {
		t.Fatalf("watchdog replaced real dial outcome: %v", err)
	}
}
