package tailnet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
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
	a.clientEngine = &tailcat.Server{}
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
