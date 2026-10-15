package connectorprotocol

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelendpoint"
)

const (
	CarrierBootstrapSchema   = "paperboat.connector-bootstrap/v1"
	MaximumBootstrapCarriers = 2
)

var spkiHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type CarrierBootstrapRequest struct {
	Schema            string `json:"schema"`
	Kind              string `json:"kind"`
	SessionID         string `json:"session_id"`
	ProcessGeneration uint64 `json:"process_generation"`
	ConfigGeneration  uint64 `json:"config_generation"`
	ConfigContentHash string `json:"config_content_hash"`
}

func (r CarrierBootstrapRequest) Validate() error {
	if r.Schema != CarrierBootstrapSchema || r.Kind != "carrier_bootstrap_request" || wire.ValidateIdentifier(r.SessionID) != nil || r.ProcessGeneration == 0 || r.ConfigGeneration == 0 || !wire.ValidContentHash(r.ConfigContentHash) {
		return wire.ErrInvalidInput
	}
	return nil
}

type CarrierBootstrapNode struct {
	EdgeNodeID                string   `json:"edge_node_id"`
	EdgeProcessEpoch          string   `json:"edge_process_epoch"`
	FailureDomain             string   `json:"failure_domain"`
	Endpoints                 []string `json:"endpoints"`
	ServerSPKISHA256          string   `json:"server_spki_sha256"`
	ServerCertificateChainPEM string   `json:"server_certificate_chain_pem"`
}

func (n CarrierBootstrapNode) Validate() error {
	if wire.ValidateIdentifier(n.EdgeNodeID) != nil || wire.ValidateOpaqueEpoch(n.EdgeProcessEpoch) != nil || wire.ValidateIdentifier(n.FailureDomain) != nil || len(n.Endpoints) != 2 || !spkiHashPattern.MatchString(n.ServerSPKISHA256) || !validCertificateChainPEM(n.ServerCertificateChainPEM) {
		return wire.ErrInvalidInput
	}
	seen := map[string]bool{}
	for _, raw := range n.Endpoints {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Hostname() == "" || parsed.Port() == "" {
			return wire.ErrInvalidInput
		}
		switch parsed.Scheme {
		case "h2", "h3":
			if seen[parsed.Scheme] {
				return wire.ErrInvalidInput
			}
			seen[parsed.Scheme] = true
		default:
			return wire.ErrInvalidInput
		}
	}
	if !seen["h2"] || !seen["h3"] {
		return wire.ErrInvalidInput
	}
	return nil
}

type CarrierBootstrapDescriptor struct {
	Schema               string                 `json:"schema"`
	Kind                 string                 `json:"kind"`
	AccountID            string                 `json:"account_id"`
	TunnelID             string                 `json:"tunnel_id"`
	ConnectorID          string                 `json:"connector_id"`
	HostID               string                 `json:"host_id"`
	StableEndpointID     string                 `json:"stable_endpoint_id"`
	SessionID            string                 `json:"session_id"`
	ProcessGeneration    uint64                 `json:"process_generation"`
	CredentialGeneration uint64                 `json:"credential_generation"`
	ConfigGeneration     uint64                 `json:"config_generation"`
	ConfigContentHash    string                 `json:"config_content_hash"`
	Carriers             []CarrierBootstrapNode `json:"carriers"`
	IngressDecisions     []wire.IngressDecision `json:"ingress_decisions"`
	IssuedAt             time.Time              `json:"issued_at"`
	ExpiresAt            time.Time              `json:"expires_at"`
}

func (d CarrierBootstrapDescriptor) Validate(now time.Time) error {
	if d.Schema != CarrierBootstrapSchema || d.Kind != "carrier_bootstrap_descriptor" || wire.ValidateIdentifier(d.AccountID) != nil || wire.ValidateIdentifier(d.TunnelID) != nil || wire.ValidateIdentifier(d.ConnectorID) != nil || wire.ValidateIdentifier(d.HostID) != nil || tunnelendpoint.ValidateID(d.StableEndpointID) != nil || wire.ValidateIdentifier(d.SessionID) != nil || d.ProcessGeneration == 0 || d.CredentialGeneration == 0 || d.ConfigGeneration == 0 || !wire.ValidContentHash(d.ConfigContentHash) || len(d.Carriers) == 0 || len(d.Carriers) > MaximumBootstrapCarriers || d.IssuedAt.IsZero() || d.ExpiresAt.IsZero() || !d.ExpiresAt.After(d.IssuedAt) || d.ExpiresAt.Sub(d.IssuedAt) > 15*time.Second {
		return wire.ErrInvalidInput
	}
	if !now.IsZero() && (d.IssuedAt.After(now.Add(wire.MaxClockSkew)) || !d.ExpiresAt.After(now)) {
		return wire.ErrInvalidInput
	}
	seenNodes, seenDomains := map[string]bool{}, map[string]bool{}
	for _, carrier := range d.Carriers {
		if carrier.Validate() != nil || seenNodes[carrier.EdgeNodeID+"\x00"+carrier.EdgeProcessEpoch] || seenDomains[carrier.FailureDomain] {
			return wire.ErrInvalidInput
		}
		seenNodes[carrier.EdgeNodeID+"\x00"+carrier.EdgeProcessEpoch] = true
		seenDomains[carrier.FailureDomain] = true
	}
	if len(d.IngressDecisions) > 4096 {
		return wire.ErrInvalidInput
	}
	for _, ingress := range d.IngressDecisions {
		at := now
		if at.IsZero() {
			at = ingress.IssuedAt
		}
		if ingress.Validate(at) != nil || ingress.Binding.AccountID != d.AccountID || ingress.Binding.TunnelID != d.TunnelID || ingress.Binding.HostID != d.HostID || ingress.ConnectorID != d.ConnectorID || ingress.SessionID != d.SessionID || ingress.ProcessGeneration != d.ProcessGeneration || ingress.ConfigGeneration != d.ConfigGeneration {
			return wire.ErrInvalidInput
		}
	}
	return nil
}

