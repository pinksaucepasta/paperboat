// Package metering records machine presence and runtime observations: heartbeats from enrolled
// machines, and the wait on machine events that proves a machine is alive. The repository refreshes
// last-seen time only when it is older than a configured interval, and detects a second process
// claiming the same machine identity.
package metering

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

var ErrDuplicateMachineIdentity = errors.New("machine identity is active on another installation")

type RuntimeRepository struct {
	db                     *db.DB
	presenceRefresh        time.Duration
	identityConflictWindow time.Duration

	presenceMu      sync.Mutex
	presencePending map[string]struct{}
	// writePresence is the one statement a flush runs; replaced in tests.
	writePresence func(context.Context, []string) error
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
// held up to 55 s, so a healthy machine's row is never older than the gate plus a wait plus the flush delay.
const machineWaitPresenceGate = 25 * time.Second

// PresenceFlushEvery is how long a wait's presence touch may sit in memory before it is written. A wait is held
// 55 s, so one write per machine per wait became one statement a second for the whole fleet (commit and WAL sync
// per row were ~10% of Postgres busy time in production). The gate, a held wait, this delay and a client's first
// reconnect after a server crash (presenceReconnectBudget) stay under the offline cutoff
// (TestBatchedPresenceNeverApproachesTheOfflineCutoff).
const PresenceFlushEvery = time.Second

// presenceReconnectBudget is the longest first retry of a daemon's wait after the server dropped it: cli/internal/
// hostruntime/machineevents minBackoff (1 s) plus reconnectSpread (5 s).
const presenceReconnectBudget = 6 * time.Second

// TouchMachinePresence records that machineID's machine-events wait is alive: it queues the machine for the next
// FlushPresence (see TouchUserMachinesPresence for what the write does). A crash loses at most PresenceFlushEvery of
// touches; the machines' next waits repeat them.
func (r *RuntimeRepository) TouchMachinePresence(_ context.Context, machineID string) error {
	r.presenceMu.Lock()
	defer r.presenceMu.Unlock()
	if r.presencePending == nil {
		r.presencePending = map[string]struct{}{}
	}
	r.presencePending[machineID] = struct{}{}
	return nil
}

// FlushPresence writes every queued touch in one statement. On failure the machines stay queued for the next flush.
func (r *RuntimeRepository) FlushPresence(ctx context.Context) error {
	r.presenceMu.Lock()
	ids := make([]string, 0, len(r.presencePending))
	for id := range r.presencePending {
		ids = append(ids, id)
	}
	clear(r.presencePending)
	r.presenceMu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	write := r.writePresence
	if write == nil {
		write = func(ctx context.Context, ids []string) error {
			_, err := r.db.Queries().TouchUserMachinesPresence(ctx, dbsqlc.TouchUserMachinesPresenceParams{Ids: ids, GateSeconds: machineWaitPresenceGate.Seconds()})
			return err
		}
	}
	if err := write(ctx, ids); err != nil {
		r.presenceMu.Lock()
		defer r.presenceMu.Unlock()
		if r.presencePending == nil {
			r.presencePending = map[string]struct{}{}
		}
		for _, id := range ids {
			r.presencePending[id] = struct{}{}
		}
		return err
	}
	return nil
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
