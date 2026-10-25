package runtime

import (
	"context"
	"errors"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp"
)

type runtimeSourceFake struct {
	mu           sync.Mutex
	admissions   []runtimewire.Admission
	err          error
	endpointErr  error
	observations []runtimewire.Observation
	// endpointUpdate, when set, answers endpoint pulls; endpointPulls records each since sent.
	endpointUpdate func(since string) control.EndpointUpdate
	endpointPulls  []string
}

func (s *runtimeSourceFake) RuntimeCarrierAdmissions(context.Context, string, string) ([]runtimewire.Admission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runtimewire.Admission(nil), s.admissions...), s.err
}
func (s *runtimeSourceFake) EndpointAdmissions(_ context.Context, _, _, since string) (control.EndpointUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpointPulls = append(s.endpointPulls, since)
	if s.endpointUpdate != nil {
		return s.endpointUpdate(since), s.endpointErr
	}
	out := []runtimewire.Admission{}
	for _, a := range s.admissions {
		if a.EndpointID != "" {
			out = append(out, a)
		}
	}
	return control.EndpointUpdate{Complete: true, Admissions: out, Cursor: "cursor"}, s.endpointErr
}
func (s *runtimeSourceFake) ObserveRuntimeCarriers(_ context.Context, _, _ string, o []runtimewire.Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = append(s.observations, o...)
	return nil
}
func runtimeAdmission(t *testing.T) runtimewire.Admission {
	t.Helper()
	p := testPreviewCarrierAdmission("preview", "operation", "runtime_route", "machine.runtime.example.test", time.Now().Add(time.Hour))
	b := p.Binding
	return runtimewire.Admission{Schema: runtimewire.Schema, Binding: runtimewire.Binding{AccountID: b.AccountID, HostID: b.HostID, TunnelID: b.TunnelID, ConnectorID: b.ConnectorID, SessionID: b.SessionID, ProcessGeneration: b.ProcessGeneration, ConfigGeneration: b.ConfigGeneration, InstallationGeneration: 1, RouteID: b.RouteID, RouteGeneration: b.RouteGeneration, EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch, EdgeCarrierServerSPKISHA256: b.EdgeCarrierServerSPKISHA256, EdgeCarrierServerCertificateChainPEM: b.EdgeCarrierServerCertificateChainPEM, MachineIdentityPublicKey: b.MachineIdentityPublicKey, MachineIdentityThumbprint: b.MachineIdentityThumbprint}, AttachmentGeneration: p.AttachmentGeneration, ConfigContentHash: p.ConfigContentHash, EdgeEndpoints: p.EdgeEndpoints, Hostname: "machine.runtime.example.test", RouteKind: runtimewire.RouteKind, RouteRevision: 1, ExpiresAt: p.ExpiresAt}
}
func waitRuntime(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runtime carrier condition timed out")
}
func TestRuntimeCarrierReadinessRevocationExpiryAndRecovery(t *testing.T) {
	for _, mode := range []string{"revocation", "expiry", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			a := runtimeAdmission(t)
			if mode == "expiry" {
				a.ExpiresAt = time.Now().Add(700 * time.Millisecond)
			}
			source := &runtimeSourceFake{admissions: []runtimewire.Admission{a}}
			expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: a.Binding.EdgeNodeID, ProcessEpoch: a.Binding.EdgeProcessEpoch})
			if err != nil {
				t.Fatal(err)
			}
			routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: a.Binding.EdgeProcessEpoch})
			if err != nil {
				t.Fatal(err)
			}
			w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpointRegistry(t, a.Binding.EdgeProcessEpoch), Interval: 20 * time.Millisecond, Timeout: time.Second}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err = w.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := w.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			waitRuntime(t, func() bool { return len(expected.Snapshot()) == 1 })
			source.mu.Lock()
			for _, o := range source.observations {
				if o.Ready {
					t.Error("ready before carrier attached")
				}
			}
			source.mu.Unlock()
			ea, err := control.ExpectedRuntimeAdmission(a, a.Binding.EdgeNodeID, a.Binding.EdgeProcessEpoch, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			server := newPreviewCarrierTestServer(t, ea.Identity)
			done := make(chan error, 1)
			go func() { done <- w.Handle(ctx, server) }()
			waitRuntime(t, func() bool {
				source.mu.Lock()
				defer source.mu.Unlock()
				for _, o := range source.observations {
					if o.Ready {
						return true
					}
				}
				return false
			})
			source.mu.Lock()
			source.err = errors.New("control unavailable")
			source.mu.Unlock()
			time.Sleep(60 * time.Millisecond)
			if routes.Len() != 1 {
				t.Fatal("failed pull removed valid route")
			}
			if mode == "expiry" {
				time.Sleep(time.Until(a.ExpiresAt) + 100*time.Millisecond)
				if routes.Len() != 1 {
					t.Fatal("admitted runtime carrier expired during authority outage")
				}
			}
			switch mode {
			case "revocation", "expiry":
				source.mu.Lock()
				source.err = nil
				source.admissions = nil
				source.mu.Unlock()
			case "disconnect":
				_ = server.Close()
			}
			waitRuntime(t, func() bool { return routes.Len() == 0 })
			if mode == "disconnect" {
				source.mu.Lock()
				source.err = nil
				source.mu.Unlock()
				second := newPreviewCarrierTestServer(t, ea.Identity)
				secondDone := make(chan error, 1)
				go func() { secondDone <- w.Handle(ctx, second) }()
				waitRuntime(t, func() bool { return routes.Len() == 1 })
				_ = second.Close()
				select {
				case <-secondDone:
				case <-time.After(time.Second):
					t.Fatal("replacement handler leaked")
				}
			}
			_ = server.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler leaked")
			}
		})
	}
}

