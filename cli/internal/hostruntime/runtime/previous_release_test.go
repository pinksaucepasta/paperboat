package runtime

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
)

// The update gate 2026.11.16.2 persisted mid-update (testdata from that release's gate, at the candidate step)
// reloads after an update, so an update in flight across the binary swap is not lost.
func TestReloadsUpdateGateWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-update-gate.json")
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "gate.json")
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	health := http.NewServeMux()
	registerHostLivenessAndDiagnostics(health, nil, nil, nil, nil, nil)
	if _, err := newStandaloneUpdateGate(standaloneUpdateGateConfig{MachineID: "machine_01", StatePath: statePath, Health: health, Workloads: func() hostdproto.WorkloadStatus { return hostdproto.WorkloadStatus{Generation: 1} }}); err != nil {
		t.Fatalf("previous release's update gate: %v", err)
	}
}
