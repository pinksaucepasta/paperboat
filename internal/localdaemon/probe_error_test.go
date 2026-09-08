package localdaemon

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/connectionmanager"
	"testing"
)

func TestProbeAuthorityFailuresRemainTerminalAcrossIPC(t *testing.T) {
	for class := connectionmanager.FailureAuthentication; class <= connectionmanager.FailureGeneration; class++ {
		err := localProbeError(&connectionmanager.Failure{Class: class})
		if !errors.Is(err, localapi.ErrPermission) {
			t.Fatalf("class %d lost authority denial", class)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, &connectionmanager.Failure{Class: connectionmanager.FailureReachability}} {
		if errors.Is(localProbeError(err), localapi.ErrPermission) {
			t.Fatal("transient failure classified as authorization denial")
		}
	}
}
