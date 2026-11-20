package connectorprotocol

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The private-access documents the server issues and its three consumers (machine CLI, edge, server itself)
// decode: one definition each, decode mode per side (clients tolerant, server strict).
const (
	PrivateAccessAdmissionKind = "private_access_carrier_admission"
	PrivateAccessSnapshotKind  = "private_access_carrier_snapshot"
	PrivateAccessGrantKind     = "private_access_authorization"

	MaxPrivateAccessCertificateChain = 64 << 10
)

var (
	accessorConfigHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	accessorNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// PrivateAccessAdmission is a short-lived, secret-free projection that authorizes one enrolled machine
// identity to establish a carrier to one exact edge and target generation.
type PrivateAccessAdmission struct {
	Schema                               string    `json:"schema"`
	Kind                                 string    `json:"kind"`
	AccountID                            string    `json:"account_id"`
	MachineID                            string    `json:"machine_id"`
	InstallationGeneration               uint64    `json:"installation_generation"`
	AccessorPublicKey                    string    `json:"accessor_public_key"`
	AccessorThumbprint                   string    `json:"accessor_thumbprint"`
	ResourceKind                         string    `json:"resource_kind"`
	ResourceID                           string    `json:"resource_id"`
	TunnelName                           string    `json:"tunnel_name,omitempty"`
	RouteName                            string    `json:"route_name,omitempty"`
	OperationID                          string    `json:"operation_id,omitempty"`
	ConnectorID                          string    `json:"connector_id,omitempty"`
	CarrierSessionID                     string    `json:"carrier_session_id"`
	RouteID                              string    `json:"route_id"`
	RouteGeneration                      uint64    `json:"route_generation"`
	SessionGeneration                    uint64    `json:"session_generation"`
	ProcessGeneration                    uint64    `json:"process_generation"`
	ConfigGeneration                     uint64    `json:"config_generation"`
	AssignmentGeneration                 uint64    `json:"assignment_generation"`
	AssignmentID                         string    `json:"assignment_id"`
	ConfigContentHash                    string    `json:"config_content_hash"`
	EdgeNodeID                           string    `json:"edge_node_id"`
	EdgeProcessEpoch                     string    `json:"edge_process_epoch"`
	Protocol                             string    `json:"protocol"`
	Hostname                             string    `json:"hostname"`
	MatchType                            string    `json:"match_type"`
	WildcardSuffix                       string    `json:"wildcard_suffix,omitempty"`
	EdgeEndpoints                        []string  `json:"edge_endpoints"`
	ExpiresAt                            time.Time `json:"expires_at"`
	TunnelID                             string    `json:"tunnel_id"`
	CarrierConnectorID                   string    `json:"carrier_connector_id"`
	EdgeCarrierServerSPKISHA256          string    `json:"edge_carrier_server_spki_sha256"`
	EdgeCarrierServerCertificateChainPEM string    `json:"edge_carrier_server_certificate_chain_pem"`
}

// PrivateAccessSnapshot is the complete admission set one machine (or one edge) is told about.
type PrivateAccessSnapshot struct {
	Schema     string                   `json:"schema"`
	Kind       string                   `json:"kind"`
	Complete   bool                     `json:"complete"`
	Admissions []PrivateAccessAdmission `json:"admissions"`
}

// ValidateEnvelope checks the snapshot's own fields; each admission is checked by its consumer.
func (s PrivateAccessSnapshot) ValidateEnvelope() error {
	if s.Schema != PrivateAccessSchema || s.Kind != PrivateAccessSnapshotKind || !s.Complete || s.Admissions == nil {
		return ErrInvalidInput
	}
	return nil
}

// ValidAccessorName reports whether value is a tunnel or route name of at most maximum bytes.
func ValidAccessorName(value string, maximum int) bool {
	return value == strings.TrimSpace(value) && len(value) >= 1 && len(value) <= maximum && accessorNamePattern.MatchString(value)
}

// ValidAccessorMatch is the one rule for an admission's host match. The server issues HTTP catch_all
// admissions (a private catch-all route is admitted under its stable endpoint hostname), so every side accepts them.
func ValidAccessorMatch(protocol, hostname, matchType, suffix string) bool {
	if protocol == "private_tcp" {
		return hostname == "" && matchType == "catch_all" && suffix == ""
	}
	if protocol != "http" || hostname == "" || hostname != strings.ToLower(hostname) {
		return false
	}
	switch matchType {
	case "exact", "managed_exact", "catch_all":
		return suffix == "" && !strings.Contains(hostname, "*")
	case "one_label_wildcard":
		return suffix != "" && !strings.Contains(suffix, "*") && !strings.HasPrefix(suffix, ".") && !strings.HasSuffix(suffix, ".") && hostname == "*."+suffix
	default:
		return false
	}
}

// Validate checks everything about an admission that does not depend on the consumer: identifiers,
// generations, expiry (when now is set), key and thumbprint, edge endpoints, match, and the resource shape.
// Certificate-chain parsing and edge identity binding stay with each consumer.
func (a PrivateAccessAdmission) Validate(now time.Time) error {
	if a.Schema != PrivateAccessSchema || a.Kind != PrivateAccessAdmissionKind {
		return ErrInvalidInput
	}
	for _, value := range []string{a.AccountID, a.MachineID, a.ResourceID, a.CarrierSessionID, a.RouteID, a.AssignmentID, a.EdgeNodeID, a.TunnelID, a.CarrierConnectorID} {
		if ValidateIdentifier(value) != nil {
			return ErrInvalidInput
		}
	}
	if ValidateOpaqueEpoch(a.EdgeProcessEpoch) != nil || a.InstallationGeneration == 0 || a.RouteGeneration == 0 || a.SessionGeneration == 0 || a.ProcessGeneration == 0 || a.ConfigGeneration == 0 || a.AssignmentGeneration == 0 || a.ExpiresAt.IsZero() || !now.IsZero() && !a.ExpiresAt.After(now) {
		return ErrInvalidInput
	}
	if !accessorConfigHashPattern.MatchString(a.ConfigContentHash) || !accessorConfigHashPattern.MatchString(a.EdgeCarrierServerSPKISHA256) || len(a.EdgeCarrierServerCertificateChainPEM) == 0 || len(a.EdgeCarrierServerCertificateChainPEM) > MaxPrivateAccessCertificateChain {
		return ErrInvalidInput
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(a.AccessorPublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return ErrInvalidInput
	}
	if thumbprint, err := IdentityThumbprint(ed25519.PublicKey(key)); err != nil || thumbprint != a.AccessorThumbprint {
		return ErrInvalidInput
	}
	if len(a.EdgeEndpoints) != 2 {
		return ErrInvalidInput
	}
	for index, scheme := range []string{"h2", "h3"} {
		endpoint, err := url.Parse(a.EdgeEndpoints[index])
		if err != nil {
			return ErrInvalidInput
		}
		port, portErr := strconv.ParseUint(endpoint.Port(), 10, 16)
		if endpoint.Scheme != scheme || endpoint.Hostname() == "" || portErr != nil || port == 0 || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return ErrInvalidInput
		}
	}
	if !ValidAccessorMatch(a.Protocol, a.Hostname, a.MatchType, a.WildcardSuffix) {
		return ErrInvalidInput
	}
	switch a.ResourceKind {
	case "preview":
		if a.TunnelName != "" || a.RouteName != "" || ValidateIdentifier(a.OperationID) != nil || a.ConnectorID != "" || a.Protocol != "http" {
			return ErrInvalidInput
		}
	case "tunnel":
		if !ValidAccessorName(a.TunnelName, 63) || !ValidAccessorName(a.RouteName, 80) || ValidateIdentifier(a.ConnectorID) != nil || a.OperationID != "" {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

// PrivateAccessGrantIssue is the body a machine sends to obtain a grant. It omits account, machine and
// session: the server derives those from the machine proof and never accepts them from the body.
type PrivateAccessGrantIssue struct {
	ResourceKind         string    `json:"resource_kind"`
	ResourceID           string    `json:"resource_id"`
	RouteID              string    `json:"route_id"`
	Audience             string    `json:"audience"`
	ExpiresAt            time.Time `json:"expires_at"`
	Nonce                string    `json:"nonce"`
	OperationID          string    `json:"operation_id,omitempty"`
	ConnectorID          string    `json:"connector_id,omitempty"`
	CarrierSessionID     string    `json:"carrier_session_id"`
	RouteGeneration      uint64    `json:"route_generation"`
	ProcessGeneration    uint64    `json:"process_generation"`
	ConfigGeneration     uint64    `json:"config_generation"`
	SessionGeneration    uint64    `json:"session_generation"`
	AssignmentGeneration uint64    `json:"assignment_generation"`
	EdgeNodeID           string    `json:"edge_node_id"`
	EdgeProcessEpoch     string    `json:"edge_process_epoch"`
	Protocol             string    `json:"protocol"`
	Method               string    `json:"method,omitempty"`
	Host                 string    `json:"host,omitempty"`
	Path                 string    `json:"path,omitempty"`
	IdempotencyKey       string    `json:"idempotency_key"`
	RequestID            string    `json:"request_id"`
	CorrelationID        string    `json:"correlation_id"`
}

// PrivateAccessGrantResponse carries the signed grant and the server-normalized request it signs.
type PrivateAccessGrantResponse struct {
	Schema        string               `json:"schema"`
	Kind          string               `json:"kind"`
	Grant         string               `json:"grant"`
	ExpiresAt     time.Time            `json:"expires_at"`
	RequestID     string               `json:"request_id"`
	CorrelationID string               `json:"correlation_id"`
	Request       PrivateAccessRequest `json:"request"`
}

// PrivateAccessDecision is what the server answers an edge's authorize call with. It carries no token,
// credential or target secret; the edge asks again after ExpiresAt.
type PrivateAccessDecision struct {
	Ingress              *IngressDecision `json:"ingress,omitempty"`
	Schema               string           `json:"schema"`
	Kind                 string           `json:"kind"`
	DecisionID           string           `json:"decision_id"`
	Allowed              bool             `json:"allowed"`
	Reason               string           `json:"reason"`
	ExpiresAt            time.Time        `json:"expires_at"`
	RequestID            string           `json:"request_id"`
	CorrelationID        string           `json:"correlation_id"`
	ResourceKind         string           `json:"resource_kind,omitempty"`
	ResourceID           string           `json:"resource_id,omitempty"`
	RouteID              string           `json:"route_id,omitempty"`
	OperationID          string           `json:"operation_id,omitempty"`
	ConnectorID          string           `json:"connector_id,omitempty"`
	CarrierSessionID     string           `json:"carrier_session_id,omitempty"`
	RouteGeneration      uint64           `json:"route_generation,omitempty"`
	SessionGeneration    uint64           `json:"session_generation,omitempty"`
	ProcessGeneration    uint64           `json:"process_generation,omitempty"`
	ConfigGeneration     uint64           `json:"config_generation,omitempty"`
	AssignmentGeneration uint64           `json:"assignment_generation,omitempty"`
	Protocol             string           `json:"protocol,omitempty"`
}
