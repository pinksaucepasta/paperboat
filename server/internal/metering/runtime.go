// Package metering records machine presence and runtime observations: heartbeats from enrolled
// machines, and the wait on machine events that proves a machine is alive. The repository refreshes
// last-seen time only when it is older than a configured interval, and detects a second process
// claiming the same machine identity.
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

// machineWaitPresenceGate is how old last_seen_at may get before a held machine-events wait refreshes it. A wait is
// held up to 55 s, so a healthy machine's row is never older than that plus its reconnect gap, well inside the
// offline cutoff (90 s).
const machineWaitPresenceGate = 30 * time.Second

// TouchMachinePresence records that machineID's machine-events wait is alive (see TouchUserMachinePresence).
func (r *RuntimeRepository) TouchMachinePresence(ctx context.Context, machineID string) error {
	_, err := r.db.Queries().TouchUserMachinePresence(ctx, dbsqlc.TouchUserMachinePresenceParams{ID: machineID, GateSeconds: machineWaitPresenceGate.Seconds()})
	return err
}

type RuntimeObservation struct {
	EnvironmentID         string
	MachineID             string
	ObservedAt            time.Time
	ReporterVersion       string
	ReporterSHA256        string
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
	// Read committed plus the machine row lock taken first: every later statement reads and writes a row no one
	// else can change until commit, which is all this transaction needs. Under SERIALIZABLE each heartbeat (and the
	// triggers its writes fire) took predicate locks; ~300 daemons reconnecting after a deploy exhausted the shared
	// predicate lock table (SQLSTATE 53200, max_pred_locks_per_transaction) and failed every serializable caller.
	return r.db.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		instance, err := tx.Queries().GetUserMachineRuntimeInstanceForUpdate(ctx, dbsqlc.GetUserMachineRuntimeInstanceForUpdateParams{ID: observation.MachineID, EnvironmentID: observation.EnvironmentID})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if observation.WorkerGeneration > 0 && observation.OSBootID != "" {
			if err == nil && instance.Online && instance.LastSeenAt.Valid && instance.LastSeenAt.Time.After(time.Now().UTC().Add(-r.identityConflictWindow)) && instance.OsBootID.Valid && instance.OsBootID.String != observation.OSBootID && observation.WorkerGeneration <= uint64(instance.WorkerGeneration) {
				return ErrDuplicateMachineIdentity
			}
		}
		refresh := r.presenceRefresh.Seconds()
		eligible, err := tx.Queries().MarkUserMachineOnlineFromHelper(ctx, dbsqlc.MarkUserMachineOnlineFromHelperParams{ID: observation.MachineID, EnvironmentID: observation.EnvironmentID, ObservedCapabilities: observation.Capabilities, ReporterVersion: observation.ReporterVersion, ReporterSha256: observation.ReporterSHA256, PresenceRefreshSeconds: refresh})
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
