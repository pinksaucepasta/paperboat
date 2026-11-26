package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// An operator drain never fences the relay: its next heartbeat (the process has not applied the drain yet) is accepted
// and the lease carries the drain with the operator's deadline, so the relay drains itself with the whole grace. A
// restart inside the drain is admitted and comes back draining, not ready (it looped on 409 until undrain before).
// The drain outlives its deadline until resume; after it the relay starts ready again.
func TestRelayOperatorDrainReachesRelayThroughLeasePostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_opdrain")
	ctx := t.Context()
	deadline := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	n.exec(ctx, `UPDATE control_tunnel_nodes SET state='draining',ready=false,drain_deadline=$2 WHERE id=$1`, n.id, deadline)
	state := func() (string, bool, *time.Time) {
		t.Helper()
		var s string
		var ready bool
		var d *time.Time
		if err := n.store.SQL().QueryRowContext(ctx, `SELECT state,ready,drain_deadline FROM control_tunnel_nodes WHERE id=$1`, n.id).Scan(&s, &ready, &d); err != nil {
			t.Fatal(err)
		}
		return s, ready, d
	}

	// 1. Drain, then a heartbeat that does not know about it yet.
	lease, err := n.service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: n.id, Generation: n.generation, ProcessEpoch: n.epoch, Ready: true, Subjects: n.members(), SubjectsDigest: relaySubjectsDigestOf(t, n.members())})
	if err != nil {
		t.Fatalf("heartbeat of an operator-drained relay refused: %v", err)
	}
	if !lease.Draining || lease.DrainDeadline != deadline.Unix() {
		t.Fatalf("lease draining=%v deadline=%d, want true and %d", lease.Draining, lease.DrainDeadline, deadline.Unix())
	}
	if s, ready, d := state(); s != "draining" || ready || d == nil || !d.Equal(deadline) {
		t.Fatalf("after heartbeat: state %s ready %v deadline %v, want draining, not ready, %v", s, ready, d, deadline)
	}

	// 2. The process restarts inside the drain: admitted, draining, deadline kept.
	start := func(epoch string, expected int64) (int, relayNodeLeaseDocument) {
		raw, _ := json.Marshal(relayNodeStart{NodeID: n.id, ExpectedGeneration: expected, ProcessEpoch: epoch})
		req := httptest.NewRequest(http.MethodPost, "/v1/relay/nodes/start", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		n.service.handleRelayStart(rec, req)
		var lease relayNodeLeaseDocument
		_ = json.Unmarshal(rec.Body.Bytes(), &lease)
		return rec.Code, lease
	}
	n.exec(ctx, `UPDATE control_tunnel_nodes SET registry_expires_at=now()-interval '1 minute' WHERE id=$1`, n.id) // the old process is gone
	code, restarted := start(n.epoch+"-restart", n.generation)
	if code != http.StatusOK || !restarted.Draining || restarted.DrainDeadline != deadline.Unix() {
		t.Fatalf("restart while draining: status %d draining=%v deadline=%d, want 200, true, %d", code, restarted.Draining, restarted.DrainDeadline, deadline.Unix())
	}
	if s, ready, d := state(); s != "draining" || ready || d == nil || !d.Equal(deadline) {
		t.Fatalf("after restart: state %s ready %v deadline %v, want draining, not ready, %v", s, ready, d, deadline)
	}
	if _, err = n.service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: n.id, Generation: restarted.Generation, ProcessEpoch: restarted.ProcessEpoch, Ready: true, Subjects: n.members(), SubjectsDigest: relaySubjectsDigestOf(t, n.members())}); err != nil {
		t.Fatalf("restarted draining relay's heartbeat: %v", err)
	}
	if s, ready, _ := state(); s != "draining" || ready {
		t.Fatalf("a draining relay reporting ready: state %s ready %v, want draining and not ready", s, ready)
	}

	// 3. The grace ends (ReconcileExpiredDrains: offline, deadline kept). The drain stays in force: the drained process
	// keeps heartbeating, and a restart is admitted drained and never turns ready on its own.
	n.exec(ctx, `UPDATE control_tunnel_nodes SET state='offline',ready=false WHERE id=$1`, n.id)
	lease, err = n.service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: n.id, Generation: restarted.Generation, ProcessEpoch: restarted.ProcessEpoch, Draining: true, Subjects: n.members(), SubjectsDigest: relaySubjectsDigestOf(t, n.members())})
	if err != nil || !lease.Draining {
		t.Fatalf("drained heartbeat past the deadline: err %v draining %v, want accepted and draining", err, lease.Draining)
	}
	n.exec(ctx, `UPDATE control_tunnel_nodes SET registry_expires_at=now()-interval '1 minute' WHERE id=$1`, n.id)
	code, late := start(n.epoch+"-late", restarted.Generation)
	if code != http.StatusOK || !late.Draining {
		t.Fatalf("restart after the deadline: status %d draining %v, want 200 and draining", code, late.Draining)
	}
	lease, err = n.service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: n.id, Generation: late.Generation, ProcessEpoch: late.ProcessEpoch, Ready: true, Subjects: n.members(), SubjectsDigest: relaySubjectsDigestOf(t, n.members())})
	if err != nil || !lease.Draining {
		t.Fatalf("first heartbeat of the late restart: err %v draining %v, want accepted and draining", err, lease.Draining)
	}
	if s, ready, d := state(); s != "offline" || ready || d == nil || !d.Equal(deadline) {
		t.Fatalf("drain past its deadline: state %s ready %v deadline %v, want offline, not ready, %v", s, ready, d, deadline)
	}

	// 4. Undrain (nodedrain.Resume: registered, no deadline). The still-drained process reports draining once (its own
	// shutdown: offline) and learns the drain is lifted; its restart, inside the live lease, is admitted and turns ready.
	n.exec(ctx, `UPDATE control_tunnel_nodes SET state='registered',ready=false,drain_deadline=NULL WHERE id=$1`, n.id)
	lease, err = n.service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: n.id, Generation: late.Generation, ProcessEpoch: late.ProcessEpoch, Draining: true, Subjects: n.members(), SubjectsDigest: relaySubjectsDigestOf(t, n.members())})
	if err != nil || lease.Draining {
		t.Fatalf("drained process after undrain: err %v draining %v, want accepted and not draining", err, lease.Draining)
	}
	code, fresh := start(n.epoch+"-undrained", late.Generation)
	if code != http.StatusOK || fresh.Draining || fresh.DrainDeadline != 0 {
		t.Fatalf("start after undrain: status %d draining=%v, want 200 and not draining", code, fresh.Draining)
	}
	lease, err = n.service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: n.id, Generation: fresh.Generation, ProcessEpoch: fresh.ProcessEpoch, Ready: true, Subjects: n.members(), SubjectsDigest: relaySubjectsDigestOf(t, n.members())})
	if err != nil || lease.Draining {
		t.Fatalf("heartbeat after undrain: err %v draining %v", err, lease.Draining)
	}
	if s, ready, d := state(); s != "ready" || !ready || d != nil {
		t.Fatalf("after undrain: state %s ready %v deadline %v, want ready", s, ready, d)
	}
}
