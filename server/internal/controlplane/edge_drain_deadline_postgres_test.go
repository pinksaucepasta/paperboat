package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/nodedrain"
	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
)

type drainHeartbeatFixture struct {
	t       *testing.T
	store   *db.DB
	service *EdgeService
	node    string
}

func newDrainHeartbeatFixture(t *testing.T) *drainHeartbeatFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	f := &drainHeartbeatFixture{t: t, store: store, service: NewEdgeService(store, "credential"), node: fmt.Sprintf("drain_hb_%d", time.Now().UnixNano())}
	if _, err = store.SQL().ExecContext(t.Context(), `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region','domain',ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour')`, f.node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.node)
	})
	return f
}

func (f *drainHeartbeatFixture) heartbeat(ready, draining bool) *int64 {
	f.t.Helper()
	deadline, err := f.service.Heartbeat(f.t.Context(), edgeNodeObservation{NodeID: f.node, ProcessEpoch: "epoch", Ready: ready, Draining: draining, At: time.Now()})
	if err != nil {
		f.t.Fatal(err)
	}
	return deadline
}

func (f *drainHeartbeatFixture) drain(move bool) {
	f.t.Helper()
	err := f.store.InTx(f.t.Context(), func(ctx context.Context, tx *db.Tx) error { return nodedrain.Begin(ctx, tx, f.node, move) })
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *drainHeartbeatFixture) row() (state string, ready bool, deadline *time.Time) {
	f.t.Helper()
	if err := f.store.SQL().QueryRowContext(f.t.Context(), `SELECT state,ready,drain_deadline FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.node).Scan(&state, &ready, &deadline); err != nil {
		f.t.Fatal(err)
	}
	return
}

func TestHeartbeatAnswerCarriesOperatorDrainDeadline(t *testing.T) {
	f := newDrainHeartbeatFixture(t)
	f.drain(true)
	deadline := f.heartbeat(true, false)
	want := time.Now().Add(nodedrain.MoveGrace).Unix()
	if deadline == nil || *deadline < want-15 || *deadline > want+2 {
		t.Fatalf("deadline %v, want about %d (Unix seconds, one move grace ahead)", deadline, want)
	}
	recorder := httptest.NewRecorder()
	writeHeartbeatAnswer(recorder, edgelimits.Default(), deadline)
	var answer struct {
		DrainDeadline *int64 `json:"drain_deadline"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.DrainDeadline == nil || *answer.DrainDeadline != *deadline {
		t.Fatalf("answer %s: %v", recorder.Body.String(), err)
	}
	// A deadline already past is not announced: the edge's carriers are gone with the admissions.
	if _, err := f.store.SQL().ExecContext(t.Context(), `UPDATE paperboat.control_tunnel_nodes SET drain_deadline=now()-interval '1 second' WHERE id=$1`, f.node); err != nil {
		t.Fatal(err)
	}
	if got := f.heartbeat(true, false); got != nil {
		t.Fatalf("a past deadline was announced: %d", *got)
	}
}

func TestHeartbeatAnswerHasNoDeadlineWhenNotDraining(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeHeartbeatAnswer(recorder, edgelimits.Default(), nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["drain_deadline"]; present {
		t.Fatalf("answer for a node that is not draining names a deadline: %s", recorder.Body.String())
	}
	f := newDrainHeartbeatFixture(t)
	if got := f.heartbeat(true, false); got != nil {
		t.Fatalf("a ready node was told a deadline: %d", *got)
	}
	// The edge's own shutdown drain has no deadline: nothing to announce back to it.
	if got := f.heartbeat(false, true); got != nil {
		t.Fatalf("the edge's own drain was answered with a deadline: %d", *got)
	}
}

func TestOperatorDrainSurvivesEdgeHeartbeats(t *testing.T) {
	f := newDrainHeartbeatFixture(t)
	f.drain(false)
	_, _, first := f.row()
	for range 3 {
		if got := f.heartbeat(true, false); got == nil {
			t.Fatal("a draining node was not told its deadline")
		}
		state, ready, deadline := f.row()
		if state != "draining" || ready || deadline == nil || !deadline.Equal(*first) {
			t.Fatalf("a heartbeat that says ready undid the drain: %s ready=%v deadline=%v (was %v)", state, ready, deadline, first)
		}
	}
	// Resume brings it back: registered, then ready on the next heartbeat that says so, with no deadline in the answer.
	if err := f.store.InTx(t.Context(), func(ctx context.Context, tx *db.Tx) error { return nodedrain.Resume(ctx, tx, f.node) }); err != nil {
		t.Fatal(err)
	}
	if got := f.heartbeat(true, false); got != nil {
		t.Fatalf("a resumed node was told a deadline: %d", *got)
	}
	if state, ready, deadline := f.row(); state != "ready" || !ready || deadline != nil {
		t.Fatalf("after resume: %s ready=%v deadline=%v", state, ready, deadline)
	}
}
