package connectorprotocol

import (
	"context"
	"sync"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type stagedConfig struct {
	owner     *stagedApplier
	activated bool
	aborted   bool
}

func (s *stagedConfig) Activate(ctx context.Context) error {
	s.owner.mu.Lock()
	activateErr := s.owner.activateErr
	activateStarted := s.owner.activateStarted
	activateRelease := s.owner.activateRelease
	s.owner.mu.Unlock()
	if activateStarted != nil {
		s.owner.activateStartedOnce.Do(func() { close(activateStarted) })
	}
	if activateRelease != nil {
		select {
		case <-activateRelease:
		case <-ctx.Done():
		}
	}
	s.owner.mu.Lock()
	if activateErr != nil {
		s.owner.mu.Unlock()
		return activateErr
	}
	s.activated = true
	s.owner.mu.Unlock()
	if s.owner.events != nil {
		s.owner.events <- "activation_done"
	}
	return nil
}

func (s *stagedConfig) Abort(context.Context) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.aborted = true
	return s.owner.abortErr
}

type stagedApplier struct {
	mu                  sync.Mutex
	prepareErr          error
	activateErr         error
	abortErr            error
	prepared            []*stagedConfig
	activateStarted     chan struct{}
	activateRelease     chan struct{}
	activateStartedOnce sync.Once
	events              chan string
}

type testDrainer struct {
	mu         sync.Mutex
	active     uint32
	stopCalls  int
	forceCalls int
	stopErr    error
	activeErr  error
	forceErr   error
}

func (d *testDrainer) StopNewStreams(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopCalls++
	return d.stopErr
}

func (d *testDrainer) ActiveStreams(context.Context) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active, d.activeErr
}

func (d *testDrainer) ForceClose(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.forceCalls++
	if d.forceErr == nil {
		d.active = 0
	}
	return d.forceErr
}

func (a *stagedApplier) PrepareSnapshot(context.Context, wire.Snapshot) (wire.PreparedConfig, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prepareErr != nil {
		return nil, a.prepareErr
	}
	prepared := &stagedConfig{owner: a}
	a.prepared = append(a.prepared, prepared)
	return prepared, nil
}

func (a *stagedApplier) PrepareDelta(ctx context.Context, delta wire.Delta) (wire.PreparedConfig, error) {
	return a.PrepareSnapshot(ctx, wire.Snapshot{Generation: delta.Generation})
}