type CarrierBootstrapSource interface {
	Descriptor(context.Context, ActiveControlSession) (CarrierBootstrapDescriptor, error)
}

type SQLCarrierBootstrapSource struct {
	Quota accountingauthority.QuotaChecker
	DB    *db.DB
	Clock wire.Clock
}

func (s SQLCarrierBootstrapSource) Descriptor(ctx context.Context, active ActiveControlSession) (CarrierBootstrapDescriptor, error) {
	if ctx == nil || s.DB == nil || active.validate() != nil {
		return CarrierBootstrapDescriptor{}, wire.ErrInvalidInput
	}
	tunnel, err := s.DB.Queries().GetTunnelManagedEndpointForConnectorBootstrapV1(ctx, dbsqlc.GetTunnelManagedEndpointForConnectorBootstrapV1Params{TunnelID: active.TunnelID, AccountID: active.AccountID})
	if err != nil {
		return CarrierBootstrapDescriptor{}, err
	}
	if tunnelendpoint.ValidateID(tunnel.StableEndpointID) != nil {
		return CarrierBootstrapDescriptor{}, fmt.Errorf("persisted stable endpoint identity is invalid: %w", wire.ErrSnapshotRejected)
	}
	now := time.Now().UTC()
	if s.Clock != nil {
		now = s.Clock.Now().UTC()
	}
	rows, err := s.DB.Queries().ListReadyConnectorCarrierNodesV1(ctx, dbsqlc.ListReadyConnectorCarrierNodesV1Params{Now: now, RowLimit: MaximumBootstrapCarriers, AccountID: active.AccountID, ConnectorID: active.ConnectorID, SessionID: active.SessionID, ConfigGeneration: int64(active.ConfigGeneration)})
	if err != nil {
		return CarrierBootstrapDescriptor{}, err
	}
	expiresAt := now.Add(15 * time.Second)
	carriers := make([]CarrierBootstrapNode, 0, len(rows))
	seenDomains := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if _, duplicate := seenDomains[row.FailureDomain.String]; duplicate {
			continue
		}
		if !row.CarrierEndpointHost.Valid || !row.CarrierEndpointTcpPort.Valid || !row.CarrierEndpointQuicPort.Valid || !row.CarrierServerSpkiSha256.Valid || !row.CarrierServerCertificateChainPem.Valid || !row.FailureDomain.Valid {
			continue
		}
		host := row.CarrierEndpointHost.String
		if net.ParseIP(host) == nil && (len(host) > 253 || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/\\\r\n")) {
			continue
		}
		carrier := CarrierBootstrapNode{
			EdgeNodeID: row.EdgeNodeID, EdgeProcessEpoch: row.EdgeProcessEpoch, FailureDomain: row.FailureDomain.String,
			Endpoints: []string{
				"h2://" + net.JoinHostPort(host, fmt.Sprint(row.CarrierEndpointTcpPort.Int32)),
				"h3://" + net.JoinHostPort(host, fmt.Sprint(row.CarrierEndpointQuicPort.Int32)),
			},
			ServerSPKISHA256: row.CarrierServerSpkiSha256.String, ServerCertificateChainPEM: row.CarrierServerCertificateChainPem.String,
		}
		if carrier.Validate() != nil {
			continue
		}
		seenDomains[carrier.FailureDomain] = struct{}{}
		carriers = append(carriers, carrier)
		if row.ValidUntil.Before(expiresAt) {
			expiresAt = row.ValidUntil
		}
	}
	if len(carriers) == 0 {
		return CarrierBootstrapDescriptor{}, wire.CodeError(wire.ErrNotReady, wire.ReasonSnapshotRejected, true, nil)
	}
	// The descriptor and its ingress decisions form one authority snapshot.
	// Validate them against the same instant used by both SQL projections.
	ingress, err := (SQLIngressSource{DB: s.DB, Clock: wire.ClockFunc(func() time.Time { return now }), Quota: s.Quota}).Daemon(ctx, active)
	if err != nil {
		return CarrierBootstrapDescriptor{}, err
	}
	descriptor := CarrierBootstrapDescriptor{
		Schema: CarrierBootstrapSchema, Kind: "carrier_bootstrap_descriptor",
		AccountID: active.AccountID, TunnelID: active.TunnelID, ConnectorID: active.ConnectorID, HostID: active.HostID,
		StableEndpointID: tunnel.StableEndpointID,
		SessionID:        active.SessionID, ProcessGeneration: active.ProcessGeneration, CredentialGeneration: active.CredentialGeneration,
		ConfigGeneration: active.ConfigGeneration, ConfigContentHash: active.ConfigContentHash, Carriers: carriers, IngressDecisions: ingress,
		IssuedAt: now, ExpiresAt: expiresAt,
	}
	if err := descriptor.Validate(now); err != nil {
		return CarrierBootstrapDescriptor{}, err
	}
	return descriptor, nil
}

func validCertificateChainPEM(value string) bool {
	if len(value) == 0 || len(value) > 64<<10 || !strings.HasPrefix(value, "-----BEGIN CERTIFICATE-----") || strings.ContainsRune(value, '\x00') {
		return false
	}
	rest := []byte(value)
	count := 0
	for len(rest) > 0 {
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return false
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return false
		}
		count++
		rest = next
	}
	return count > 0
}
