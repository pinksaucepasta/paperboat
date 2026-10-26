//go:build darwin || linux || windows

package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// Every offline decision is recorded before the host acts: a start with a scope is admitted, a
// start without one or on a dispatching host is refused and recorded as a refusal (F21-11).
func TestOfflineAdmissionRecordsEveryDecisionFirst(t *testing.T) {
	queue, err := offlineaudit.Open(filepath.Join(t.TempDir(), "offline-audit.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(allowed bool) native.OfflineAdmission {
		return native.OfflineAdmission{Peer: tailnet.NetworkBinding{AccountID: "acct", EndpointID: "cli_1"}, Header: streamauth.Header{OperationID: "op_1"}, Capability: "exec", Allowed: allowed}
	}
	if err := offlineAdmission(queue, false)(context.Background(), request(true)); err != nil {
		t.Fatalf("a scoped offline start was refused: %v", err)
	}
	if err := offlineAdmission(queue, false)(context.Background(), request(false)); err == nil {
		t.Fatal("an offline start without a scope was admitted")
	}
	if err := offlineAdmission(queue, true)(context.Background(), request(true)); err == nil {
		t.Fatal("a dispatching host admitted an offline start")
	}
	if offlineAdmission(nil, false) != nil {
		t.Fatal("a host without an audit queue would admit offline sessions")
	}
	events := queue.Oldest(10)
	want := []struct{ kind, reason string }{{offlineaudit.KindStarted, ""}, {offlineaudit.KindRefused, offlineaudit.ReasonNoScope}, {offlineaudit.KindRefused, offlineaudit.ReasonPeerUnknown}}
	if len(events) != len(want) {
		t.Fatalf("recorded %+v", events)
	}
	for i, w := range want {
		if events[i].Kind != w.kind || events[i].Reason != w.reason || events[i].PeerAccountID != "acct" || events[i].OperationID != "op_1" {
			t.Fatalf("event %d = %+v, want %s/%s", i, events[i], w.kind, w.reason)
		}
	}
}
