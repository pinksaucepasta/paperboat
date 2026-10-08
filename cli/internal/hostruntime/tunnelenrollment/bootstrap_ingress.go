package tunnelenrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

// assemblyIngressAuthority independently reads the machine-authenticated server
// projection. Each candidate owns its authority and bounded cache until drained.
// Only fixed categories and validated protocol error codes are emitted.
var ingressDiagnosticLogger = slog.New(slog.NewTextHandler(os.Stderr, nil))

type assemblyIngressAuthority struct {
	source      *HTTPSProductionAssemblySource
	request     ActivationRequest
	generation  uint64
	contentHash string
	mu          sync.Mutex
	body        []byte
	decisions   []connectorprotocol.IngressDecision
	fetched     time.Time
}

func (a *assemblyIngressAuthority) lookup(ctx context.Context, open connectorprotocol.StreamOpen, claimed connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	// The carrier has already authenticated this open tuple. Independently
	// fence it to this assembly and immutable candidate before any lookup.
	if open.Validate() != nil || open.AccountID != a.request.AccountID || open.TunnelID != a.request.TunnelID || open.ConnectorID != a.request.ConnectorID || open.ProcessGeneration != a.request.ProcessGeneration || open.Generation != a.generation || a.contentHash == "" {
		ingressDiagnosticLogger.WarnContext(ctx, "durable ingress authority rejected", "code", "candidate_identity")
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	if claimed.Binding.Audience != "public" {
		if a.source.browserIngress == nil {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		return a.source.browserIngress(ctx, open, claimed)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Err() != nil {
		ingressDiagnosticLogger.WarnContext(ctx, "durable ingress authority rejected", "code", "unbound_or_cancelled")
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	// Only one session's bounded cache exists per candidate; reconnecting a
	// generation replaces it rather than retaining session-indexed state.
	body, err := json.Marshal(carrierBootstrapRequest{Schema: carrierBootstrapSchema, Kind: "carrier_bootstrap_request", SessionID: open.SessionID, ProcessGeneration: a.request.ProcessGeneration, ConfigGeneration: a.generation, ConfigContentHash: a.contentHash})
	if err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	if !bytes.Equal(body, a.body) {
		a.body = body
		a.decisions = nil
		a.fetched = time.Time{}
	}
	now := time.Now().UTC()
	if a.fetched.IsZero() || now.Sub(a.fetched) >= connectorprotocol.IngressRefreshInterval {
		d, err := a.source.fetchCarrierDescriptor(ctx, a.request, a.body)
		if err != nil {
			var bootstrap *CarrierBootstrapError
			if errors.As(err, &bootstrap) {
				ingressDiagnosticLogger.WarnContext(ctx, "durable ingress authority refresh failed", "code", bootstrap.Code, "status", bootstrap.StatusCode)
			} else {
				ingressDiagnosticLogger.WarnContext(ctx, "durable ingress authority refresh failed", "code", "unavailable")
			}
			a.decisions = nil
			a.fetched = time.Time{}
			return connectorprotocol.IngressDecision{}, err
		}
		if d.AccountID != open.AccountID || d.TunnelID != open.TunnelID || d.ConnectorID != open.ConnectorID || d.SessionID != open.SessionID || d.ProcessGeneration != open.ProcessGeneration || d.ConfigGeneration != open.Generation || d.ConfigContentHash != a.contentHash || len(d.IngressDecisions) > 4096 {
			ingressDiagnosticLogger.WarnContext(ctx, "durable ingress authority rejected", "code", "descriptor_identity")
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		a.decisions = d.IngressDecisions
		a.fetched = now
	}
	for _, d := range a.decisions {
		if claimed.Authorize(d, open, claimed.EdgeNodeID, claimed.EdgeProcessEpoch, time.Now().UTC()) == nil {
			return d, nil
		}
	}
	code := "authority_binding"
	for _, d := range a.decisions {
		if d.IssuedAt.After(time.Now().UTC().Add(connectorprotocol.MaxClockSkew)) {
			code = "authority_not_yet_valid"
			break
		}
		if !d.ExpiresAt.After(time.Now().UTC()) {
			code = "authority_expired"
			break
		}
	}
	ingressDiagnosticLogger.WarnContext(ctx, "durable ingress authority rejected", "code", code)
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}
