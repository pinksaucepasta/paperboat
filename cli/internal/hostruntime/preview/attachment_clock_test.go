package preview

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestMachineAttachmentAdmissionUsesServerClock(t *testing.T) {
	ctx := context.Background()
	stateRoot, store := newMachineAttachmentIdentity(t)
	local := time.Now()
	serverclock.Observe(local.Add(-10*time.Minute).UTC().Format(http.TimeFormat), local, local)
	t.Cleanup(func() { now := time.Now(); serverclock.Observe(now.UTC().Format(http.TimeFormat), now, now) })
	if serverclock.Offset() > -9*time.Minute {
		t.Fatal("server clock offset was not learned")
	}
	now := serverclock.Now().UTC()
	identityValue := testPreviewCarrierIdentity(1)
	identityValue.TunnelID, identityValue.ConnectorID = identityValue.HostID, identityValue.HostID
	admission := machineAttachmentAdmission(t, store, now, identityValue, []string{"h2://edge.example.test:8443", "h3://edge.example.test:9443"})

	admission.ExpiresAt = now.Add(2 * time.Minute)
	var mu sync.Mutex
	var calls int
	var edges []*connector.DataCarrier
	factory := func(got connector.DataCarrierIdentity, config connector.DataCarrierPoolConfig, endpoints connector.NetworkDialerConfig) (connector.DataCarrierSessionSource, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		local, remote := net.Pipe()
		edge, err := connector.NewDataCarrierServer(ctx, remote, config.Carrier, connector.DataCarrierAdmission{Identity: got, Authorize: func(context.Context, connector.StreamOpen) error { return nil }})
		if err != nil {
			_ = local.Close()
			_ = remote.Close()
			return connector.DataCarrierSessionSource{}, err
		}
		mu.Lock()
		edges = append(edges, edge)
		mu.Unlock()
		dialer := connector.DataCarrierDialer(func(_ context.Context, request connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
			return connector.DataCarrierDialResult{Link: local, PeerIdentity: got, Transport: request.Transport, EdgeID: request.EdgeID, FailureDomain: request.FailureDomain}, nil
		})
		return connector.NewDataCarrierSessionSource(got, config, dialer)
	}
	source, err := NewMachineAttachmentSessionSource(MachineAttachmentSessionSourceConfig{StateRoot: stateRoot, SessionFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = source.Close(context.Background())
		mu.Lock()
		defer mu.Unlock()
		for _, edge := range edges {
			_ = edge.Close()
		}
	})

	first, err := acquireAdmittedCarrier(source, ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	if first.Active == nil {
		t.Fatal("fresh admission did not establish a carrier")
	}
	thumbprint, err := connectorprotocol.IdentityThumbprint(store.Current().Public())
	if err != nil {
		t.Fatal(err)
	}
	b := admission.Binding
	accessor := AccessorCarrierAdmission{
		AccountID: b.AccountID, MachineID: b.HostID,
		AccessorPublicKey: b.MachineIdentityPublicKey, AccessorThumbprint: thumbprint,
		TunnelID: b.TunnelID, CarrierConnectorID: b.ConnectorID, CarrierSessionID: b.SessionID,
		ProcessGeneration: b.ProcessGeneration, ConfigGeneration: b.ConfigGeneration,
		EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch,
		EdgeCarrierServerSPKISHA256:          b.EdgeCarrierServerSPKISHA256,
		EdgeCarrierServerCertificateChainPEM: b.EdgeCarrierServerCertificateChainPEM,
		EdgeEndpoints:                        admission.EdgeEndpoints, ExpiresAt: admission.ExpiresAt,
	}
	if _, err := source.AcquirePrivateAccessCarrier(ctx, accessor); err != nil {
		t.Fatalf("fresh accessor admission: %v", err)
	}
	accessor.ExpiresAt = now.Add(-time.Second)
	if _, err := source.AcquirePrivateAccessCarrier(ctx, accessor); !errors.Is(err, ErrMachineAttachmentSessionInvalid) {
		t.Fatalf("expired accessor admission: %v", err)
	}
	admission.ExpiresAt = now.Add(-time.Second)
	if _, err := acquireAdmittedCarrier(source, ctx, admission); !errors.Is(err, ErrMachineAttachmentSessionInvalid) {
		t.Fatalf("expired admission: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("factory calls = %d, want 1", calls)
	}
}
