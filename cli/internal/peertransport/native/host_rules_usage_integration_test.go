package native_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tstest/integration"
)

type usageSink struct {
	mu      sync.Mutex
	reports []api.NativeUsageReport
}

func (s *usageSink) ReportNativeUsage(_ context.Context, reports []api.NativeUsageReport, _, _ int64, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, reports...)
	return nil
}

type closeSignalConn struct {
	net.Conn
	closed chan<- struct{}
}

func (c closeSignalConn) Close() error {
	err := c.Conn.Close()
	select {
	case c.closed <- struct{}{}:
	default:
	}
	return err
}

// Every managed SSH stream is admitted from the host's own rules (D1, 46cc2833d), so it carries no access session.
// Its usage must still name the peer the native session proved, or the server cannot attribute it and every SSH
// byte goes unrecorded. 1 MiB each way over a host-rules SSH stream is reported under ssh, for the peer, exactly.
func TestHostRulesSSHStreamUsageNamesItsPeer(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_peer", EndpointID: "cli_client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_host", EndpointID: "machine_host", Role: "machine", MachineID: "machine_host", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	serverConfig.Peers[0].Scopes = append(serverConfig.Peers[0].Scopes,
		tailnet.NetworkScope{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "managed_ssh", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: now + 300})
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	sink := &usageSink{}
	usage, err := bandwidth.Open(filepath.Join(t.TempDir(), "usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	// As the host runtime wires it (runtime/native_peer_unix.go). hostClosed fires when the echo closes its end, after
	// io.Copy returned: QUIC hands the client the last bytes before the host's Write returns and is metered.
	hostClosed := make(chan struct{}, 1)
	meter := func(conn net.Conn, binding native.MeterBinding) net.Conn {
		return closeSignalConn{Conn: usage.Wrap(conn, bandwidth.Binding{AccessSessionID: binding.AccessSessionID, StreamID: binding.StreamID, Consumer: binding.Consumer, PeerAccountID: binding.PeerAccountID, Reverse: binding.Reverse}, func(direction string) bandwidth.Path {
			mode, node := binding.Path(direction)
			return bandwidth.Path{Mode: mode, NodeID: node}
		}), closed: hostClosed}
	}
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS, MeterIncoming: meter, AdmitOffline: func(context.Context, native.OfflineAdmission) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	descriptor := startOfflineTestServer(t, serverOwner, serverAuthority, dm.Regions[1])
	session, err := clientOwner.Dial(t.Context(), descriptor, "machine_host", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	header, err := streamauth.NewOffline("op_ssh", "ssh", "stream_ssh", time.Now().Add(time.Minute), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenOffline(t.Context(), header)
	if err != nil {
		t.Fatal(err)
	}
	const size = 1 << 20
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	go func() { _, _ = stream.Write(payload) }()
	_ = stream.SetReadDeadline(time.Now().Add(20 * time.Second))
	echoed := make([]byte, size)
	if _, err := io.ReadFull(stream, echoed); err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("ssh echo: %v", err)
	}
	_ = stream.Close()
	select {
	case <-hostClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("the host never finished serving the ssh stream")
	}
	closeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := usage.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var up, down int64
	for _, r := range sink.reports {
		if r.Consumer != "ssh" || r.AccessSessionID != "machine_host" || r.PeerAccountID != "account_peer" || r.StreamID != "stream_ssh" {
			t.Fatalf("usage report %+v, want ssh for account_peer on machine_host", r)
		}
		up, down = up+r.UploadBytes, down+r.DownloadBytes
	}
	if up != size || down != size {
		t.Fatalf("ssh usage %d up / %d down over %d reports, want %d each way", up, down, len(sink.reports), size)
	}
}