// Audit T10: when the host sends a goaway on its runtime carrier, the edge takes that carrier's
// routes out at once (new requests get the route-unavailable answer instead of a closing carrier)
// and keeps the carrier until the host closes it.
func TestRuntimeCarrierRoutesLeaveOnHostGoaway(t *testing.T) {
	a := runtimeAdmission(t)
	source := &runtimeSourceFake{admissions: []runtimewire.Admission{a}}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: a.Binding.EdgeNodeID, ProcessEpoch: a.Binding.EdgeProcessEpoch})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: a.Binding.EdgeProcessEpoch})
	if err != nil {
		t.Fatal(err)
	}
	w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpointRegistry(t, a.Binding.EdgeProcessEpoch), Interval: 20 * time.Millisecond, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Shutdown(context.Background()) }()
	waitRuntime(t, func() bool { return len(expected.Snapshot()) == 1 })
	ea, err := control.ExpectedRuntimeAdmission(a, a.Binding.EdgeNodeID, a.Binding.EdgeProcessEpoch, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	session := newTestCarrierSession()
	session.inbound = make(chan datacarrier.StreamLink, 1)
	config := datacarrier.DefaultConfig()
	config.Identity = ea.Identity
	config.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	server, err := datacarrier.NewServerWithSession(context.Background(), session, config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- w.Handle(ctx, server) }()
	waitRuntime(t, func() bool { return routes.Len() == 1 })

	edgeEnd, hostEnd := net.Pipe()
	session.inbound <- edgeEnd
	id := ea.Identity
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: id.AccountID, TunnelID: id.TunnelID, ConnectorID: id.ConnectorID, SessionID: id.SessionID, ProcessGeneration: id.ProcessGeneration, Generation: id.Generation, RouteID: a.Binding.RouteID, RequestID: "request_goaway", Kind: connectorprotocol.Goaway}
	if err := connectorprotocol.WriteStreamOpen(hostEnd, open); err != nil {
		t.Fatal(err)
	}
	if err := connectorprotocol.WriteGoawayNotice(hostEnd, connectorprotocol.GoawayHostDraining, time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	_ = hostEnd.Close()
	waitRuntime(t, func() bool { return routes.Len() == 0 })
	select {
	case err := <-done:
		t.Fatalf("handler left before the host closed its carrier: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = server.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handler after the carrier closed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler kept a closed carrier")
	}
}
