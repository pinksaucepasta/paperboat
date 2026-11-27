package runtimeattachment

import (
	"errors"
	"testing"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// L42 (staging drill F21-17): a pull that reaches the server before a reconnecting edge's heartbeat is refused as
// retryable instead of answered without the edge's carriers; once the heartbeat lands it is answered in full.
func TestPullRefusedUntilReconnectingEdgeHeartbeatsOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "reconnect")
	if _, err := f.service.Attach(f.ctx, f.proof, f.request); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SQL().ExecContext(f.ctx, `UPDATE control_tunnel_nodes SET last_heartbeat_at=now()-interval '100 seconds' WHERE id=$1`, f.edge.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(f.ctx, f.edge, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pull before the reconnect heartbeat = %v, want ErrUnavailable", err)
	}
	caps := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityEndpointDeltas}
	if _, err := f.service.PullEndpoints(f.ctx, f.edge, caps, "", "", 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("endpoint pull before the reconnect heartbeat = %v, want ErrUnavailable", err)
	}
	if _, err := f.database.SQL().ExecContext(f.ctx, `UPDATE control_tunnel_nodes SET last_heartbeat_at=now() WHERE id=$1`, f.edge.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(f.ctx, f.edge, nil); err != nil {
		t.Fatalf("pull after the heartbeat = %v, want an answer", err)
	}
}
