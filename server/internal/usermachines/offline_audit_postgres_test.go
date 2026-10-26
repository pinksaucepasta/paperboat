package usermachines

import (
	"errors"
	"testing"
	"time"
)

// F21-11: a host's offline session events land once each in the audit log of the workspace that decides that access,
// marked origin=offline; a re-upload is counted as duplicates; invalid events and a revoked machine are refused.
func TestOfflineAuditBacklogLandsOnceInTheDecidingWorkspacePostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	var generation int64
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT installation_generation FROM paperboat.user_machines WHERE id=$1`, f.machine).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []OfflineAuditEvent{
		{EventID: "evt_started_" + f.suffix, Kind: "offline_session_started", Capability: "terminal", PeerEndpointID: "cli_" + f.suffix, PeerAccountID: f.account, OperationID: "op_" + f.suffix, OccurredAt: now.Add(-time.Hour)},
		{EventID: "evt_refused_" + f.suffix, Kind: "offline_session_refused", Capability: "exec", PeerEndpointID: "cli_other_" + f.suffix, PeerAccountID: "gone_" + f.suffix, OperationID: "op_refused_" + f.suffix, OccurredAt: now.Add(-time.Minute), Reason: "no_scope"},
	}
	receipt, err := f.service.RecordOfflineAuditBacklog(f.ctx, f.machine, generation, events)
	if err != nil || receipt.Accepted != 2 || receipt.Duplicates != 0 {
		t.Fatalf("first upload: %+v %v", receipt, err)
	}
	type row struct{ event, result, workspace, origin, actor string }
	rows, err := f.store.SQL().QueryContext(f.ctx, `SELECT event_type,result,coalesce(activity_workspace_id,''),metadata->>'origin',coalesce(actor_user_id,'') FROM paperboat.audit_events WHERE resource_type IN ('machine_access','user_machine') AND resource_id=$1 AND idempotency_key LIKE 'offline:%' ORDER BY event_type`, f.machine)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.event, &r.result, &r.workspace, &r.origin, &r.actor); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 2 || got[0].event != accessDeniedEvent || got[0].result != "denied" || got[0].actor != "" || got[1].event != accessStartedEvent || got[1].actor != f.account {
		t.Fatalf("audit rows %+v", got)
	}
	for _, r := range got {
		if r.origin != "offline" || r.workspace != "personal:"+f.account {
			t.Fatalf("event %s: origin %q workspace %q, want offline in personal:%s", r.event, r.origin, r.workspace, f.account)
		}
	}
	if again, err := f.service.RecordOfflineAuditBacklog(f.ctx, f.machine, generation, events); err != nil || again.Accepted != 0 || again.Duplicates != 2 {
		t.Fatalf("re-upload: %+v %v", again, err)
	}
	future := events[0]
	future.EventID, future.OccurredAt = "evt_future_"+f.suffix, now.Add(time.Hour)
	unknownReason := events[1]
	unknownReason.EventID, unknownReason.Reason = "evt_reason_"+f.suffix, "because"
	for name, batch := range map[string][]OfflineAuditEvent{"future": {future}, "reason": {unknownReason}, "empty": nil} {
		if _, err := f.service.RecordOfflineAuditBacklog(f.ctx, f.machine, generation, batch); !errors.Is(err, ErrOfflineAuditInvalid) {
			t.Errorf("%s: %v, want ErrOfflineAuditInvalid", name, err)
		}
	}
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET state='revoked',revoked_at=now() WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	late := events[0]
	late.EventID = "evt_late_" + f.suffix
	if _, err := f.service.RecordOfflineAuditBacklog(f.ctx, f.machine, generation, []OfflineAuditEvent{late}); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked machine: %v, want ErrNotFound", err)
	}
}
