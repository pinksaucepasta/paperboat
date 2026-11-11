package tailnet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=regional-recovery-test
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=regional-recovery-test
	"tailscale.com/types/key"
)

func TestRegionalRecoveryRecreatedAfterAuthorityDrop(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	old := &regionalRecovery{authority: a, control: newRegionalControl(a.private, time.Now), cancel: cancelWorker, done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 64)}
	a.recoveryWorkers.Add(1)
	go func() {
		defer a.recoveryWorkers.Done()
		defer close(old.done)
		<-workerCtx.Done()
	}()
	a.relay.mu.Lock()
	a.relay.regionalRecoveryConfigured = true
	a.relay.recovery = old
	a.relay.mu.Unlock()

	a.mu.Lock()
	a.dropLocked()
	a.mu.Unlock()
	select {
	case <-old.done:
	case <-time.After(time.Second):
		t.Fatal("detached regional recovery worker did not stop")
	}
	if _, err := a.Descriptor(configuration.Peers[0].Identity.EndpointID); !errors.Is(err, ErrAuthority) {
		t.Fatalf("dropped authority admitted descriptor: %v", err)
	}
	if err := a.PrepareRegional(t.Context(), ""); !errors.Is(err, ErrAuthority) {
		t.Fatalf("dropped authority prepared regional recovery: %v", err)
	}
	a.relay.mu.Lock()
	retained := a.relay.recovery
	a.relay.mu.Unlock()
	if retained != nil {
		t.Fatal("dropped authority retained newly prepared recovery")
	}

	configuration.Generation++
	configuration.IssuedAt = time.Now().Unix()
	configuration.ExpiresAt = configuration.IssuedAt + 300
	configuration.Peers[0].Scopes[0].ExpiresAt = configuration.ExpiresAt
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.clientEngine = &mesh.Server{}
	a.mu.Unlock()
	if err := a.PrepareRegional(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	a.relay.mu.Lock()
	fresh, configured := a.relay.recovery, a.relay.regionalRecoveryConfigured
	a.relay.mu.Unlock()
	if !configured || fresh == nil || fresh == old {
		t.Fatal("fresh authority reused stopped regional recovery")
	}
	select {
	case <-fresh.done:
		t.Fatal("fresh regional recovery starts stopped")
	default:
	}
	if fresh.control == old.control || !fresh.control.private.Equal(a.private) {
		t.Fatal("fresh regional recovery reused old control authority")
	}
}

func TestRegionalControlHookPreservesApplicationAndBootstrapPackets(t *testing.T) {
	r := &regionalRecovery{inbox: make(chan regionalInbound, 1)}
	a := &Authority{relay: relayAuthority{recovery: r}}
	for _, packet := range [][]byte{{1, 0, 0, 0, 9}, []byte("meow\x02"), []byte("unrecognized")} {
		if a.relayControl(1, key.NewNode().Public(), packet) {
			t.Fatal("unrelated packet consumed by regional metadata hook")
		}
	}
	if len(r.inbox) != 0 {
		t.Fatal("application traffic queued as control")
	}
	p := append(append([]byte(nil), regionalControlMagic...), 1)
	if !a.relayControl(1, key.NewNode().Public(), p) || len(r.inbox) != 1 {
		t.Fatal("regional metadata not routed")
	}
	// A full queue drops metadata instead of blocking the encrypted packet reader.
	if !a.relayControl(1, key.NewNode().Public(), p) || len(r.inbox) != 1 {
		t.Fatal("control queue bound changed")
	}
}

func TestRegionalRetryBoundsAndProcessReset(t *testing.T) {
	n := RegionalNode{NodeGeneration: 1, ProcessEpoch: "first"}
	now := time.Unix(100, 0)
	var r regionalRetry
	for _, delay := range []time.Duration{3, 6, 12, 12, 12} {
		r = r.failed(n, now)
		if got := r.next.Sub(now); got != delay*time.Second {
			t.Fatalf("retry delay %v, want %vs", got, delay)
		}
	}
	n.ProcessEpoch = "replacement"
	r = r.failed(n, now)
	if r.next.Sub(now) != 3*time.Second {
		t.Fatal("replacement process inherited old failure backoff")
	}
}

