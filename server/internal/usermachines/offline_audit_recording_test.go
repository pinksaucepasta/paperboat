package usermachines

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// Recording reports ride the audit backlog: the report alone, valid for its kind, and nothing of the session kinds.
func TestRecordingAuditEventsCarryOnlyAValidReport(t *testing.T) {
	now := time.Now().UTC()
	report := &recording.Report{RecordingID: "rec_01234567-89ab-cdef-0123-456789abcdef", SessionID: "sess_1", Trigger: recording.TriggerRule, KeyGeneration: 1, StartedAt: now}
	ok := OfflineAuditEvent{EventID: "ev_1", Kind: recording.KindStarted, OccurredAt: now, Recording: report}
	if !validOfflineAuditEvent(ok, now) {
		t.Fatal("valid recording_started rejected")
	}
	for name, e := range map[string]OfflineAuditEvent{
		"no report":         {EventID: "ev_1", Kind: recording.KindStarted, OccurredAt: now},
		"wrong kind":        {EventID: "ev_1", Kind: recording.KindStopped, OccurredAt: now, Recording: report},
		"report on session": {EventID: "ev_1", Kind: "offline_session_started", OccurredAt: now, Recording: report, Capability: "terminal", OperationID: "op_1", PeerEndpointID: "cli_1", PeerAccountID: "acct_1"},
		"extra peer":        {EventID: "ev_1", Kind: recording.KindStarted, OccurredAt: now, Recording: report, PeerAccountID: "acct_1"},
		"too old":           {EventID: "ev_1", Kind: recording.KindStarted, OccurredAt: now.Add(-8 * 24 * time.Hour), Recording: report},
	} {
		if validOfflineAuditEvent(e, now) {
			t.Errorf("%s accepted", name)
		}
	}
}
