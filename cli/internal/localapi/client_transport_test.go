package localapi

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
)

func TestLocalDialFailurePreservesCancellationAndPermission(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, os.ErrPermission} {
		err := &net.OpError{Op: "dial", Net: "unix", Err: cause}
		got := localDialFailure(err)
		if !errors.Is(got, cause) || errors.Is(got, ErrTransportUnavailable) {
			t.Fatalf("nonretryable error classification: %v", got)
		}
	}
	for _, cause := range []error{os.ErrNotExist, net.ErrClosed} {
		if got := localDialFailure(cause); !errors.Is(got, cause) || !errors.Is(got, ErrTransportUnavailable) {
			t.Fatalf("transport error classification: %v", got)
		}
	}
}
