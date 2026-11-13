package reportcause

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

func TestEdgeFailureProjection(t *testing.T) {
	reporter, err := reporting.New("paperboat-edge")
	if err != nil {
		t.Fatal(err)
	}
	const reference = "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	err = errors.Join(control.ErrControlUnavailable, errors.New("token=private"))
	fault := reporter.ObserveFailure(reporting.WithSupportReference(context.Background(), reference), "node_heartbeat", err)
	if fault.Component != "paperboat-edge" || fault.Name != "edge_node_heartbeat_failed" || fault.Stage != "node_heartbeat" ||
		fault.Cause != "control_unavailable" || fault.SupportReference != reference {
		t.Fatalf("fault=%+v", fault)
	}
}

func TestRequestFailureCarriesSupportReference(t *testing.T) {
	reporter, _ := reporting.New("paperboat-edge")
	const reference = "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	fault := reporter.ObserveFailure(context.Background(), "control_request", &control.RequestFailure{SupportReference: reference})
	if fault.SupportReference != reference || fault.Name != "edge_control_request_failed" {
		t.Fatalf("fault=%+v", fault)
	}
}
