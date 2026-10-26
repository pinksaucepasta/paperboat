//go:build darwin || linux || windows

package runtime

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
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

// F5-12: a managed SSH session is queued for the workspace audit log whether or not Paperboat was reachable:
// the host's own audit queues the start, refusal and end (with bytes and the start's id), origin online or offline;
// the offline admission records no offline_session_* for SSH, only a refusal as ssh_session_refused.
func TestManagedSSHSessionsAreQueuedForAuditOnlineAndOffline(t *testing.T) {
	queue, err := offlineaudit.Open(filepath.Join(t.TempDir(), "offline-audit.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
		}
	}()
	host, err := managedssh.NewHost(managedssh.HostConfig{MaxStreams: 1, ProbeTimeout: time.Second, DialTimeout: time.Second, Audit: SSHSessionAudit(queue)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReconcileTarget(context.Background(), 5, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	serve := func(peer managedssh.Peer) {
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() { _, _ = host.Serve(managedssh.WithPeer(context.Background(), peer), 5, server); close(done) }()
		_, _ = client.Write([]byte("0123456789"))
		_, _ = io.ReadFull(client, make([]byte, 10))
		_ = client.Close()
		<-done
	}
	serve(managedssh.Peer{AccountID: "acct", EndpointID: "cli_1", OperationID: "op_online"})
	serve(managedssh.Peer{AccountID: "acct", EndpointID: "cli_1", OperationID: "op_offline", Offline: true})
	admission := native.OfflineAdmission{Peer: tailnet.NetworkBinding{AccountID: "acct", EndpointID: "cli_2"}, Header: streamauth.Header{OperationID: "op_denied"}, Capability: "managed_ssh"}
	if err := offlineAdmission(queue, false)(context.Background(), admission); err == nil {
		t.Fatal("an offline SSH start without a scope was admitted")
	}
	admission.Allowed = true
	if err := offlineAdmission(queue, false)(context.Background(), admission); err != nil {
		t.Fatalf("a scoped offline SSH start was refused: %v", err)
	}
	// F5-08: a carrier (dispatching) serves SSH itself, so it admits a scoped offline SSH start too.
	if err := offlineAdmission(queue, true)(context.Background(), admission); err != nil {
		t.Fatalf("a carrier refused a scoped offline SSH start: %v", err)
	}

	events := queue.Oldest(20)
	kinds := []string{}
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{offlineaudit.KindSSHStarted, offlineaudit.KindSSHEnded, offlineaudit.KindSSHStarted, offlineaudit.KindSSHEnded, offlineaudit.KindSSHRefused}
	if len(events) != len(want) {
		t.Fatalf("queued %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("queued %v, want %v", kinds, want)
		}
	}
	for i, origin := range map[int]string{0: offlineaudit.OriginOnline, 2: offlineaudit.OriginOffline} {
		started, ended := events[i], events[i+1]
		if started.Origin != origin || ended.Origin != origin || started.Generation != 5 || started.PeerAccountID != "acct" || started.PeerEndpointID != "cli_1" {
			t.Fatalf("session at %d: %+v %+v", i, started, ended)
		}
		if ended.SessionID != started.EventID || ended.Outcome != managedssh.EndedClosed || ended.BytesIn != 10 || ended.BytesOut != 10 {
			t.Fatalf("end at %d: %+v (start %s)", i+1, ended, started.EventID)
		}
	}
	if events[0].OperationID != "op_online" || events[2].OperationID != "op_offline" {
		t.Fatalf("operation ids: %q %q", events[0].OperationID, events[2].OperationID)
	}
	if r := events[4]; r.Reason != managedssh.RefusedNoAccess || r.Origin != offlineaudit.OriginOffline || r.OperationID != "op_denied" || r.PeerEndpointID != "cli_2" {
		t.Fatalf("refusal = %+v", r)
	}
}

// F5-12: the carrier serves real `pb ssh` streams on its own Host, so its sessions reach its persisted backlog.
func TestCarrierSSHSessionsAreQueuedInTheCarrierBacklog(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
		}
	}()
	host, err := managedssh.NewHost(managedssh.HostConfig{MaxStreams: 1, ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := auditCarrierSSH(root, host); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReconcileTarget(context.Background(), 5, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		_, _ = host.Serve(managedssh.WithPeer(context.Background(), managedssh.Peer{AccountID: "acct", EndpointID: "cli_1", OperationID: "op_1"}), 5, server)
		close(done)
	}()
	_, _ = client.Write([]byte("abc"))
	_, _ = io.ReadFull(client, make([]byte, 3))
	_ = client.Close()
	<-done
	reopened, err := offlineaudit.Open(filepath.Join(root, "carrier-offline-audit.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	events := reopened.Oldest(10)
	if len(events) != 2 || events[0].Kind != offlineaudit.KindSSHStarted || events[1].Kind != offlineaudit.KindSSHEnded || events[1].BytesIn != 3 {
		t.Fatalf("carrier backlog = %+v", events)
	}
}
