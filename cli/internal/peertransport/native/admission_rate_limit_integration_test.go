package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=native-admission-rate-limit-regression
	"tailscale.com/tstest/integration"
)

// rateLimitedControl is the machine peer-network endpoint as the server runs it: a per-machine token bucket that
// answers 429 with a Retry-After (router_02.go), and a configuration holding every grant issued so far. The bucket
// is counted in requests, not time (every third request is refused, the first included), so the number of 429s a
// run sees does not depend on how fast the host is. gate, while set, holds every request until it is closed.
type rateLimitedControl struct {
	signer   ed25519.PrivateKey
	mu       sync.Mutex
	config   tailnet.NetworkConfiguration
	gate     chan struct{}
	requests atomic.Int32
	limited  atomic.Int32
}

func (c *rateLimitedControl) issue(resource string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := c.config.Peers[0].Scopes[0]
	scope.ResourceID = resource
	c.config.Peers[0].Scopes = append(c.config.Peers[0].Scopes, scope)
}

func (c *rateLimitedControl) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, errors.New("not used")
}

func (c *rateLimitedControl) PeerNetworkConfiguration(ctx context.Context, _ api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	call := c.requests.Add(1)
	c.mu.Lock()
	gate := c.gate
	c.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return api.PeerNetworkConfigurationResult{}, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if call%3 == 1 {
		c.limited.Add(1)
		return api.PeerNetworkConfigurationResult{}, &api.APIError{Status: 429, Code: "rate_limited", RetryAfter: 20 * time.Millisecond}
	}
	c.config.Generation++
	return api.PeerNetworkConfigurationResult{Configuration: testConfigurationToken(c.config, c.signer)}, nil
}

// A machine whose peer-network budget other engines already spent (pbvm4 feat9, after an update: old and new
// carrier generations on one machine credential) keeps admitting a loop of new operations: none is refused, an
// open stream is never blocked behind a renewal, and a burst of operations shares renewals instead of sending one
// each. Before, the first 429 refused the stream and held every admission for the 12 s refill, so 20 MB sends took
// 11.8 s. No assertion measures wall-clock time (that failed at 1.72 s under load); the hard timeouts of open and
// echo bound a hang.
func TestNewOperationsAdmitThroughAMachineRateLimit(t *testing.T) {
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
	const groups, perGroup = 20, 3
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	for i := range groups * perGroup {
		scope := clientConfig.Peers[0].Scopes[0]
		scope.ResourceID = fmt.Sprintf("grant_%d", i)
		clientConfig.Peers[0].Scopes = append(clientConfig.Peers[0].Scopes, scope)
	}
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	// Other engines already spent the burst: the first admission renewal is refused with a 429.
	control := &rateLimitedControl{signer: signerPrivate, config: serverConfig}

	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS, RefreshAuthority: func(ctx context.Context) error {
		return serverAuthority.RefreshForAdmission(ctx, control)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	apps, err := nativesession.New(nativesession.Config{
		Authorize:     func(_ context.Context, h streamauth.Header) (string, error) { return h.Credential, nil },
		ServeStream:   func(_ context.Context, _ streamauth.Header, c net.Conn) error { _, err := io.Copy(c, c); return err },
		ServeTransfer: func(context.Context, net.Conn) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serverOwner.Listen(ctx, dm.Regions[1], func(ctx context.Context, session *native.Session) error { return apps.Serve(ctx, session) })
	}()
	defer func() { cancel(); serverOwner.Close(); <-done; apps.Wait() }()
	session, err := clientOwner.Dial(ctx, server.Address(), "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	open := func(id, resource string) (net.Conn, error) {
		h, err := streamauth.New(id, "terminal", id, resource, time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			return nil, err
		}
		openCtx, cancelOpen := context.WithTimeout(ctx, 5*time.Second)
		defer cancelOpen()
		return session.OpenAuthorized(openCtx, h, resource, "terminal")
	}
	echo := func(c net.Conn) error {
		if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			return err
		}
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		var b [1]byte
		_, err := io.ReadFull(c, b[:])
		return err
	}

	existing, err := open("existing", "grant_test")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	var rejected atomic.Int64
	var wg sync.WaitGroup
	// A renewal held by the control (a slow or rate-limited server) must not block an open stream: while the
	// first group's operations wait for it, the existing stream keeps answering.
	hold := make(chan struct{})
	control.mu.Lock()
	control.gate = hold
	control.mu.Unlock()
	for g := range groups {
		for i := range perGroup {
			resource := fmt.Sprintf("grant_%d", g*perGroup+i)
			control.issue(resource) // the operation is committed before its client dials
			wg.Go(func() {
				c, err := open(resource, resource)
				if err == nil {
					err = echo(c)
					c.Close()
				}
				if err != nil {
					rejected.Add(1)
					t.Logf("%s: %v", resource, err)
				}
			})
		}
		if g == 0 {
			for range 5 {
				if err := echo(existing); err != nil {
					t.Fatalf("open stream blocked behind a held renewal: %v", err)
				}
			}
			close(hold)
		} else if err := echo(existing); err != nil {
			t.Fatalf("open stream failed during the dial loop: %v", err)
		}
	}
	wg.Wait()
	t.Logf("%d dials: rejected=%d; %d renewals, %d answered 429", groups*perGroup, rejected.Load(), control.requests.Load(), control.limited.Load())
	if rejected.Load() != 0 {
		t.Fatalf("%d new operations refused", rejected.Load())
	}
	if control.limited.Load() == 0 {
		t.Fatal("the control never rate limited; the test does not reproduce the trigger")
	}
	// Admissions whose stream arrived before a renewal started share it: a burst is one request, not one each.
	if requests := int(control.requests.Load()); requests > groups*perGroup/2 {
		t.Fatalf("%d renewals for %d operations: admissions are not sharing renewals", requests, groups*perGroup)
	}
}
