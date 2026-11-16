package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// A daemon that is not listening is an operational failure with the "unreachable" wording, not an unexpected one: every
// local API request (not only streams) reports a dial failure joined with ErrTransportUnavailable. (A missing socket is
// ENOENT, which the classifier already treated as unexpected before; it is deliberately unchanged.)
func TestADaemonThatIsNotListeningStaysOperational(t *testing.T) {
	for name, cause := range map[string]error{
		"refused socket": &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
	} {
		err := fmt.Errorf("Get \"http://paperboat.local/v1/snapshot\": %w", errors.Join(localapi.ErrTransportUnavailable, cause))
		if kind := classifyCommandFailure(err).kind; kind != commandOperational {
			t.Fatalf("%s: kind = %v, want commandOperational (%v)", name, kind, commandOperational)
		}
	}
}

// A dial on a cancelled context fails with net.canceledError (wrapped by OpError); it is a cancel (exit 130), not a
// generic operational failure, and an initial connect must not retry it.
func TestACancelledDialIsACancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:1")
	if dialErr == nil {
		t.Fatal("dial on a cancelled context succeeded")
	}
	if kind := classifyCommandFailure(fmt.Errorf("connect: %w", dialErr)).kind; kind != commandCanceled {
		t.Fatalf("kind = %v, want commandCanceled (%v) for %v", kind, commandCanceled, dialErr)
	}
}
