package contracttest

import (
	"testing"

	runtimehealth "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/health"
	sharedcontract "github.com/pinksaucepasta/paperboat/shared/telemetry/contracttest"
)

func TestTelemetryHealthV1HostSnapshotContract(t *testing.T) {
	sharedcontract.VerifyHealthV1(t, runtimehealth.NewHealthTracker)
}
