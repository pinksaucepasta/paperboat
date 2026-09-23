package localdaemon

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"reflect"
	"testing"
	"time"
)

func TestInventoryDiagnosticsContainOnlySafeStageAndCategory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"API", &api.APIError{Status: 429, Code: "secret-code", Message: "secret-token", Details: map[string]any{"body": "secret-payload"}}, "http_429"},
		{"deadline", context.DeadlineExceeded, "deadline"},
		{"auth", api.ErrUnauthenticated, "unauthenticated"},
		{"other", errors.New("secret-payload"), "source_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inventorySourceFailure("peer_approval", tc.err)
			if !errors.Is(err, tc.err) {
				t.Fatal("lost original error identity")
			}
			severity, fields := inventoryRefreshDiagnostic(err)
			want := map[string]string{"outcome": "degraded", "phase": "peer_approval", "reason": tc.reason}
			if severity != "warning" || !reflect.DeepEqual(fields, want) {
				t.Fatalf("diagnostic=%s %v", severity, fields)
			}
			if _, err := diagnostics.NewEvent(time.Now().UTC(), "reconciliation", "inventory_refresh", severity, fields); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestInventoryReportsEachFailureAndRecovery(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{err: context.DeadlineExceeded}, {err: api.ErrUnauthenticated}, {}}}
	var observed []error
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, OnRefresh: func(err error) { observed = append(observed, err) }})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = inventory.Refresh(context.Background())
	}
	if len(observed) != 3 || !errors.Is(observed[0], context.DeadlineExceeded) || !errors.Is(observed[1], api.ErrUnauthenticated) || observed[2] != nil {
		t.Fatal("failure/recovery callbacks missing")
	}
}
