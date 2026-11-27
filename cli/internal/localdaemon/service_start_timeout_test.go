package localdaemon

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
)

// The recorder drops an event the validator rejects; the doctor check depends on this one being kept.
func TestServiceStartTimeoutEventIsAccepted(t *testing.T) {
	if _, err := diagnostics.NewEvent(time.Now(), "local_daemon", ServiceStartTimeoutCode, "warning", serviceStartTimeoutFields()); err != nil {
		t.Fatalf("event rejected: %v", err)
	}
	recorder := diagnostics.NewMemoryRecorder()
	recordServiceStartTimeout(diagnostics.WithRecorder(context.Background(), recorder))
	recent := recorder.Recent()
	if len(recent) != 1 || recent[0].Code != ServiceStartTimeoutCode {
		t.Fatalf("recent = %+v, want one %s event", recent, ServiceStartTimeoutCode)
	}
}
