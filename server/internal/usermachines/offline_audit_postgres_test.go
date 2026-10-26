package usermachines

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/teams"
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

// PLAN 3b D1: managed SSH has no server admission, so the host reports every SSH session, online or offline, through
// the same endpoint: a start and its end, and a refusal with a typed reason.
func TestSSHSessionEventsLandInTheAuditLogOfTheDecidingWorkspacePostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	var generation int64
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT installation_generation FROM paperboat.user_machines WHERE id=$1`, f.machine).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := OfflineAuditEvent{EventID: "ssh_start_" + f.suffix, Kind: "ssh_session_started", PeerEndpointID: "cli_" + f.suffix, PeerAccountID: f.account, OccurredAt: now.Add(-time.Minute), Origin: "online", Generation: 3}
	end := OfflineAuditEvent{EventID: "ssh_end_" + f.suffix, Kind: "ssh_session_ended", PeerEndpointID: start.PeerEndpointID, PeerAccountID: f.account, OccurredAt: now, Origin: "online", SessionID: start.EventID, Outcome: "revoked", BytesIn: 10, BytesOut: 20}
	refused := OfflineAuditEvent{EventID: "ssh_no_" + f.suffix, Kind: "ssh_session_refused", PeerEndpointID: start.PeerEndpointID, PeerAccountID: f.account, OccurredAt: now, Origin: "offline", Reason: "busy"}
	receipt, err := f.service.RecordOfflineAuditBacklog(f.ctx, f.machine, generation, []OfflineAuditEvent{start, end, refused})
	if err != nil || receipt.Accepted != 3 {
		t.Fatalf("upload: %+v %v", receipt, err)
	}
	rows, err := f.store.SQL().QueryContext(f.ctx, `SELECT event_type,result,coalesce(activity_workspace_id,''),metadata->>'origin',metadata->>'capability',coalesce(metadata->>'outcome',''),coalesce(metadata->>'bytes_out','') FROM paperboat.audit_events WHERE resource_type='machine_access' AND resource_id=$1 AND metadata->>'capability'='managed_ssh' ORDER BY event_type`, f.machine)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var typ, result, workspace, origin, capability, outcome, out string
		if err := rows.Scan(&typ, &result, &workspace, &origin, &capability, &outcome, &out); err != nil {
			t.Fatal(err)
		}
		if workspace != "personal:"+f.account {
			t.Errorf("%s filed in %q", typ, workspace)
		}
		got = append(got, typ+"/"+result+"/"+origin+"/"+outcome+"/"+out)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// What the workspace audit log (GET /v1/activity and its CSV export) returns for these events: who, outcome, bytes,
	// origin, and the start's id that the end names.
	details := map[string]map[string]any{}
	metaRows, err := f.store.SQL().QueryContext(f.ctx, `SELECT event_type,metadata FROM paperboat.audit_events WHERE resource_type='machine_access' AND resource_id=$1 AND metadata->>'capability'='managed_ssh'`, f.machine)
	if err != nil {
		t.Fatal(err)
	}
	for metaRows.Next() {
		var typ string
		var raw []byte
		if err := metaRows.Scan(&typ, &raw); err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		details[typ] = teams.ProjectActivityMetadata(metadata)
	}
	metaRows.Close()
	ended, started := details[accessEndedEvent], details[accessStartedEvent]
	if ended["outcome"] != "revoked" || ended["bytes_in"] != float64(10) || ended["bytes_out"] != float64(20) || ended["origin"] != "online" || ended["session_id"] != start.EventID ||
		started["session_id"] != start.EventID || started["origin"] != "online" || started["peer_account_id"] != f.account || started["scope_generation"] != float64(3) {
		t.Fatalf("activity details: ended %v started %v", ended, started)
	}
	want := []string{accessDeniedEvent + "/denied/offline//", accessEndedEvent + "/success/online/revoked/20", accessStartedEvent + "/success/online//"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("audit rows %v, want %v", got, want)
	}
	bad := map[string]OfflineAuditEvent{}
	e := start
	e.EventID, e.Origin = "bad_origin_"+f.suffix, ""
	bad["origin"] = e
	e = refused
	e.EventID, e.Reason = "bad_reason_"+f.suffix, "no_scope"
	bad["reason"] = e
	e = end
	e.EventID, e.SessionID = "bad_end_"+f.suffix, ""
	bad["end without session"] = e
	e = end
	e.EventID, e.Outcome = "bad_outcome_"+f.suffix, "weird"
	bad["outcome"] = e
	for name, ev := range bad {
		if _, err := f.service.RecordOfflineAuditBacklog(f.ctx, f.machine, generation, []OfflineAuditEvent{ev}); !errors.Is(err, ErrOfflineAuditInvalid) {
			t.Errorf("%s: %v, want ErrOfflineAuditInvalid", name, err)
		}
	}
}
