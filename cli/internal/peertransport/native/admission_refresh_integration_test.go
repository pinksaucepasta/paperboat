package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=native-admission-refresh-regression
	"tailscale.com/tstest/integration"
)

func TestNativeNewResourceRefreshAndRejectedStreamIsolation(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	// Only the dialing endpoint has observed the new operation initially.
	clientConfig.Generation++
	added := clientConfig.Peers[0].Scopes[0]
	added.ResourceID = "grant_new"
	clientConfig.Peers[0].Scopes = append(clientConfig.Peers[0].Scopes, added)
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	var refreshes atomic.Int32
	var refreshMode atomic.Int32
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS, RefreshAuthority: func(ctx context.Context) error {
		refreshes.Add(1)
		if refreshMode.Load() == 1 {
			return errors.New("refresh failed")
		}
		if refreshMode.Load() == 2 {
			return context.Canceled
		}
		serverConfig.Generation++
		scope := serverConfig.Peers[0].Scopes[0]
		scope.ResourceID = "grant_new"
		present := false
		for _, existing := range serverConfig.Peers[0].Scopes {
			present = present || existing.ResourceID == scope.ResourceID
		}
		if !present {
			serverConfig.Peers[0].Scopes = append(serverConfig.Peers[0].Scopes, scope)
		}
		applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	blockedStarted := make(chan struct{})
	blockedRelease := make(chan struct{})
	releaseBlocked := sync.OnceFunc(func() { close(blockedRelease) })
	defer releaseBlocked()
	var temporaryCalls atomic.Int32
	var acceptedSessions atomic.Int32
	apps, err := nativesession.New(nativesession.Config{Authorize: func(admitCtx context.Context, h streamauth.Header) (string, error) {
		if h.Credential == "blocked" {
			close(blockedStarted)
			select {
			case <-blockedRelease:
				return "grant_test", nil
			case <-admitCtx.Done():
				return "", admitCtx.Err()
			}
		}
		if h.Credential == "starting" {
			if temporaryCalls.Add(1) == 1 {
				return "", &native.AdmissionRefusal{Code: native.AdmissionUnavailable, Err: errors.New("worker starting")}
			}
			return "grant_test", nil
		}
		if h.Credential == "invalid" {
			return "", errors.New("denied")
		}
		return h.Credential, nil
	}, ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error { _, err := io.Copy(c, c); return err }, ServeTransfer: func(context.Context, net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	server, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serverOwner.Listen(ctx, dm.Regions[1], func(ctx context.Context, session *native.Session) error {
			acceptedSessions.Add(1)
			return apps.Serve(ctx, session)
		})
	}()
	defer func() { cancel(); serverOwner.Close(); <-done; apps.Wait() }()
	session, err := clientOwner.Dial(ctx, server.Address(), "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	rawOpen := func(id, credential, resource string) (net.Conn, error) {
		h, e := streamauth.New(id, "terminal", id, credential, time.Now().Add(time.Minute), 1<<20)
		if e != nil {
			t.Fatal(e)
		}
		return session.OpenAuthorized(ctx, h, resource, "terminal")
	}
	deny := func(id, credential, resource string) {
		t.Helper()
		c, e := rawOpen(id, credential, resource)
		if c != nil {
			c.Close()
		}
		var refusal *native.AdmissionRefusal
		if !errors.As(e, &refusal) || refusal.Code != native.AdmissionDenied {
			t.Error("admission did not return a typed permanent refusal")
		}
	}
	open := func(id, credential, resource string) net.Conn {
		t.Helper()
		c, e := rawOpen(id, credential, resource)
		if e != nil {
			t.Fatal(e)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}
	echo := func(c net.Conn) {
		t.Helper()
		if e := c.SetDeadline(time.Now().Add(3 * time.Second)); e != nil {
			t.Fatal(e)
		}
		if _, e := c.Write([]byte("x")); e != nil {
			t.Fatal(e)
		}
		var b [1]byte
		if _, e := io.ReadFull(c, b[:]); e != nil || b[0] != 'x' {
			t.Fatalf("echo failed: %v", e)
		}
	}
	existing := open("existing", "grant_test", "grant_test")
	defer existing.Close()
	echo(existing)
	deny("bad", "invalid", "grant_test")
	var b [1]byte
	starting := open("starting", "starting", "grant_test")
	echo(starting)
	starting.Close()
	if temporaryCalls.Load() < 2 || acceptedSessions.Load() != 1 {
		t.Fatal("temporary admission refusal did not recover on the same QUIC connection")
	}
	blockedHeader, e := streamauth.New("blocked", "terminal", "blocked", "blocked", time.Now().Add(time.Minute), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	blockedCtx, cancelBlocked := context.WithCancel(ctx)
	defer cancelBlocked()
	type openResult struct {
		conn net.Conn
		err  error
	}
	blockedDone := make(chan openResult, 1)
	go func() {
		c, e := session.OpenAuthorized(blockedCtx, blockedHeader, "grant_test", "terminal")
		blockedDone <- openResult{c, e}
	}()
	select {
	case <-blockedStarted:
	case <-ctx.Done():
		t.Fatal("blocked authorization never reached target")
	}
	cancelBlocked()
	select {
	case result := <-blockedDone:
		if result.conn != nil {
			result.conn.Close()
		}
		releaseBlocked()
		if !errors.Is(result.err, context.Canceled) {
			t.Fatal("pending admission did not preserve caller cancellation")
		}
	case <-time.After(time.Second):
		releaseBlocked()
		t.Fatal("pending admission did not cancel promptly")
	}
	echo(existing)
	if acceptedSessions.Load() != 1 {
		t.Fatal("cancelling admission replaced the QUIC connection")
	}

	if refreshes.Load() != 0 {
		t.Fatal("invalid credential triggered refresh")
	}
	echo(existing)
	fresh := open("fresh", "grant_new", "grant_new")
	defer fresh.Close()
	echo(fresh)
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d", refreshes.Load())
	}
	for _, mode := range []int32{0, 1, 2} {
		refreshMode.Store(mode)
		deny("denied", "not_in_current_scope", "grant_test")
		echo(existing)
	}
	if refreshes.Load() != 4 {
		t.Fatalf("refresh attempts=%d", refreshes.Load())
	}
	echo(existing)
	// Invalid or incomplete admission belongs to that stream, not every
	// authorized application already sharing its QUIC connection.
	for _, kind := range []string{"expired", "malformed", "unknown_consumer", "incomplete"} {
		h, e := streamauth.New("bad_header", "terminal", "bad_header", "grant_test", time.Now().Add(time.Minute), 1<<20)
		if e != nil {
			t.Fatal(e)
		}
		if kind == "expired" {
			h.DeadlineUnix = time.Now().Add(-time.Second).Unix()
		}
		if kind == "unknown_consumer" {
			h.Consumer = "unsupported"
		}
		encoded, e := json.Marshal(h)
		if e != nil {
			t.Fatal(e)
		}
		if kind == "malformed" {
			encoded = []byte("not-json-frame")
		}
		length := len(encoded)
		if kind == "incomplete" {
			length += 10
		}
		raw, e := session.Open(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if e = raw.SetDeadline(time.Now().Add(12 * time.Second)); e != nil {
			t.Fatal(e)
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(length))
		if _, e = raw.Write(append(size[:], encoded...)); e != nil {
			t.Fatal(e)
		}
		_, readErr := raw.Read(b[:])
		raw.Close()
		if readErr == nil {
			t.Fatalf("%s header accepted", kind)
		}
		if timeout, ok := readErr.(net.Error); ok && timeout.Timeout() {
			t.Fatalf("%s header admission was not bounded", kind)
		}
		echo(existing)
	}
	// Ending one operation must revoke its stream without dropping another
	// still-authorized operation sharing the same native peer connection.
	serverConfig.Generation++
	serverConfig.Peers[0].Scopes = serverConfig.Peers[0].Scopes[:1]
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	if err := existing.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	echo(existing)
	if err := fresh.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Read(b[:]); err == nil {
		t.Fatal("withdrawn operation retained its stream")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("withdrawn stream was not actively closed")
	}
}
