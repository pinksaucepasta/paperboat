//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/availability"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

type fixedAvailability struct{ observation *availability.Observation }

func (f fixedAvailability) Observation() *availability.Observation { return f.observation }

type fixedUpdater struct {
	response updated.ControlResponse
	err      error
}

func (f fixedUpdater) Status(context.Context) (updated.ControlResponse, error) {
	return f.response, f.err
}

// pb doctor reported every unix machine degraded because the root host service always said "unknown".
func TestUpdateHealthAvailabilityReportsTheUpdatersHealth(t *testing.T) {
	failed := func(failure updateflow.Failure) updated.ControlResponse {
		return updated.ControlResponse{Status: "ok", Transaction: workerupdate.TransactionState{Failure: failure}}
	}
	for _, tc := range []struct {
		name, reported, want string
		updater              fixedUpdater
	}{
		{"idle", "unknown", "healthy", fixedUpdater{response: updated.ControlResponse{Status: "ok"}}},
		{"activation in progress", "unknown", "healthy", fixedUpdater{response: updated.ControlResponse{Status: "ok", Pending: true, Transaction: workerupdate.TransactionState{CandidateVersion: "2026.11.09.0"}}}},
		{"candidate rejected after a clean rollback", "unknown", "healthy", fixedUpdater{response: updated.ControlResponse{Status: "ok", ActivationFailure: "health_failed", Transaction: workerupdate.TransactionState{Failure: updateflow.FailureHealth, Quarantined: true}}}},
		{"rollback failed", "unknown", "recovery_required", fixedUpdater{response: failed(updateflow.FailureRollback)}},
		{"contradictory journal", "unknown", "recovery_required", fixedUpdater{response: failed(updateflow.FailureContradictoryJournal)}},
		{"updater error status", "unknown", "recovery_required", fixedUpdater{response: updated.ControlResponse{Status: "error"}}},
		{"updater unreachable", "unknown", "recovery_required", fixedUpdater{err: updated.ErrUnavailable}},
		{"host service value kept", "healthy", "healthy", fixedUpdater{err: errors.New("must not be asked")}},
	} {
		source := fixedAvailability{&availability.Observation{UpdateHealth: tc.reported}}
		got := updateHealthAvailability{source, tc.updater}.Observation(context.Background())
		if got == nil || got.UpdateHealth != tc.want {
			t.Fatalf("%s: update health %+v, want %s", tc.name, got, tc.want)
		}
	}
	if got := (updateHealthAvailability{fixedAvailability{}, fixedUpdater{}}).Observation(context.Background()); got != nil {
		t.Fatalf("no availability observation yet, got %+v", got)
	}
}

type blockedUpdater struct{}

func (blockedUpdater) Status(ctx context.Context) (updated.ControlResponse, error) {
	<-ctx.Done()
	return updated.ControlResponse{}, ctx.Err()
}

// A cancelled send or shutdown waited up to 2 s on a wedged updater socket because the probe ignored the caller's context.
func TestUpdateHealthAvailabilityStopsWithTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := fixedAvailability{&availability.Observation{UpdateHealth: "unknown"}}
	started := time.Now()
	got := updateHealthAvailability{source, blockedUpdater{}}.Observation(ctx)
	if took := time.Since(started); took > 500*time.Millisecond || got == nil || got.UpdateHealth != "recovery_required" {
		t.Fatalf("returned %+v after %v, want recovery_required at once", got, took)
	}
}
