package metering

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

var ErrDuplicateMachineIdentity = errors.New("machine identity is active on another installation")

type RuntimeRepository struct {
	db                     *db.DB
	presenceRefresh        time.Duration
	identityConflictWindow time.Duration
}

// NewRuntimeRepository records machine heartbeats. presenceRefresh is how stale last_seen_at may become
// before an otherwise unchanged heartbeat writes it again (half the offline cutoff, so an idle machine
// costs one row write per presenceRefresh instead of one per heartbeat).
func NewRuntimeRepository(store *db.DB, presenceRefresh time.Duration, identityConflictWindow ...time.Duration) *RuntimeRepository {
	window := 2 * time.Minute
	if len(identityConflictWindow) > 0 && identityConflictWindow[0] > 0 {
		window = identityConflictWindow[0]
	}
	return &RuntimeRepository{db: store, presenceRefresh: presenceRefresh, identityConflictWindow: window}
}

type RuntimeObservation struct {
	EnvironmentID         string
	MachineID             string
	ObservedAt            time.Time
	ReporterVersion       string
	WorkerGeneration      uint64
	OSBootID              string
	WorkerServiceScope    string
	ConnectorState        string
	ConnectorGeneration   uint64
	DiagnosticsObservedAt time.Time
	Capabilities          []string
}

func (r *RuntimeRepository) RecordRuntimeObservation(ctx context.Context, observation RuntimeObservation) error {
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now().UTC()
	}
	return r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if observation.WorkerGeneration > 0 && observation.OSBootID != "" {
			instance, err := tx.Queries().GetUserMachineRuntimeInstanceForUpdate(ctx, dbsqlc.GetUserMachineRuntimeInstanceForUpdateParams{ID: observation.MachineID, EnvironmentID: observation.EnvironmentID})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && instance.Online && instance.LastSeenAt.Valid && instance.LastSeenAt.Time.After(time.Now().UTC().Add(-r.identityConflictWindow)) && instance.OsBootID.Valid && instance.OsBootID.String != observation.OSBootID && observation.WorkerGeneration <= uint64(instance.WorkerGeneration) {
				return ErrDuplicateMachineIdentity
			}
		}
		refresh := r.presenceRefresh.Seconds()
		eligible, err := tx.Queries().MarkUserMachineOnlineFromHelper(ctx, dbsqlc.MarkUserMachineOnlineFromHelperParams{ID: observation.MachineID, EnvironmentID: observation.EnvironmentID, ObservedCapabilities: observation.Capabilities, ReporterVersion: observation.ReporterVersion, PresenceRefreshSeconds: refresh})
		if err != nil || !eligible {
			return err
		}
		if _, err = tx.Queries().MarkPairingInstallationReady(ctx, sql.NullString{String: observation.MachineID, Valid: true}); err != nil {
			return err
		}
		if observation.WorkerGeneration > 0 {
			if observation.DiagnosticsObservedAt.IsZero() {
				observation.DiagnosticsObservedAt = observation.ObservedAt
			}
			if _, err = tx.Queries().RecordUserMachineRuntimeDiagnostics(ctx, dbsqlc.RecordUserMachineRuntimeDiagnosticsParams{ID: observation.MachineID, EnvironmentID: observation.EnvironmentID, WorkerGeneration: int64(observation.WorkerGeneration), OsBootID: sql.NullString{String: observation.OSBootID, Valid: true}, WorkerServiceScope: observation.WorkerServiceScope, ConnectorState: observation.ConnectorState, ConnectorGeneration: int64(observation.ConnectorGeneration), ObservedAt: sql.NullTime{Time: observation.DiagnosticsObservedAt, Valid: true}}); err != nil {
				return err
			}
		}
		return nil
	})
}