type failingRegionalEngine struct{ cause error }

func (*failingRegionalEngine) SetRelayRegions([]*tailcfg.DERPRegion) error { return nil }
func (*failingRegionalEngine) SetPeerRelayNodes([]*tailcfg.Node) error     { return nil }
func (*failingRegionalEngine) RelayTransport(tailcfg.DERPRegionID) string  { return "direct" }
func (e *failingRegionalEngine) PrepareRelay(context.Context, tailcfg.DERPRegionID) error {
	return e.cause
}
func (*failingRegionalEngine) OnNetworkChange(func()) func() { return func() {} }
func (*failingRegionalEngine) ReSTUN(string)                 {}
func (*failingRegionalEngine) SetPeerRelayRegion(key.NodePublic, tailcfg.DERPRegionID) error {
	return nil
}
func (*failingRegionalEngine) SendRelayControl(key.NodePublic, tailcfg.DERPRegionID, []byte) error {
	return nil
}

func TestRegionalStatusDoesNotExposePreparationErrorText(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{
		NodeID: "relay_test", NodeGeneration: 1, ProcessEpoch: "epoch_test", Region: "test", FailureDomain: "domain_test",
		Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: 443,
		State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now,
	}
	a.regional = &RegionalCandidates{IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
	a.relay.mu.Lock()
	a.relay.grants = map[string]peernetwork.RelayGrant{node.NodeID: {NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, ExpiresAt: now + 60}}
	r := &regionalRecovery{
		authority: a, control: newRegionalControl(a.private, time.Now), engine: &failingRegionalEngine{cause: errors.New("private relay hostname and token")},
		peerID: "machine_test", done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 64),
	}
	a.relay.recovery = r
	a.relay.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	go r.run(ctx)
	defer cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := a.RegionalStatus()
		if status.Reason == "no_mutually_reachable_node" {
			if got := status.ProbeFailures[node.NodeID]; got != "prepare_failed" {
				t.Fatalf("preparation status=%q", got)
			}
			if strings.Contains(status.ProbeFailures[node.NodeID], "private relay") || strings.Contains(status.ProbeFailures[node.NodeID], "token") {
				t.Fatalf("status exposed transport text: %+v", status)
			}
			cancel()
			select {
			case <-r.done:
			case <-time.After(time.Second):
				t.Fatal("regional recovery worker did not stop after cancellation")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("regional status did not report failed preparation: %+v", a.RegionalStatus())
}

// A recovery with no bound pair has no relay path to report: pb status must not warn "No relay is usable"
// for an engine whose dials all went direct.
func TestRegionalStatusWithNoBoundPairReportsNothing(t *testing.T) {
	a := &Authority{}
	r := &regionalRecovery{authority: a, status: RegionalStatus{Redundancy: RedundancyNone, Reason: "no_mutually_reachable_node"}}
	a.relay.recovery = r
	if got := a.RegionalStatus(); got.Reason != "" || got.NodeID != "" {
		t.Fatalf("unbound recovery reported %+v", got)
	}
	r.peerID = "machine_test"
	if got := a.RegionalStatus(); got.Reason != "no_mutually_reachable_node" {
		t.Fatalf("bound recovery lost its status: %+v", got)
	}
}

// Regional recovery follows the pair last dialed successfully: it stayed on the first peer ever prepared and
// pb status warned about that pair while the machines in use reached their relays (hp 2026-10-27).
func TestRegionalRecoveryFollowsTheLastDialedPair(t *testing.T) {
	a := &Authority{}
	r := &regionalRecovery{authority: a, engine: &failingRegionalEngine{cause: errors.New("unused")}, peerID: "machine_first"}
	a.relay.recovery = r
	a.BindRegionalPeer("machine_in_use")
	if got := r.pair(); got != "machine_in_use" {
		t.Fatalf("recovery pair = %q after a dial to machine_in_use", got)
	}
	idle := &regionalRecovery{authority: a}
	a.relay.recovery = idle
	a.BindRegionalPeer("machine_in_use") // not started yet: PrepareRegional binds it
	if got := idle.pair(); got != "" {
		t.Fatalf("unstarted recovery bound to %q", got)
	}
}

// An unusable relay state shows only once it has lasted regionalWarningDelay (Tailscale's TimeToVisible): a
// probe round between relays is not a warning.
func TestUnusableRelayIsReportedAfterTheWarningDelay(t *testing.T) {
	previous := regionalWarningDelay
	regionalWarningDelay = 200 * time.Millisecond
	t.Cleanup(func() { regionalWarningDelay = previous })
	a := &Authority{}
	r := &regionalRecovery{authority: a, peerID: "machine_test", status: RegionalStatus{Redundancy: RedundancyNone, Reason: "regional_admission_denied"}}
	a.relay.recovery = r
	if got := a.RegionalStatus(); got.Reason != "" {
		t.Fatalf("unusable relay reported at once: %+v", got)
	}
	time.Sleep(250 * time.Millisecond)
	if got := a.RegionalStatus(); got.Reason != "regional_admission_denied" {
		t.Fatalf("unusable relay not reported after the delay: %+v", got)
	}
	r.mu.Lock()
	r.status = RegionalStatus{NodeID: "relay_a", Reason: "healthy_choice_retained"}
	r.mu.Unlock()
	_ = a.RegionalStatus()
	r.mu.Lock()
	r.status = RegionalStatus{Redundancy: RedundancyNone, Reason: "regional_admission_denied"}
	r.mu.Unlock()
	if got := a.RegionalStatus(); got.Reason != "" {
		t.Fatalf("a new unusable period was reported without its own delay: %+v", got)
	}
}

// A direct contact is already authorized by the signed network. Regional
// rendezvous continues independently, including while its relay is unavailable.
func TestDirectPeerDoesNotWaitForRegionalRendezvous(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Peers[0].Endpoints = []string{"192.0.2.10:41642"}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	engine := &mesh.Server{}
	a.mu.Lock()
	a.clientEngine = engine
	a.mu.Unlock()
	a.relay.mu.Lock()
	a.relay.regionalRecoveryConfigured = true
	recovery := a.newRegionalRecoveryLocked()
	recovery.engine = engine
	a.relay.recovery = recovery
	a.relay.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := a.PrepareRegional(ctx, cfg.Peers[0].Identity.EndpointID); err != nil {
		t.Fatal("authorized direct peer waited for regional rendezvous", err)
	}
}

func TestDeniedFenceBacksOffToTheCapLogsOnceAndClearsOnRecovery(t *testing.T) {
	var f deniedFence
	now := time.Unix(1000, 0)
	if f.holds(5, now) {
		t.Fatal("a fence that never denied holds")
	}
	for n := 1; n <= 12; n++ {
		if began := f.deny(5, now); began != (n == 1) {
			t.Fatalf("denial %d reported began=%v: the refused state must be announced once", n, began)
		}
		wait := f.next.Sub(now)
		if wait >= deniedBackoffCap*3/2 || wait < deniedBackoffBase/2 {
			t.Fatalf("denial %d waits %v: outside the jittered 0.5 s to 5 s cap", n, wait)
		}
		if n > 6 && wait < deniedBackoffCap/2 {
			t.Fatalf("denial %d waits %v: the backoff never reached its cap", n, wait)
		}
	}
	if !f.holds(5, now) || !f.holds(4, now) {
		t.Fatal("the fence does not hold the refused grant right after a denial")
	}
	if f.holds(6, now) {
		t.Fatal("a newer grant must lift the fence at once")
	}
	if f.holds(5, now.Add(deniedBackoffCap*2)) {
		t.Fatal("the fence outlived its backoff cap")
	}
	since, was := f.clear()
	if !was || !since.Equal(now) {
		t.Fatalf("recovery reported was=%v since=%v", was, since)
	}
	if _, again := f.clear(); again {
		t.Fatal("recovery was reported twice")
	}
	if f.holds(5, now) {
		t.Fatal("a cleared fence still holds")
	}
}

func TestDeniedFenceLiftsOnANetworkChangeAndRestartsItsBackoff(t *testing.T) {
	var f deniedFence
	now := time.Unix(1000, 0)
	for i := 0; i < 10; i++ {
		f.deny(5, now)
	}
	f.networkChanged()
	if f.holds(5, now) {
		t.Fatal("a network change did not lift the wait")
	}
	if began := f.deny(5, now); began {
		t.Fatal("a network change ended the refused state; only a relay that prepares may")
	}
	if wait := f.next.Sub(now); wait >= deniedBackoffBase*3/2*2 {
		t.Fatalf("backoff after a network change is %v: it did not restart", wait)
	}
	var idle deniedFence
	idle.networkChanged() // nothing refused: nothing to reset
	if idle.failures != 0 {
		t.Fatal("a network change invented a refusal")
	}
}

type fatalRefusal struct{}

func (fatalRefusal) Error() string { return "relay refused admission" }
func (fatalRefusal) Fatal() bool   { return true }

// scriptedRegionalEngine refuses admission (fatally) until allow is set, counts the attempts and keeps the
// network-change callback the recovery loop registered.
type scriptedRegionalEngine struct {
	failingRegionalEngine
	mu         sync.Mutex
	allow      bool
	attempts   int
	callback   func()
	registered int
	released   int
}

func (e *scriptedRegionalEngine) PrepareRelay(context.Context, tailcfg.DERPRegionID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts++
	if e.allow {
		return nil
	}
	return fatalRefusal{}
}

func (e *scriptedRegionalEngine) OnNetworkChange(f func()) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.callback, e.registered = f, e.registered+1
	return func() { e.mu.Lock(); e.released++; e.mu.Unlock() }
}

func (e *scriptedRegionalEngine) state() (attempts int, registered, released int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attempts, e.registered, e.released
}

// After every relay refused admission, the loop tries them again on its own (no new grant, no restart): the
// 22-minute wedge of F21-48 on Windows held the relay path until a daemon restart.
func TestRegionalRecoveryTriesRefusedRelaysAgainWithoutANewGrant(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{
		NodeID: "relay_test", NodeGeneration: 1, ProcessEpoch: "epoch_test", Region: "test", FailureDomain: "domain_test",
		Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: 443,
		State: "ready", ObservedAt: now, ExpiresAt: now + 600, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now,
	}
	a.regional = &RegionalCandidates{IssuedAt: now, ExpiresAt: now + 600, Nodes: []RegionalNode{node}}
	a.relay.mu.Lock()
	a.relay.grants = map[string]peernetwork.RelayGrant{node.NodeID: {NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, ExpiresAt: now + 600}}
	engine := &scriptedRegionalEngine{}
	r := &regionalRecovery{
		authority: a, control: newRegionalControl(a.private, time.Now), engine: engine, wake: make(chan struct{}, 1),
		peerID: "machine_test", done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 64),
	}
	a.relay.recovery = r
	a.relay.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	go r.run(ctx)
	defer cancel()
	waitFor := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("%s did not happen within %v", what, limit)
	}
	denied := func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.status.Reason == "regional_admission_denied" }
	waitFor("the refusal", 5*time.Second, denied)
	if _, registered, _ := engine.state(); registered != 1 {
		t.Fatalf("the loop registered %d network-change callbacks, want 1", registered)
	}
	engine.mu.Lock()
	engine.allow = true
	engine.mu.Unlock()
	waitFor("the relay being tried again with the same grant", 15*time.Second, func() bool { return !denied() })
	cancel()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not stop")
	}
	if _, _, released := engine.state(); released != 1 {
		t.Fatalf("the network-change callback was released %d times, want 1", released)
	}
	// the registered callback only signals; it never blocks the network monitor
	engine.callback()
	engine.callback()
}
