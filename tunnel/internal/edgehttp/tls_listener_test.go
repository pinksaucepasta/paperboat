package edgehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
)

type tlsAuthorityFixture struct{ publicTCPAuthorityFixture }

func (f *tlsAuthorityFixture) ResolveDecision(ctx context.Context, d connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	decisions, _ := f.Snapshot(ctx)
	for _, current := range decisions {
		if current.Binding == d.Binding {
			return current, nil
		}
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}

func TestTLSListenerTwoOriginsCertificatesAndRemoval(t *testing.T) {
	// Two real TLS origins own separate certificates and receive original client
	// handshakes through the production listener and authenticated carrier bridge.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	identity := replicaIdentity("host_pinned", "connector_pinned", "session_pinned", 3)
	carrier, connector := testEdgePreviewCarrierPair(t, identity)
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err = registry.Attach(carrier); err != nil {
		t.Fatal(err)
	}
	authority := &tlsAuthorityFixture{}
	decisions := make([]connectorprotocol.IngressDecision, 2)
	clients := make([]*tls.Config, 2)
	certs := make([][]byte, 2)
	origins := make(map[string]string)
	var originListeners []net.Listener
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var workers sync.WaitGroup
	for i := range 2 {
		host := fmt.Sprintf("origin%d.customer.test", i)
		clientCfg, serverCfg, _, _, _ := task24Certificates(t, host)
		clientCfg.ServerName = host
		clients[i] = clientCfg
		certs[i] = serverCfg.Certificates[0].Certificate[0]
		origin, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
		if err != nil {
			t.Fatal(err)
		}
		defer origin.Close()
		originListeners = append(originListeners, origin)
		d := publicTCPDecision(time.Now().UTC(), 443)
		d.Binding.Protocol = "tls"
		d.Binding.ListenerID = "listener_tls_443"
		d.Binding.Hostname = host
		d.Binding.RouteID = fmt.Sprintf("route_tls_%d", i)
		d.Binding.TargetID = d.Binding.RouteID
		d.Binding.PublicationID = d.Binding.RouteID
		decisions[i] = d
		origins[d.Binding.RouteID] = origin.Addr().String()
		workers.Go(func() {
			for {
				conn, err := origin.Accept()
				if err != nil {
					return
				}
				workers.Go(func() {
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					data, err := io.ReadAll(conn)
					if err == nil {
						_, _ = conn.Write(data)
					}
				})
			}
		})
	}
	authority.set(decisions...)

	httpsClient, httpsServer, _, _, _ := task24Certificates(t, "web.customer.test")
	httpsClient.ServerName = "web.customer.test"
	httpsServer.ClientAuth = tls.NoClientCert
	var httpsConflict atomic.Bool
	manager, err := NewSharedTLSListener(probe, authority, registry, func(host string) bool {
		return host == "web.customer.test" || host == decisions[0].Binding.Hostname && httpsConflict.Load()
	}, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	web := &http.Server{TLSConfig: httpsServer, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Protocol", r.Proto)
		_, _ = io.WriteString(w, "https preserved")
	})}
	defer web.Close()
	go web.ServeTLS(manager, "", "")
	transport := &http.Transport{TLSClientConfig: httpsClient, ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	checkHTTPS := func() {
		t.Helper()
		response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get("https://web.customer.test/")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || string(data) != "https preserved" || response.ProtoMajor != 2 {
			t.Fatalf("HTTPS/HTTP2 failed: %s %q %v", response.Proto, data, err)
		}
	}
	checkHTTPS()

	workers.Go(func() {
		for {
			stream, open, err := connector.AcceptStream(ctx)
			if err != nil {
				return
			}
			workers.Go(func() {
				defer stream.Close()
				d, err := connectorprotocol.ReadIngressDecision(stream, time.Now().UTC())
				if err != nil {
					return
				}
				if d.Binding.RouteID != open.RouteID {
					return
				}
				origin, err := net.DialTimeout("tcp", origins[open.RouteID], time.Second)
				if err != nil {
					return
				}
				defer origin.Close()
				finished := make(chan struct{})
				go func() { _, _ = io.Copy(origin, stream); _ = origin.(*net.TCPConn).CloseWrite(); close(finished) }()
				_, _ = io.Copy(stream, origin)
				_ = stream.CloseWrite()
				<-finished
			})
		}
	})
	roundtrip := func(i int) {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, clients[i])
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if !bytes.Equal(conn.ConnectionState().PeerCertificates[0].Raw, certs[i]) {
			t.Fatal("edge replaced origin certificate")
		}
		payload := bytes.Repeat([]byte{byte(i), 0, 255, 13, 10}, 20000)
		if _, err = conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err = conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(conn)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("TLS stream mismatch: %v", err)
		}
	}
	roundtrip(0)
	roundtrip(1)
	duplicate := decisions[0]
	duplicate.Binding.AccountID = "conflicting_tenant"
	authority.set(decisions[0], decisions[1], duplicate)
	conflictClient := clients[0].Clone()
	conflictClient.InsecureSkipVerify = true // detects either mode accepting, independent of which certificate is returned
	rejectConflict := func() {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, conflictClient)
		if err == nil {
			conn.Close()
			t.Fatal("conflicting authority reached a TLS origin or HTTPS termination")
		}
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("conflict did not close promptly")
		}
	}
	rejectConflict()
	authority.set(decisions...)
	httpsConflict.Store(true)
	rejectConflict()
	httpsConflict.Store(false)
	roundtrip(0)
	active, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, clients[0])
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	_ = active.SetReadDeadline(time.Now().Add(7 * time.Second))
	authority.set(decisions[1])
	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, clients[0]); err == nil {
		conn.Close()
		t.Fatal("removed hostname accepted")
	}
	var revokedByte [1]byte
	if _, err := active.Read(revokedByte[:]); err == nil {
		t.Fatal("revoked TLS stream stayed open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revoked TLS stream missed authority refresh deadline")
	}
	roundtrip(1)
	unknown := clients[0].Clone()
	unknown.ServerName = "unknown.customer.test"
	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, unknown); err == nil {
		conn.Close()
		t.Fatal("unknown hostname accepted")
	}
	authority.set()

	// Removing all opaque routes must retain the shared HTTPS listener.
	checkHTTPS()
	cancel()
	if err = web.Close(); err != nil {
		t.Fatal(err)
	}

	for _, listener := range originListeners {
		_ = listener.Close()
	}
	workers.Wait()
}

