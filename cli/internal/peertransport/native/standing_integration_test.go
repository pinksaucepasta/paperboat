package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tstest/integration"
)

// A terminal or exec whose credential checks out is admitted from the host's standing rule for the peer (resource =
// the host itself), so a new operation costs the host no config refresh and no server request; a capability without
// a standing rule still refreshes once, and withdrawing the rule closes a running stream.
func TestStandingRuleAdmitsExecWithoutHostRefresh(t *testing.T) {
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
	// The host knows no per-operation scope for the peer, only its standing exec rule.
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{
		{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "exec", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: 0},
		// The owner's standing inspector rule (server: owner only): pb preview inspect asked one host refresh per
		// request, which a budgeted host could not give in time (hp 2026-10-27, 10 of 10 end_of_stream).
		{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "inspector", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: 0},
	}
	clientConfig.Peers[0].Scopes = append(clientConfig.Peers[0].Scopes, tailnet.NetworkScope{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "inspector", Direction: "dial", Port: tailnet.NetworkPort, ExpiresAt: now + 300})
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	var refreshes atomic.Int32
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS, RefreshAuthority: func(context.Context) error {
		refreshes.Add(1)
		return errors.New("host refresh must not be needed")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	server, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go func() {
		_ = serverOwner.Listen(ctx, dm.Regions[1], func(serveCtx context.Context, session *native.Session) error {
			for {
				stream, _, err := session.AcceptAuthorized(serveCtx, func(context.Context, streamauth.Header) (string, error) {
					return "operation_never_published", nil // a fresh access session the host has no scope for
				})
				if errors.Is(err, native.ErrStreamRejected) {
					continue
				}
				if err != nil {
					return err
				}
				go func() { defer stream.Close(); _, _ = io.Copy(stream, stream) }()
			}
		})
	}()
	session, err := clientOwner.Dial(ctx, server.Address(), "machine_host", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	open := func(consumer string) (net.Conn, error) {
		header, err := streamauth.New("op_"+consumer, consumer, "stream_"+consumer, "credential", time.Now().Add(time.Minute), 1<<20)
		if consumer == "inspector" { // the shape tunnel.InspectorRequest sends
			target := []byte(`{"action":"inspect","resource_id":"preview_test","resource_kind":"preview","route_id":"route_test"}`)
			header, err = streamauth.NewNativePrivate("credential_inspector", "inspector", "inspector_request", "grant", time.Now().Add(time.Minute), 1<<20, target)
		}
		if err != nil {
			t.Fatal(err)
		}
		return session.OpenAuthorized(ctx, header, "grant_test", consumer)
	}
	echo := func(stream net.Conn) error {
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := stream.Write([]byte("x")); err != nil {
			return err
		}
		var b [1]byte
		_, err := io.ReadFull(stream, b[:])
		return err
	}

	var held net.Conn
	for i := 0; i < 5; i++ {
		stream, err := open("exec")
		if err != nil {
			t.Fatal(err)
		}
		if err := echo(stream); err != nil {
			t.Fatalf("exec %d under the standing rule: %v", i, err)
		}
		if i == 4 {
			held = stream
		} else {
			stream.Close()
		}
	}
	if refreshes.Load() != 0 {
		t.Fatalf("host refreshed %d times for 5 execs", refreshes.Load())
	}
	for i := 0; i < 3; i++ {
		stream, err := open("inspector")
		if err != nil {
			t.Fatal(err)
		}
		if err := echo(stream); err != nil {
			t.Fatalf("inspector %d under the standing rule: %v", i, err)
		}
		stream.Close()
	}
	if refreshes.Load() != 0 {
		t.Fatalf("host refreshed %d times for 3 inspector requests", refreshes.Load())
	}

	// No standing rule for terminal: the old path, one refresh, which here fails.
	terminal, err := open("terminal")
	if err != nil {
		t.Fatal(err)
	}
	if err := echo(terminal); err == nil {
		t.Fatal("terminal admitted without a scope")
	}
	terminal.Close()
	if refreshes.Load() != 1 {
		t.Fatalf("terminal refreshes=%d, want 1", refreshes.Load())
	}

	// Withdrawing the standing rule ends the running exec within seconds.
	serverConfig.Generation++
	// (a peer must keep some scope in a valid document: an unrelated resource stays)
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "other_machine", ResourceGeneration: 1, Capability: "exec", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: 0}}
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	_ = held.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	var b [1]byte
	if _, err := held.Read(b[:]); err == nil {
		t.Fatal("running exec survived the withdrawal of its standing rule")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("withdrawal took %s to end the stream", elapsed)
	}
	held.Close()
}
