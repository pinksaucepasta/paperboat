package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"io"
	"net"
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
	apps, err := nativesession.New(nativesession.Config{Authorize: func(_ context.Context, h streamauth.Header) (string, error) {
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
	go func() { defer close(done); _ = serverOwner.Listen(ctx, dm.Regions[1], apps.Serve) }()
	defer func() { cancel(); serverOwner.Close(); <-done; apps.Wait() }()
	session, err := clientOwner.Dial(ctx, server.Address(), "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	open := func(id, credential, resource string) net.Conn {
		t.Helper()
		h, e := streamauth.New(id, "terminal", id, credential, time.Now().Add(time.Minute), 1<<20)
		if e != nil {
			t.Fatal(e)
		}
		c, e := session.OpenAuthorized(ctx, h, resource, "terminal")
		if e != nil {
			t.Fatal(e)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}
	echo := func(c net.Conn) {
		t.Helper()
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
	bad := open("bad", "invalid", "grant_test")
	var b [1]byte
	_, err = bad.Read(b[:])
	bad.Close()
	if err == nil {
		t.Fatal("invalid credential accepted")
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
		denied := open("denied", "not_in_current_scope", "grant_test")
		_, readErr := denied.Read(b[:])
		denied.Close()
		if readErr == nil {
			t.Fatal("resource absent from current authority accepted")
		}
		echo(existing)
	}
	if refreshes.Load() != 4 {
		t.Fatalf("refresh attempts=%d", refreshes.Load())
	}
	echo(existing)
}
