package controlplane

import "testing"

// F21-01: the control plane answered a relay late (slow after an API cut). Refusing the late heartbeat fenced the
// relay, which restarted under a new process epoch and invalidated every grant, ending live relayed sessions. A
// heartbeat from the process that owns the node's generation and epoch renews the lease even after it ran out and
// the reconciler marked the node offline; another epoch, or a retired node, is still refused.
func TestRelayLateHeartbeatRenewsItsLeaseOnPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "late")
	n.exec(t.Context(), `UPDATE control_tunnel_nodes SET registry_expires_at=now()-interval '2 minutes',last_heartbeat_at=now()-interval '3 minutes',state='offline',ready=false WHERE id=$1`, n.id)
	n.mustObserve(nil, "")
	if live := n.count(`SELECT count(*) FROM control_tunnel_nodes WHERE id=$1 AND state='ready' AND ready AND registry_expires_at>now()`, n.id); live != 1 {
		t.Fatal("a late heartbeat from the node's own process did not renew its lease")
	}
	owner := n.epoch
	n.epoch = owner + "_other"
	if _, err := n.observe(nil, ""); err == nil {
		t.Fatal("a heartbeat from another process epoch was accepted")
	}
	n.epoch = owner
	n.exec(t.Context(), `UPDATE control_tunnel_nodes SET state='retired',ready=false WHERE id=$1`, n.id)
	if _, err := n.observe(nil, ""); err == nil {
		t.Fatal("a retired node's heartbeat was accepted")
	}
}
