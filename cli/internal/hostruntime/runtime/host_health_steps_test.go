//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
)

// The host's typed health (served at /diagnostics for pb doctor) is ready on a healthy host, names a stable
// component that failed to start with its cause and retry, and is ready again once the component recovers.
func TestHostHealthNamesAFailingComponentStepUntilItRecovers(t *testing.T) {
	tracker, err := NewHostHealthTracker()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(Config{Version: "test", HealthTracker: tracker, Components: []Component{{Capability: "worker_lifecycle", Required: true, Service: workerLifecycleService{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	if overall := r.TypedHealth().Overall; overall.Status != health.StatusReady {
		t.Fatalf("healthy host overall = %+v, want ready (unreported dimensions must not read as unknown)", overall)
	}
	r.ReportComponentStep("peer_transport", errors.New("peer registration refused by the server"), 30*time.Second)
	snapshot := r.TypedHealth()
	edge := snapshot.Dimensions.Get(health.DimensionEdge)
	if edge.Status != health.StatusDegraded || edge.Code != "start_failed" || !strings.Contains(edge.Summary, "peer_transport") ||
		!strings.Contains(edge.Summary, "registration refused") || edge.Retry != health.RetryScheduled || edge.NextRetryAt == nil {
		t.Fatalf("failing step = %+v", edge)
	}
	if snapshot.Overall.Status != health.StatusDegraded || snapshot.Overall.Dimension != health.DimensionEdge {
		t.Fatalf("overall while failing = %+v", snapshot.Overall)
	}
	r.ReportComponentStep("peer_transport", nil, 0)
	if snapshot = r.TypedHealth(); snapshot.Overall.Status != health.StatusReady || snapshot.Dimensions.Get(health.DimensionEdge).Status != health.StatusReady {
		t.Fatalf("after recovery = %+v", snapshot.Overall)
	}
}
