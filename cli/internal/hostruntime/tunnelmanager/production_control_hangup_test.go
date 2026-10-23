package tunnelmanager

import (
	"bytes"
	"errors"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

// During shutdown the control plane hanging up is the expected end of the stream. Whether the
// cancellation or the EOF reaches the control loop first is timing (about 1 run in 150 under CPU
// load), so Shutdown must not report the EOF as a cleanup failure; a real I/O error is still kept.
func TestShutdownDoesNotReportTheControlPlaneHangingUp(t *testing.T) {
	_, hangUp := connectorprotocol.ReadFrame(bytes.NewReader(nil))
	if hangUp == nil {
		t.Fatal("reading a closed stream succeeded")
	}
	if got := unexpectedControlFailure(hangUp); got != nil {
		t.Fatalf("hang-up kept as a shutdown failure: %v", got)
	}
	if got := unexpectedControlFailure(syscall.EIO); !errors.Is(got, syscall.EIO) {
		t.Fatalf("a real I/O failure was dropped: %v", got)
	}
}
