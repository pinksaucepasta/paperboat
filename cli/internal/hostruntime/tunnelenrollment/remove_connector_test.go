package tunnelenrollment

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A connector the control plane reports gone is forgotten: its journal record and key go, so the
// next hostd start does not reactivate it, and other tunnels are untouched.
func TestRemoveConnectorForgetsOnlyThatConnector(t *testing.T) {
	gone, kept := "tunnel_gone_01", "tunnel_kept_01"
	store, _ := writeMultiResumeJournalFixture(t, 1, gone, kept)
	state, err := store.loadJournal()
	if err != nil {
		t.Fatal(err)
	}
	goneRecord := state.Records[gone]
	request := goneRecord.activationRequest()

	other := request
	other.ConnectorID = "connector_replaced_01"
	if err := store.RemoveConnector(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if state, _ = store.loadJournal(); len(state.Records) != 2 {
		t.Fatalf("a record of another connector was removed: %v", state.Records)
	}

	if err := store.RemoveConnector(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	state, err = store.loadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := state.Records[gone]; present || len(state.Records) != 1 || state.Records[kept].Phase != "active" {
		t.Fatalf("journal after removal: %v", state.Records)
	}
	if _, err := store.Sign(context.Background(), goneRecord.Credential.Reference, []byte("x")); err == nil {
		t.Fatal("the removed connector's signing key is still usable")
	}
	if _, err := store.Sign(context.Background(), state.Records[kept].Credential.Reference, []byte("x")); err != nil {
		t.Fatalf("the other connector's key was removed: %v", err)
	}
	if err := store.RemoveConnector(context.Background(), request); err != nil {
		t.Fatalf("removal is not idempotent: %v", err)
	}

	activator := &parallelResumeActivator{blockTunnel: "none", blockEntered: make(chan struct{}), releaseBlock: make(chan struct{}), otherEntered: make(chan struct{}, 4)}
	manager, err := NewManager(ManagerConfig{ControlURL: "https://api.example.test", HostID: "host_01", Auth: &testAuth{}, Credentials: store, Activator: activator, ControlToken: "local-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Resume(context.Background()); err != nil && !errors.Is(err, ErrActivation) {
		t.Fatal(err)
	}
	activator.mu.Lock()
	defer activator.mu.Unlock()
	if activator.calls[gone] != 0 || activator.calls[kept] != 1 {
		t.Fatalf("resume activated %v", activator.calls)
	}
}

type removingCredentials struct {
	activatorCredentials
	removed []ActivationRequest
}

func (c *removingCredentials) RemoveConnector(_ context.Context, request ActivationRequest) error {
	c.removed = append(c.removed, request)
	return nil
}

// The activator drops a gone connector's assembly entry and has its record removed, once.
func TestActivatorRetiresAGoneConnectorByForgettingItsRecord(t *testing.T) {
	credentials := &removingCredentials{}
	activator, err := NewProductionAssemblyActivator(ProductionAssemblyActivatorConfig{Credentials: credentials, Source: &invalidAssemblySource{}})
	if err != nil {
		t.Fatal(err)
	}
	request := activationRequestFixture()
	key := request.HostID + "\x00" + request.TunnelID
	activator.assemblies[key] = &activatedAssembly{request: request, ready: make(chan struct{})}
	activator.retireGone(request)
	if _, present := activator.assemblies[key]; present {
		t.Fatal("the gone connector's assembly entry was kept")
	}
	if len(credentials.removed) != 1 || credentials.removed[0].ConnectorID != request.ConnectorID {
		t.Fatalf("removed = %v", credentials.removed)
	}
}

func TestCarrierBootstrapRetryDelayDoublesToACapWithSpread(t *testing.T) {
	previous := time.Duration(0)
	for attempt := 1; attempt <= 12; attempt++ {
		delay := carrierBootstrapRetryDelay(attempt)
		if delay < 100*time.Millisecond || delay > 5*time.Second {
			t.Fatalf("attempt %d waits %v", attempt, delay)
		}
		if attempt <= 3 && delay < previous/2 {
			t.Fatalf("backoff shrank at attempt %d: %v after %v", attempt, delay, previous)
		}
		previous = delay
	}
}
