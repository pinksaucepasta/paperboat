package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

// A peer on this machine answers a handshake in milliseconds, so a silent first flight (its carrier is replaced by an
// update) is worth 300 ms, not a second. The updated daemon is a new process with no restart mark for the peer, so the
// wait cannot depend on one: on the pbvm3 pair every failed handshake of the held terminal's redial cost the full second.
func TestSameHostPeerGivesASilentFirstFlightOnlyTheShortWait(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	regions := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	public, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	client := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	machine := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	sender := testLocalAuthority(t, client, testKeys{"native_test": public}, "machine_test")
	receiver := testAuthority(t, machine, testKeys{"native_test": public})
	client.WireGuardPublicKey, client.KeyGeneration = prepareBinding(t, sender)
	machine.WireGuardPublicKey, machine.KeyGeneration = prepareBinding(t, receiver)
	now := time.Now().Unix()
	applyTestConfiguration(t, sender, signer, testConfiguration(now, 1, client, machine, "dial"))
	applyTestConfiguration(t, receiver, signer, testConfiguration(now, 1, machine, client, "accept"))
	serverTLS.ClientAuth = tls.RequireAnyClientCert
	serverTLS.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 {
			return errors.New("missing fixture client certificate")
		}
		fingerprint := sha256.Sum256(state.PeerCertificates[0].Raw)
		if hex.EncodeToString(fingerprint[:]) != clientFingerprint {
			return errors.New("wrong fixture client certificate")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	address, flows, _, stop := listenWithLostHandshake(t, ctx, receiver, regions.Regions[1], serverTLS, 1)
	defer stop()
	owner, err := native.NewOwner(native.Config{Authority: sender, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// The mechanism under test is how long the dialer waits on a silent first flight, not how long the retry's
	// own handshake takes (a loaded -race runner stretches that past the escalated wait and costs a third
	// attempt): the gap between the first and second flights reaching the listener is that wait.
	arrivals := make(chan time.Time, 2)
	go func() {
		for seen := int32(0); seen < 2 && ctx.Err() == nil; time.Sleep(time.Millisecond) {
			for ; flows.Load() > seen && seen < 2; seen++ {
				arrivals <- time.Now()
			}
		}
	}()
	started := time.Now()
	session, err := owner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	t.Logf("same-host dial with one lost first flight took %v", time.Since(started))
	var first, second time.Time
	for _, at := range []*time.Time{&first, &second} {
		select {
		case *at = <-arrivals:
		case <-ctx.Done():
			t.Fatal("the listener did not see a first and a second handshake flight")
		}
	}
	// 300 ms short wait against the 1 s normal timeout: the midpoint separates them with room for timer jitter.
	if wait := second.Sub(first); wait > 650*time.Millisecond {
		t.Fatalf("the retry followed the silent first flight after %v: it waited the normal timeout for a peer on this machine", wait)
	}
}