func TestTLSListenerRejectsConflictingTenantHostname(t *testing.T) {
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer registry.Close()
	d := publicTCPDecision(time.Now().UTC(), 443)
	d.Binding.Protocol = "tls"
	d.Binding.ListenerID = "listener_tls_443"
	other := d
	other.Binding.AccountID = "other_tenant"
	other.Binding.TunnelID = "other_tunnel"
	other.Binding.RouteID = "other_route"
	for _, conflictHTTPS := range []bool{false, true} {
		authority := &tlsAuthorityFixture{}
		if conflictHTTPS {
			authority.set(d)
		} else {
			authority.set(d, other)
		}
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listener, err := NewSharedTLSListener(raw, authority, registry, func(string) bool { return conflictHTTPS }, 4, "")
		if err != nil {
			t.Fatal(err)
		}
		client := &tls.Config{ServerName: d.Binding.Hostname, InsecureSkipVerify: true}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", raw.Addr().String(), client)
		if err == nil {
			conn.Close()
			t.Fatal("conflicting TLS mode/tenant accepted")
		}
		listener.Close()
	}
}

func TestTLSListenerAdmissionAndInterruptedCleanup(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	defer registry.Close()
	authority := &tlsAuthorityFixture{}
	d := publicTCPDecision(time.Now().UTC(), 443)
	d.Binding.Protocol = "tls"
	d.Binding.ListenerID = "listener_tls_443"
	authority.set(d)

	manager, err := NewSharedTLSListener(probe, authority, registry, func(string) bool { return false }, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	slow, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	until := time.Now().Add(time.Second)
	for len(manager.slots) != 1 {
		if time.Now().After(until) {
			t.Fatal("inspection not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	denied, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Close()
	_ = denied.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err = denied.Read(b[:]); err == nil {
		t.Fatal("excess connection accepted")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("excess connection not rejected promptly")
	}

	closed := make(chan struct{})
	go func() { manager.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shared listener cleanup timed out")
	}

	if len(manager.slots) != 0 {
		t.Fatal("inspection admission leaked after cancellation")
	}
}
