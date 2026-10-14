package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgehttp"
)

type runtimeSourceFake struct {
	mu           sync.Mutex
	admissions   []control.RuntimeCarrierAdmission
	err          error
	observations []control.RuntimeCarrierObservation
}

func (s *runtimeSourceFake) RuntimeCarrierAdmissions(context.Context, string, string) ([]control.RuntimeCarrierAdmission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]control.RuntimeCarrierAdmission(nil), s.admissions...), s.err
}
func (s *runtimeSourceFake) ObserveRuntimeCarriers(_ context.Context, _, _ string, o []control.RuntimeCarrierObservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = append(s.observations, o...)
	return nil
}
func runtimeAdmission(t *testing.T) control.RuntimeCarrierAdmission {
	t.Helper()
	p := testPreviewCarrierAdmission("preview", "operation", "runtime_route", "machine.runtime.example.test", time.Now().Add(time.Hour))
	b := p.Binding
	return control.RuntimeCarrierAdmission{Schema: datacarrier.RuntimeCarrierSchema, Binding: control.RuntimeCarrierBinding{AccountID: b.AccountID, HostID: b.HostID, TunnelID: b.TunnelID, ConnectorID: b.ConnectorID, SessionID: b.SessionID, ProcessGeneration: b.ProcessGeneration, ConfigGeneration: b.ConfigGeneration, InstallationGeneration: 1, RouteID: b.RouteID, RouteGeneration: b.RouteGeneration, EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch, EdgeCarrierServerSPKISHA256: b.EdgeCarrierServerSPKISHA256, EdgeCarrierServerCertificateChainPEM: b.EdgeCarrierServerCertificateChainPEM, MachineIdentityPublicKey: b.MachineIdentityPublicKey, MachineIdentityThumbprint: b.MachineIdentityThumbprint}, AttachmentGeneration: p.AttachmentGeneration, ConfigContentHash: p.ConfigContentHash, EdgeEndpoints: p.EdgeEndpoints, Hostname: "machine.runtime.example.test", RouteKind: datacarrier.RuntimeCarrierRoute, RouteRevision: 1, ExpiresAt: p.ExpiresAt}
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
			source := &runtimeSourceFake{admissions: []control.RuntimeCarrierAdmission{a}}
			expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: a.Binding.EdgeNodeID, ProcessEpoch: a.Binding.EdgeProcessEpoch})
			if err != nil {
				t.Fatal(err)
			}
			routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: a.Binding.EdgeProcessEpoch})
			if err != nil {
				t.Fatal(err)
			}
			w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Interval: 20 * time.Millisecond, Timeout: time.Second}
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
			ea, err := a.Expected(a.Binding.EdgeNodeID, a.Binding.EdgeProcessEpoch, time.Now())
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
			switch mode {
			case "revocation":
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
