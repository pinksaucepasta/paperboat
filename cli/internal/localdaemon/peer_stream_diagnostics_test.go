package localdaemon

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type privateBridgeFailure struct{}

func (privateBridgeFailure) Error() string { panic("private terminal data must not be formatted") }
func (privateBridgeFailure) Unwrap() error { return syscall.EIO }

func TestLocalPeerBridgeOwnsUnexpectedFailureAndPreservesReference(t *testing.T) {
	ctx := supportref.WithContext(t.Context(), supportref.New())
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	for _, err := range []error{nil, io.EOF, context.Canceled, net.ErrClosed, errors.Join(io.EOF, context.Canceled)} {
		reportLocalPeerBridgeFailure(ctx, err)
	}
	if len(faults) != 0 {
		t.Fatal("normal stream shutdown reported as failure")
	}
	reportLocalPeerBridgeFailure(ctx, errors.Join(context.Canceled, privateBridgeFailure{}))
	if len(faults) != 1 || faults[0].Code != "terminal_session_failed" || faults[0].Stage != "delivery" || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != supportref.FromContext(ctx) {
		t.Fatalf("bridge faults=%+v", faults)
	}
}
