package usermachines

import (
	"testing"
	"time"
)

// A host queues the end of every exec it ran (outcome, exit code, duration), since a standing-rule exec makes no
// control-plane request; only an exec end carries those fields, and only with a known final state.
func TestExecEndAuditCarriesOutcomeExitCodeAndDuration(t *testing.T) {
	now := time.Now().UTC()
	code := 3
	end := OfflineAuditEvent{EventID: "exec_end_x", Kind: "offline_session_ended", Capability: "exec", PeerEndpointID: "cli_x", PeerAccountID: "acct_x", OperationID: "op_x", OccurredAt: now, Origin: "online", Outcome: "exited", ExitCode: &code, DurationMS: 1500}
	if !validOfflineAuditEvent(end, now) {
		t.Fatal("an exec end was rejected")
	}
	eventType, result, metadata := auditEventFor(end, 1)
	if eventType != accessEndedEvent || result != "success" || metadata["exit_code"] != 3 || metadata["duration_ms"] != int64(1500) || metadata["outcome"] != "exited" || metadata["origin"] != "online" || metadata["operation_id"] != "op_x" {
		t.Fatalf("exec end audit = %s %s %v", eventType, result, metadata)
	}
	for name, bad := range map[string]func(*OfflineAuditEvent){
		"unknown outcome": func(e *OfflineAuditEvent) { e.Outcome = "vanished" },
		"terminal end":    func(e *OfflineAuditEvent) { e.Capability = "terminal" },
		"negative":        func(e *OfflineAuditEvent) { e.DurationMS = -1 },
		"start with code": func(e *OfflineAuditEvent) { e.Kind, e.Outcome, e.DurationMS = "offline_session_started", "", 0 },
		"ssh with code":   func(e *OfflineAuditEvent) { e.Kind, e.Capability, e.Outcome = "ssh_session_ended", "", "closed" },
		"missing origin":  func(e *OfflineAuditEvent) { e.Origin = "" },
	} {
		e := end
		bad(&e)
		if validOfflineAuditEvent(e, now) {
			t.Errorf("%s: accepted %+v", name, e)
		}
	}
}
