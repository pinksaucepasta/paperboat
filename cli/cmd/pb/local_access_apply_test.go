package main

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"testing"
	"time"
)

func TestLocalAccessReadinessWaitsForProtectedReconciliation(t *testing.T) {
	calls := 0
	client := localDaemonSnapshotClientFunc(func(context.Context) (localapi.Snapshot, error) {
		calls++
		state := "starting"
		if calls > 1 {
			state = "ready"
		}
		return localapi.Snapshot{DaemonState: state, DaemonVersion: buildinfo.Version}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitForLocalAccessReady(ctx, client); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatal("returned before protected reconciliation")
	}
	for _, state := range []string{"awaiting_enrollment", "degraded"} {
		client := localDaemonSnapshotClientFunc(func(context.Context) (localapi.Snapshot, error) {
			return localapi.Snapshot{DaemonState: state, DaemonVersion: buildinfo.Version, Health: []localapi.HealthItem{{Code: "control_plane_unavailable"}}}, nil
		})
		if err := waitForLocalAccessReady(ctx, client); err != nil {
			t.Fatalf("offline configuration %s: %v", state, err)
		}
	}
}
func TestLocalAccessReadinessRejectsRoutingFailureAndStaleDaemon(t *testing.T) {
	failed := localDaemonSnapshotClientFunc(func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{DaemonState: "degraded", DaemonVersion: buildinfo.Version, Health: []localapi.HealthItem{{Code: "local_access_unavailable"}}}, nil
	})
	if err := waitForLocalAccessReady(t.Context(), failed); err == nil {
		t.Fatal("routing failure reported applied")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	stale := localDaemonSnapshotClientFunc(func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{DaemonState: "ready", DaemonVersion: "stale"}, nil
	})
	if err := waitForLocalAccessReady(ctx, stale); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale daemon accepted: %v", err)
	}
}
