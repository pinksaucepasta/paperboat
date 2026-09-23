package tunnelenrollment

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

// assemblyIngressAuthority independently reads the machine-authenticated server
// projection. It is scoped to one connector assembly, never shared by hosts.
type assemblyIngressAuthority struct {
	source    *HTTPSProductionAssemblySource
	request   ActivationRequest
	mu        sync.Mutex
	body      []byte
	decisions []connectorprotocol.IngressDecision
	fetched   time.Time
}

func (a *assemblyIngressAuthority) bind(w connectorprotocol.Welcome, r tunnelmanager.ApplyRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.body, _ = json.Marshal(carrierBootstrapRequest{Schema: carrierBootstrapSchema, Kind: "carrier_bootstrap_request", SessionID: w.SessionID, ProcessGeneration: a.request.ProcessGeneration, ConfigGeneration: r.Snapshot.Generation, ConfigContentHash: r.Snapshot.ContentHash})
	a.decisions = nil
	a.fetched = time.Time{}
}

func (a *assemblyIngressAuthority) lookup(ctx context.Context, open connectorprotocol.StreamOpen, claimed connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	if claimed.Binding.Audience != "public" {
		if a.source.browserIngress == nil {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		return a.source.browserIngress(ctx, open, claimed)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.body) == 0 || ctx.Err() != nil {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	now := time.Now().UTC()
	if a.fetched.IsZero() || now.Sub(a.fetched) >= connectorprotocol.IngressRefreshInterval {
		d, err := a.source.fetchCarrierDescriptor(ctx, a.request, a.body)
		if err != nil {
			a.decisions = nil
			a.fetched = time.Time{}
			return connectorprotocol.IngressDecision{}, err
		}
		if d.AccountID != open.AccountID || d.TunnelID != open.TunnelID || d.ConnectorID != open.ConnectorID || d.SessionID != open.SessionID || d.ProcessGeneration != open.ProcessGeneration || d.ConfigGeneration != open.Generation || len(d.IngressDecisions) > 4096 {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		a.decisions = d.IngressDecisions
		a.fetched = now
	}
	for _, d := range a.decisions {
		if d.Binding.RouteID == open.RouteID && d.EdgeNodeID == claimed.EdgeNodeID && d.EdgeProcessEpoch == claimed.EdgeProcessEpoch && d.Validate(time.Now().UTC()) == nil {
			return d, nil
		}
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}
