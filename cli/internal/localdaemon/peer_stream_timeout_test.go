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
func TestTerminalStreamOpenIsBoundedAndRetryable(t *testing.T) {
	ctx, abort := context.WithCancel(context.Background())
	started := time.Now()
	_, err := dialWithin(20*time.Millisecond, abort, func() (tunnel.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var retryable interface{ TransportRetryable() bool }
	if !errors.As(err, &retryable) || !retryable.TransportRetryable() || !errors.Is(err, context.Canceled) || time.Since(started) > 5*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(started))
	}
	if _, err := dialWithin(time.Minute, func() { t.Fatal("aborted a quick dial") }, func() (tunnel.Conn, error) { return nil, errors.New("refused") }); err == nil || errors.As(err, &retryable) {
		t.Fatalf("quick failure changed: %v", err)
	}
}
