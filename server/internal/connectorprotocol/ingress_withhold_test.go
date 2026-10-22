package connectorprotocol

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
)

// One route whose decision cannot be issued must not fail an edge's whole ingress snapshot (prod
// 2026-10-23: ~1 in 3 desired-state pulls answered 503 control_unavailable, so every route on the
// edge kept retrying). In an edge snapshot that route is withheld; elsewhere the projection fails.
func TestIngressRowFailureWithholdsOneRouteOnlyInAnEdgeSnapshot(t *testing.T) {
	ctx := context.Background()
	edge, daemon := SQLIngressSource{edge: true}, SQLIngressSource{}
	for _, err := range []error{wire.ErrIngressDenied, wire.ErrInvalidInput, accountingauthority.ErrUnavailable, errors.Join(accountingauthority.ErrUnavailable), sql.ErrNoRows} {
		if got := edge.rowFailed(ctx, "accounting", "route_01", err); got != nil {
			t.Fatalf("edge snapshot failed on a per-route %v: %v", err, got)
		}
		if got := daemon.rowFailed(ctx, "accounting", "route_01", err); !errors.Is(got, err) {
			t.Fatalf("daemon projection swallowed %v", err)
		}
	}
	outage := errors.New("database unavailable")
	if got := edge.rowFailed(ctx, "accounting", "route_01", outage); got != outage {
		t.Fatalf("edge snapshot withheld a route on an outage: %v", got)
	}
}
