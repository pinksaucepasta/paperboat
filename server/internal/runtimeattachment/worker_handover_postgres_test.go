package runtimeattachment

import (
	"database/sql"
	"encoding/base64"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// During pb update the next worker reports its generation before its attach replaces the old carrier,
// and the old worker keeps reporting until it stops. Through that overlap the machine's recorded worker
// generation only moves forward, and the old carrier keeps the machine's route current (migration 229
// made the carrier valid, but runtime_carrier_current still required the exact generation, so every
// helper route lookup found nothing until the attach).
func TestWorkerHandoverKeepsTheRouteAndTheGenerationPostgres(t *testing.T) {
	f := newAgentFixture(t, "handover")
	a, err := f.service.Attach(f.ctx, f.proof, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.Observe(f.ctx, f.edge, []runtimewire.Observation{{RouteID: f.route, SessionID: a.Binding.SessionID, AttachmentGeneration: a.AttachmentGeneration, Ready: true}}); err != nil {
		t.Fatal(err)
	}
	report := func(generation int64, at time.Time) {
		t.Helper()
		if _, err := f.database.Queries().RecordUserMachineRuntimeDiagnostics(f.ctx, dbsqlc.RecordUserMachineRuntimeDiagnosticsParams{ID: f.machine, EnvironmentID: f.env, WorkerGeneration: generation, OsBootID: sql.NullString{String: "boot-handover", Valid: true}, WorkerServiceScope: "user", ConnectorState: "ready", ObservedAt: sql.NullTime{Time: at, Valid: true}}); err != nil {
			t.Fatalf("report generation %d: %v", generation, err)
		}
	}
	now := time.Now().UTC()
	report(1, now)
	report(2, now.Add(time.Second)) // the next worker registers
	current := func() bool {
		t.Helper()
		var ok bool
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, f.route).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !current() {
		t.Fatal("next worker registered: the old worker's ready carrier no longer keeps the route current")
	}
	if _, err := f.database.Queries().GetActiveHelperRouteForMachine(f.ctx, dbsqlc.GetActiveHelperRouteForMachineParams{MachineID: f.machine, AccountID: f.account}); err != nil {
		t.Fatalf("next worker registered: helper route lookup: %v", err)
	}
	// The old worker renewing its attach in the overlap is told its binding is stale (409), not a 503.
	if _, err := f.service.Attach(f.ctx, f.proof, f.request); err != ErrStale {
		t.Fatalf("old worker's attach after the next worker registered: %v, want ErrStale", err)
	}
	report(1, now.Add(2*time.Second)) // the old worker's last heartbeat
	var generation int64
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT worker_generation FROM user_machines WHERE id=$1`, f.machine).Scan(&generation); err != nil || generation != 2 {
		t.Fatalf("old worker's heartbeat after the next worker's: worker_generation=%d %v, want 2", generation, err)
	}
	// A new identity (a fresh install) starts its workers again at generation 1, so the bind resets it.
	rebound, err := f.database.Queries().BindCanonicalMachineIdentity(f.ctx, dbsqlc.BindCanonicalMachineIdentityParams{PublicIdentityKey: sql.NullString{String: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Valid: true}, Now: time.Now().UTC(), EnvironmentID: f.env})
	if err != nil || rebound.WorkerGeneration != 0 {
		t.Fatalf("new identity: worker_generation=%d %v, want 0", rebound.WorkerGeneration, err)
	}
	report(1, now.Add(3*time.Second))
}
