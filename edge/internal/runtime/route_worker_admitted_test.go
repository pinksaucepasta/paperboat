package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

type controlDownRoutes struct{}

func (controlDownRoutes) DesiredRoutes(context.Context, string) ([]control.RouteAssignment, error) {
	return nil, errors.New("control down")
}

func (controlDownRoutes) DesiredRouteSnapshot(context.Context, string, string) (control.RouteSnapshot, error) {
	return control.RouteSnapshot{}, errors.New("control down")
}

// A replacement edge process is not warm until its first route snapshot is applied: until then it knows none of the
// durable tunnel carriers and refuses a known machine's carrier as unknown_machine (gate 11-12: 10 refusals over 20 s).
// A failed first fetch leaves Admitted open; a fetched snapshot closes it.
func TestRouteWorkerIsAdmittedOnlyAfterItsFirstSnapshotIsApplied(t *testing.T) {
	worker := &RouteWorker{Registry: route.NewRegistry("", ""), Source: controlDownRoutes{}, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"}
	if err := worker.reconcile(context.Background()); err == nil {
		t.Fatal("a failed fetch reconciled")
	}
	select {
	case <-worker.Admitted():
		t.Fatal("Admitted closed although no route snapshot was applied")
	default:
	}
	worker.Source = &workerSnapshotSource{snapshot: control.RouteSnapshot{Complete: true}}
	if err := worker.reconcile(context.Background()); err != nil {
		t.Fatalf("empty snapshot: %v", err)
	}
	select {
	case <-worker.Admitted():
	default:
		t.Fatal("Admitted still open after the first snapshot was applied")
	}
}
