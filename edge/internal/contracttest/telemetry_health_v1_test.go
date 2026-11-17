package contracttest

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	sharedcontract "github.com/pinksaucepasta/paperboat/shared/telemetry/contracttest"
)

func TestTelemetryHealthV1EdgeSnapshotContract(t *testing.T) {
	sharedcontract.VerifyHealthV1(t, telemetry.NewHealthTracker)
}