func testSessionPair(t *testing.T) (*wire.ServerSession, *wire.ClientSession, *testClock, *stagedApplier, wire.Snapshot) {
	t.Helper()
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	clock := &testClock{now: now}
	auth, private := testAuth(t, now, 1)
	snapshot, err := wire.NewSnapshot("tunnel_1", 1, testConfigPayload(1, "preview.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := wire.NewServer(wire.ServerConfig{
		Authenticator: wire.AuthenticatorFuncs{AuthenticateFunc: func(context.Context, wire.AuthRequest) (wire.AuthResult, error) {
			return wire.AuthResult{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, IdentityKeyID: auth.IdentityKeyID, IdentityKeyThumbprint: auth.IdentityKeyThumbprint, ProcessGeneration: auth.ProcessGeneration, CredentialGeneration: auth.CredentialGeneration, CredentialExpiresAt: now.Add(time.Hour), LeaseExpiresAt: now.Add(time.Hour)}, nil
		}, RenewFunc: func(context.Context, wire.RenewalRequest) (wire.AuthResult, error) {
			return wire.AuthResult{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, IdentityKeyID: auth.IdentityKeyID, IdentityKeyThumbprint: auth.IdentityKeyThumbprint, ProcessGeneration: auth.ProcessGeneration, CredentialGeneration: auth.CredentialGeneration + 1, CredentialExpiresAt: clock.Now().Add(time.Hour), LeaseExpiresAt: clock.Now().Add(time.Hour)}, nil
		}},
		Snapshots:    wire.SnapshotSourceFunc(func(context.Context, string) (wire.Snapshot, error) { return snapshot, nil }),
		Capabilities: requiredCapabilityList(), Clock: clock, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second,
		SessionIDs: func() (string, error) { return "sess_1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	serverSession, welcome, first, err := server.Accept(context.Background(), wire.Hello{Protocol: wire.ProtocolName, MinVersion: "1.0", MaxVersion: "1.0", AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, ProcessGeneration: auth.ProcessGeneration, Capabilities: requiredCapabilityList(), Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != snapshot.ContentHash || welcome.Lease.SessionID != welcome.SessionID {
		t.Fatal("server did not return the bound first snapshot")
	}
	applier := &stagedApplier{}
	client, err := wire.NewClientSession(wire.ClientSessionConfig{Hello: wire.Hello{Protocol: wire.ProtocolName, MinVersion: "1.0", MaxVersion: "1.0", AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, ProcessGeneration: auth.ProcessGeneration, Capabilities: requiredCapabilityList(), Auth: auth}, Applier: applier, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AcceptWelcome(welcome); err != nil {
		t.Fatal(err)
	}
	ack, err := client.ApplySnapshot(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if err := serverSession.HandleAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	readiness, err := client.MarkReady(true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverSession.HandleReadiness(context.Background(), readiness); err != nil {
		t.Fatal(err)
	}
	if _, ready, _ := serverSession.Current(); !ready {
		t.Fatal("initial session is not ready")
	}
	_ = private
	return serverSession, client, clock, applier, snapshot
}

func testPendingSessionPair(t *testing.T) (*wire.ServerSession, *wire.ClientSession, *testClock, wire.Snapshot) {
	t.Helper()
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	clock := &testClock{now: now}
	auth, _ := testAuth(t, now, 1)
	snapshot, err := wire.NewSnapshot("tunnel_1", 1, testConfigPayload(1, "preview.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := wire.NewServer(wire.ServerConfig{
		Authenticator: wire.AuthenticatorFuncs{AuthenticateFunc: func(context.Context, wire.AuthRequest) (wire.AuthResult, error) {
			return wire.AuthResult{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, IdentityKeyID: auth.IdentityKeyID, IdentityKeyThumbprint: auth.IdentityKeyThumbprint, ProcessGeneration: auth.ProcessGeneration, CredentialGeneration: auth.CredentialGeneration, CredentialExpiresAt: now.Add(time.Hour), LeaseExpiresAt: now.Add(time.Hour)}, nil
		}},
		Snapshots:    wire.SnapshotSourceFunc(func(context.Context, string) (wire.Snapshot, error) { return snapshot, nil }),
		Capabilities: requiredCapabilityList(), Clock: clock, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second,
		SessionIDs: func() (string, error) { return "sess_pending", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	serverSession, welcome, first, err := server.Accept(context.Background(), wire.Hello{Protocol: wire.ProtocolName, MinVersion: "1.0", MaxVersion: "1.0", AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, ProcessGeneration: auth.ProcessGeneration, Capabilities: requiredCapabilityList(), Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	applier := &stagedApplier{}
	client, err := wire.NewClientSession(wire.ClientSessionConfig{Hello: wire.Hello{Protocol: wire.ProtocolName, MinVersion: "1.0", MaxVersion: "1.0", AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, ProcessGeneration: auth.ProcessGeneration, Capabilities: requiredCapabilityList(), Auth: auth}, Applier: applier, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AcceptWelcome(welcome); err != nil {
		t.Fatal(err)
	}
	ack, err := client.ApplySnapshot(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if err := serverSession.HandleAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if serverSession.State() != wire.SessionAwaitingReady || client.State() != wire.SessionAwaitingReady {
		t.Fatalf("pending states server=%s client=%s", serverSession.State(), client.State())
	}
	return serverSession, client, clock, snapshot
}
