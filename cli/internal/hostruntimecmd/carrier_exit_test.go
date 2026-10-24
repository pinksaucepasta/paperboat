package hostruntimecmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/peeridentity"
)

func TestCarrierStopsRestartingWhenTheServerRejectsItsEnrollment(t *testing.T) {
	rejected := fmt.Errorf("check machine endpoint approval: %w", &peeridentity.StatusError{Status: 401})
	if got := carrierExitCode(errors.Join(errors.New("start carrier"), rejected)); got != EnrollmentRejectedExitCode {
		t.Fatalf("rejected enrollment exit %d, want %d", got, EnrollmentRejectedExitCode)
	}
	for _, err := range []error{&peeridentity.StatusError{Status: 503}, errors.New("other")} {
		if got := carrierExitCode(err); got != 1 {
			t.Fatalf("%v: exit %d, want 1 (restarted)", err, got)
		}
	}
}
