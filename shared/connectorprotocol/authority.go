package connectorprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AuthRequest struct {
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	Nonce                 string    `json:"nonce"`
	SignedProof           string    `json:"signed_proof"`
	IssuedAt              time.Time `json:"issued_at"`
	ExpiresAt             time.Time `json:"expires_at"`
}

type authProofTranscript struct {
	Domain                string    `json:"domain"`
	Protocol              string    `json:"protocol"`
	Version               string    `json:"version"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	SessionID             string    `json:"session_id,omitempty"`
	Nonce                 string    `json:"nonce"`
	IssuedAt              time.Time `json:"issued_at,omitempty"`
	ExpiresAt             time.Time `json:"expires_at,omitempty"`
	RequestedAt           time.Time `json:"requested_at,omitempty"`
}

func AuthProofPayload(request AuthRequest) ([]byte, error) {
	if err := request.validate(time.Time{}, false); err != nil {
		return nil, err
	}
	return json.Marshal(authProofTranscript{
		Domain: "paperboat.connector.auth.v1", Protocol: ProtocolName, Version: ProtocolVersion,
		AccountID: request.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID,
		HostID: request.HostID, IdentityKeyID: request.IdentityKeyID, IdentityKeyThumbprint: request.IdentityKeyThumbprint,
		ProcessGeneration: request.ProcessGeneration, CredentialGeneration: request.CredentialGeneration,
		Nonce: request.Nonce, IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt,
	})
}

func RenewalProofPayload(request RenewalRequest) ([]byte, error) {
	if err := request.validate(false); err != nil {
		return nil, err
	}
	return json.Marshal(authProofTranscript{
		Domain: "paperboat.connector.auth.renew.v1", Protocol: ProtocolName, Version: ProtocolVersion,
		AccountID: request.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID,
		HostID: request.HostID, IdentityKeyID: request.IdentityKeyID, IdentityKeyThumbprint: request.IdentityKeyThumbprint,
		ProcessGeneration: request.ProcessGeneration, CredentialGeneration: request.CredentialGeneration,
		SessionID: request.SessionID, Nonce: request.Nonce, RequestedAt: request.RequestedAt,
	})
}

func VerifyAuthProof(request AuthRequest, verify VerifyProofFunc) error {
	payload, err := AuthProofPayload(request)
	if err != nil {
		return err
	}
	return VerifyProofPayload(payload, request.SignedProof, verify)
}

func SignAuthProof(request AuthRequest, sign func([]byte) []byte) (AuthRequest, error) {
	payload, err := AuthProofPayload(request)
	if err != nil {
		return AuthRequest{}, err
	}
	request.SignedProof, err = SignProofPayload(payload, sign)
	if err != nil {
		return AuthRequest{}, err
	}
	return request, request.Validate(time.Time{})
}

func VerifyRenewalProof(request RenewalRequest, verify VerifyProofFunc) error {
	payload, err := RenewalProofPayload(request)
	if err != nil {
		return err
	}
	return VerifyProofPayload(payload, request.SignedProof, verify)
}

func SignRenewalProof(request RenewalRequest, sign func([]byte) []byte) (RenewalRequest, error) {
	payload, err := RenewalProofPayload(request)
	if err != nil {
		return RenewalRequest{}, err
	}
	request.SignedProof, err = SignProofPayload(payload, sign)
	if err != nil {
		return RenewalRequest{}, err
	}
	return request, request.Validate()
}

func (a AuthRequest) Validate(now time.Time) error {
	return a.validate(now, true)
}

func (a AuthRequest) validate(now time.Time, requireProof bool) error {
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.HostID) != nil || ValidateIdentityKey(a.IdentityKeyID, a.IdentityKeyThumbprint) != nil {
		return ErrInvalidInput
	}
	if len(a.Nonce) < 16 || len(a.Nonce) > MaxNonceBytes || strings.TrimSpace(a.Nonce) != a.Nonce || strings.ContainsAny(a.Nonce, "\r\n") || a.ProcessGeneration == 0 || a.CredentialGeneration == 0 || a.IssuedAt.IsZero() || a.ExpiresAt.IsZero() || !a.ExpiresAt.After(a.IssuedAt) || a.ExpiresAt.Sub(a.IssuedAt) > 15*time.Minute || requireProof && ValidateProof(a.SignedProof) != nil {
		return ErrInvalidInput
	}
	if !now.IsZero() && (a.IssuedAt.After(now.Add(MaxClockSkew)) || now.Sub(a.IssuedAt) > MaxClockSkew || !a.ExpiresAt.After(now)) {
		return CodeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
	}
	return nil
}

type AuthResult struct {
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	SessionID             string    `json:"session_id,omitempty"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	CredentialExpiresAt   time.Time `json:"credential_expires_at"`
	LeaseExpiresAt        time.Time `json:"lease_expires_at"`
}

func (a AuthResult) Validate(now time.Time) error {
	for _, value := range []string{a.AccountID, a.TunnelID, a.ConnectorID, a.HostID} {
		if ValidateIdentifier(value) != nil {
			return ErrAuthenticationFailed
		}
	}
	if a.SessionID != "" && ValidateIdentifier(a.SessionID) != nil {
		return ErrAuthenticationFailed
	}
	if ValidateIdentityKey(a.IdentityKeyID, a.IdentityKeyThumbprint) != nil || a.ProcessGeneration == 0 || a.CredentialGeneration == 0 || a.CredentialExpiresAt.IsZero() || a.LeaseExpiresAt.IsZero() || a.LeaseExpiresAt.After(a.CredentialExpiresAt) {
		return ErrAuthenticationFailed
	}
	if !now.IsZero() && (!a.CredentialExpiresAt.After(now) || !a.LeaseExpiresAt.After(now) || a.LeaseExpiresAt.Sub(now) > MaxLease) {
		return ErrAuthenticationFailed
	}
	return nil
}

func (a AuthResult) ValidateBound() error {
	if err := a.Validate(time.Time{}); err != nil || ValidateIdentifier(a.SessionID) != nil {
		return ErrAuthenticationFailed
	}
	return nil
}

type RenewalRequest struct {
	SessionID             string    `json:"session_id"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	Nonce                 string    `json:"nonce"`
	SignedProof           string    `json:"signed_proof"`
	RequestedAt           time.Time `json:"requested_at"`
}

func (r RenewalRequest) Validate() error {
	return r.validate(true)
}

// ValidateAt adds freshness checks that cannot be encoded in the detached
// signature alone. Durable server adapters must call this before accepting a
// renewal so an old signed nonce cannot be replayed after a process restart.
func (r RenewalRequest) ValidateAt(now time.Time) error {
	if err := r.validate(true); err != nil {
		return err
	}
	if !now.IsZero() && (r.RequestedAt.After(now.Add(MaxClockSkew)) || now.Sub(r.RequestedAt) > MaxClockSkew) {
		return CodeError(ErrAuthenticationFailed, ReasonAuthentication, false, nil)
	}
	return nil
}

func (r RenewalRequest) validate(requireProof bool) error {
	if ValidateIdentifier(r.SessionID) != nil || ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.ConnectorID) != nil || ValidateIdentifier(r.HostID) != nil || ValidateIdentityKey(r.IdentityKeyID, r.IdentityKeyThumbprint) != nil || r.ProcessGeneration == 0 || r.CredentialGeneration == 0 || len(r.Nonce) < 16 || len(r.Nonce) > MaxNonceBytes || strings.TrimSpace(r.Nonce) != r.Nonce || strings.ContainsAny(r.Nonce, "\r\n") || r.RequestedAt.IsZero() || requireProof && ValidateProof(r.SignedProof) != nil {
		return ErrInvalidInput
	}
	return nil
}

type Lease struct {
	SessionID           string    `json:"session_id"`
	ExpiresAt           time.Time `json:"expires_at"`
	HeartbeatIntervalMS uint32    `json:"heartbeat_interval_ms"`
}

func (l Lease) Validate(now time.Time) error {
	if ValidateIdentifier(l.SessionID) != nil || l.ExpiresAt.IsZero() || l.HeartbeatIntervalMS == 0 {
		return ErrInvalidInput
	}
	if !now.IsZero() && (!l.ExpiresAt.After(now) || l.ExpiresAt.Sub(now) > MaxLease || time.Duration(l.HeartbeatIntervalMS)*time.Millisecond >= l.ExpiresAt.Sub(now)) {
		return ErrInvalidInput
	}
	return nil
}

type Hello struct {
	Protocol          string      `json:"protocol"`
	MinVersion        string      `json:"min_version"`
	MaxVersion        string      `json:"max_version"`
	AccountID         string      `json:"account_id"`
	TunnelID          string      `json:"tunnel_id"`
	ConnectorID       string      `json:"connector_id"`
	HostID            string      `json:"host_id"`
	ProcessGeneration uint64      `json:"process_generation"`
	Capabilities      []string    `json:"capabilities"`
	Auth              AuthRequest `json:"auth"`
}

func (h Hello) Validate(now time.Time) error {
	if h.Protocol != ProtocolName || h.MinVersion == "" || h.MaxVersion == "" || ValidateIdentifier(h.AccountID) != nil || ValidateIdentifier(h.TunnelID) != nil || ValidateIdentifier(h.ConnectorID) != nil || ValidateIdentifier(h.HostID) != nil || h.ProcessGeneration == 0 {
		return ErrInvalidInput
	}
	if _, err := NegotiateVersion(h.MinVersion, h.MaxVersion); err != nil {
		return err
	}
	if err := ValidateCapabilities(h.Capabilities); err != nil {
		return err
	}
	if h.Auth.AccountID != h.AccountID || h.Auth.TunnelID != h.TunnelID || h.Auth.ConnectorID != h.ConnectorID || h.Auth.HostID != h.HostID || h.Auth.ProcessGeneration != h.ProcessGeneration {
		return CodeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	return h.Auth.Validate(now)
}

type Welcome struct {
	Protocol         string    `json:"protocol"`
	Version          string    `json:"version"`
	SessionID        string    `json:"session_id"`
	Capabilities     []string  `json:"capabilities"`
	Lease            Lease     `json:"lease"`
	RequiresSnapshot bool      `json:"requires_snapshot"`
	ServerTime       time.Time `json:"server_time"`
}

func (w Welcome) Validate(now time.Time) error {
	if w.Protocol != ProtocolName || w.Version != ProtocolVersion || ValidateIdentifier(w.SessionID) != nil || !w.RequiresSnapshot || ValidateCapabilities(w.Capabilities) != nil || w.ServerTime.IsZero() || w.Lease.SessionID != w.SessionID {
		return ErrInvalidInput
	}
	return w.Lease.Validate(now)
}

type Snapshot struct {
	AccountID         string          `json:"account_id,omitempty"`
	TunnelID          string          `json:"tunnel_id"`
	ConnectorID       string          `json:"connector_id,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	ProcessGeneration uint64          `json:"process_generation,omitempty"`
	Generation        uint64          `json:"generation"`
	ContentHash       string          `json:"content_hash"`
	Payload           json.RawMessage `json:"payload"`
}

func NewSnapshot(tunnelID string, generation uint64, payload []byte) (Snapshot, error) {
	if ValidateIdentifier(tunnelID) != nil || generation == 0 || len(payload) == 0 || len(payload) > MaxSnapshotBytes {
		return Snapshot{}, ErrInvalidInput
	}
	canonical, err := canonicalJSON(payload, MaxSnapshotBytes)
	if err != nil {
		return Snapshot{}, err
	}
	if err := ValidateConfigSnapshotPayload(canonical, tunnelID, generation); err != nil {
		return Snapshot{}, err
	}
	digest := sha256.Sum256(canonical)
	return Snapshot{TunnelID: tunnelID, Generation: generation, ContentHash: "sha256:" + hex.EncodeToString(digest[:]), Payload: canonical}, nil
}

func (s Snapshot) Validate() error {
	if ValidateIdentifier(s.TunnelID) != nil || s.AccountID != "" && ValidateIdentifier(s.AccountID) != nil || s.SessionID != "" && ValidateIdentifier(s.SessionID) != nil || s.Generation == 0 || !ValidContentHash(s.ContentHash) || len(s.Payload) == 0 || len(s.Payload) > MaxSnapshotBytes {
		return ErrInvalidInput
	}
	canonical, err := canonicalJSON(s.Payload, MaxSnapshotBytes)
	if err != nil || !bytes.Equal(canonical, s.Payload) {
		return CodeError(ErrContentHashMismatch, ReasonSnapshotRejected, false, err)
	}
	digest := sha256.Sum256(canonical)
	if s.ContentHash != "sha256:"+hex.EncodeToString(digest[:]) {
		return CodeError(ErrContentHashMismatch, ReasonSnapshotRejected, false, nil)
	}
	if err := ValidateConfigSnapshotPayload(canonical, s.TunnelID, s.Generation); err != nil {
		return CodeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, err)
	}
	return nil
}

var stableEndpointIDPattern = regexp.MustCompile(`^endpoint_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type wireConfigSnapshot struct {
	Schema           string            `json:"schema"`
	Kind             string            `json:"kind"`
	TunnelID         string            `json:"tunnel_id"`
	Generation       uint64            `json:"generation"`
	Name             string            `json:"name"`
	DesiredState     string            `json:"desired_state"`
	AccessMode       string            `json:"access_mode"`
	StableEndpointID string            `json:"stable_endpoint_id"`
	StableEndpoint   string            `json:"stable_endpoint"`
	ExpiresAt        *time.Time        `json:"expires_at"`
	Routes           []wireConfigRoute `json:"routes"`
}

type wireConfigRoute struct {
	ID                      string  `json:"id"`
	Name                    string  `json:"name"`
	Protocol                string  `json:"protocol"`
	MatchType               string  `json:"match_type"`
	MatchHostname           string  `json:"match_hostname,omitempty"`
	WildcardSuffix          string  `json:"wildcard_suffix,omitempty"`
	PathPrefix              *string `json:"path_prefix"`
	OriginScheme            string  `json:"origin_scheme"`
	OriginAddress           string  `json:"origin_address"`
	PreserveHost            bool    `json:"preserve_host"`
	HostOverride            *string `json:"host_override"`
	TLSVerification         string  `json:"tls_verification"`
	TLSServerName           *string `json:"tls_server_name"`
	CAReference             *string `json:"ca_reference"`
	MTLSCredentialReference *string `json:"mtls_credential_reference"`
	ConnectTimeoutMs        int32   `json:"connect_timeout_ms"`
	IdleTimeoutMs           int32   `json:"idle_timeout_ms"`
	MaxConcurrentStreams    int32   `json:"max_concurrent_streams"`
	DesiredState            string  `json:"desired_state"`
}

func ValidateConfigSnapshotPayload(payload []byte, tunnelID string, generation uint64) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return ErrSnapshotRejected
	}
	for _, field := range []string{"schema", "kind", "tunnel_id", "generation", "name", "desired_state", "access_mode", "stable_endpoint_id", "stable_endpoint", "expires_at", "routes"} {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("%w: snapshot field %s is required", ErrSnapshotRejected, field)
		}
	}
	var snapshot wireConfigSnapshot
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("%w: snapshot shape: %v", ErrSnapshotRejected, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrSnapshotRejected
	}
	if snapshot.Schema != "paperboat.preview-tunnel/v1" || snapshot.Kind != "tunnel_config_snapshot" || snapshot.TunnelID != tunnelID || snapshot.Generation != generation || strings.TrimSpace(snapshot.Name) != snapshot.Name || len(snapshot.Name) == 0 || len(snapshot.Name) > 80 || snapshot.DesiredState != "active" && snapshot.DesiredState != "paused" && snapshot.DesiredState != "deleted" || snapshot.AccessMode != "public" && snapshot.AccessMode != "private" && snapshot.AccessMode != "team" || snapshot.Routes == nil || !stableEndpointIDPattern.MatchString(snapshot.StableEndpointID) || !validWireStableEndpoint(snapshot.StableEndpoint) {
		return ErrSnapshotRejected
	}
	var rawRoutes []json.RawMessage
	if err := json.Unmarshal(fields["routes"], &rawRoutes); err != nil || len(rawRoutes) != len(snapshot.Routes) {
		return ErrSnapshotRejected
	}
	for index, route := range snapshot.Routes {
		var routeFields map[string]json.RawMessage
		if err := json.Unmarshal(rawRoutes[index], &routeFields); err != nil || routeFields == nil {
			return ErrSnapshotRejected
		}
		for _, field := range []string{"id", "name", "protocol", "match_type", "path_prefix", "origin_scheme", "origin_address", "preserve_host", "host_override", "tls_verification", "tls_server_name", "ca_reference", "mtls_credential_reference", "connect_timeout_ms", "idle_timeout_ms", "max_concurrent_streams", "desired_state"} {
			if _, ok := routeFields[field]; !ok {
				return fmt.Errorf("%w: route field %s is required", ErrSnapshotRejected, field)
			}
		}
		if (route.Protocol == "tls" || route.Protocol == "tcp") && snapshot.AccessMode != "public" {
			return ErrSnapshotRejected
		}
		if err := validateWireConfigRoute(route); err != nil {
			return fmt.Errorf("%w: route %d: %v", ErrSnapshotRejected, index, err)
		}
	}
	return nil
}

func validWireStableEndpoint(value string) bool {
	if len(value) < len("https://a") || len(value) > 264 || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" || parsed.Host == "" || parsed.Host != strings.ToLower(parsed.Host) || !wireHostnamePattern(parsed.Host) {
		return false
	}
	return true
}

func wireHostnamePattern(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for index, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' || char == '-') || index == 0 && char == '-' || index == len(value)-1 && char == '-' {
			return false
		}
	}
	return true
}

func validateWireConfigRoute(route wireConfigRoute) error {
	if ValidateIdentifier(route.ID) != nil || strings.TrimSpace(route.Name) != route.Name || len(route.Name) == 0 || len(route.Name) > 80 || strings.TrimSpace(route.OriginAddress) != route.OriginAddress || len(route.OriginAddress) == 0 || len(route.OriginAddress) > 512 || strings.ContainsAny(route.OriginAddress, "\r\n@") {
		return ErrInvalidInput
	}
	switch route.Protocol {
	case "http":
		if route.OriginScheme == "tcp" {
			return ErrInvalidInput
		}
	case "tcp_private":
		if route.OriginScheme != "tcp" {
			return ErrInvalidInput
		}
	case "tcp":
		if route.OriginScheme != "tcp" || route.MatchType != "managed_exact" || route.PathPrefix != nil {
			return ErrInvalidInput
		}
	case "tls":
		if route.OriginScheme != "tcp" || route.MatchType != "managed_exact" || route.MatchHostname == "" || route.WildcardSuffix != "" || route.PathPrefix != nil || route.PreserveHost || route.HostOverride != nil {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	switch route.MatchType {
	case "managed_exact", "exact":
		if route.MatchHostname == "" || route.WildcardSuffix != "" || !wireHostnamePattern(route.MatchHostname) {
			return ErrInvalidInput
		}
	case "one_label_wildcard":
		if route.MatchHostname != "" || route.WildcardSuffix == "" || !wireHostnamePattern(route.WildcardSuffix) {
			return ErrInvalidInput
		}
	case "catch_all":
		if route.MatchHostname != "" || route.WildcardSuffix != "" {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	switch route.OriginScheme {
	case "http", "https", "h2c", "unix", "tcp":
	default:
		return ErrInvalidInput
	}
	switch route.TLSVerification {
	case "not_applicable", "system", "custom_ca", "mutual_tls", "insecure_development":
	default:
		return ErrInvalidInput
	}
	if route.OriginScheme != "https" && route.TLSVerification != "not_applicable" {
		return ErrInvalidInput
	}
	if route.PathPrefix != nil && (len(*route.PathPrefix) == 0 || len(*route.PathPrefix) > 512 || !strings.HasPrefix(*route.PathPrefix, "/") || strings.ContainsAny(*route.PathPrefix, "\r\n")) {
		return ErrInvalidInput
	}
	if route.HostOverride != nil && (len(*route.HostOverride) == 0 || len(*route.HostOverride) > 253 || strings.TrimSpace(*route.HostOverride) != *route.HostOverride) {
		return ErrInvalidInput
	}
	if route.TLSServerName != nil && (len(*route.TLSServerName) == 0 || len(*route.TLSServerName) > 253 || strings.TrimSpace(*route.TLSServerName) != *route.TLSServerName || strings.ContainsAny(*route.TLSServerName, "\r\n")) {
		return ErrInvalidInput
	}
	for _, reference := range []*string{route.CAReference, route.MTLSCredentialReference} {
		if reference != nil && ValidateCredentialReference(*reference) != nil {
			return ErrInvalidInput
		}
	}
	if route.ConnectTimeoutMs < 100 || route.ConnectTimeoutMs > 120000 || route.IdleTimeoutMs < 1000 || route.IdleTimeoutMs > 3600000 || route.MaxConcurrentStreams < 1 || route.MaxConcurrentStreams > 100000 || route.DesiredState != "active" && route.DesiredState != "disabled" && route.DesiredState != "deleted" {
		return ErrInvalidInput
	}
	return nil
}

// ValidateBound requires the identity envelope present on connector wire
// messages. NewSnapshot and server-side sources intentionally remain able to
// construct an unbound value until the authenticated session binds it.
func (s Snapshot) ValidateBound() error {
	if ValidateIdentifier(s.AccountID) != nil || ValidateIdentifier(s.ConnectorID) != nil || ValidateIdentifier(s.SessionID) != nil || s.ProcessGeneration == 0 {
		return ErrInvalidInput
	}
	return s.Validate()
}

type Delta struct {
	AccountID           string          `json:"account_id,omitempty"`
	TunnelID            string          `json:"tunnel_id"`
	ConnectorID         string          `json:"connector_id,omitempty"`
	SessionID           string          `json:"session_id,omitempty"`
	ProcessGeneration   uint64          `json:"process_generation,omitempty"`
	PreviousGeneration  uint64          `json:"previous_generation"`
	Generation          uint64          `json:"generation"`
	PreviousContentHash string          `json:"previous_content_hash"`
	ContentHash         string          `json:"content_hash"`
	Payload             json.RawMessage `json:"payload"`
}

func NewDelta(tunnelID string, previous Snapshot, generation uint64, payload []byte) (Delta, error) {
	if err := previous.Validate(); err != nil || generation != previous.Generation+1 {
		return Delta{}, CodeError(ErrGenerationGap, ReasonGenerationGap, false, err)
	}
	snapshot, err := NewSnapshot(tunnelID, generation, payload)
	if err != nil {
		return Delta{}, err
	}
	return Delta{TunnelID: tunnelID, PreviousGeneration: previous.Generation, Generation: generation, PreviousContentHash: previous.ContentHash, ContentHash: snapshot.ContentHash, Payload: snapshot.Payload}, nil
}

func (d Delta) Validate() error {
	if ValidateIdentifier(d.TunnelID) != nil || d.AccountID != "" && ValidateIdentifier(d.AccountID) != nil || d.SessionID != "" && ValidateIdentifier(d.SessionID) != nil || !ValidContentHash(d.PreviousContentHash) || !ValidContentHash(d.ContentHash) || len(d.Payload) == 0 || len(d.Payload) > MaxDeltaBytes {
		return ErrInvalidInput
	}
	if d.PreviousGeneration == 0 || d.Generation != d.PreviousGeneration+1 {
		return CodeError(ErrGenerationGap, ReasonGenerationGap, true, nil)
	}
	canonical, err := canonicalJSON(d.Payload, MaxDeltaBytes)
	if err != nil || !bytes.Equal(canonical, d.Payload) {
		return CodeError(ErrContentHashMismatch, ReasonGenerationGap, false, err)
	}
	digest := sha256.Sum256(canonical)
	if d.ContentHash != "sha256:"+hex.EncodeToString(digest[:]) {
		return CodeError(ErrContentHashMismatch, ReasonGenerationGap, false, nil)
	}
	if err := ValidateConfigSnapshotPayload(canonical, d.TunnelID, d.Generation); err != nil {
		return CodeError(ErrDeltaRejected, ReasonGenerationGap, false, err)
	}
	return nil
}

func (d Delta) ValidateBound() error {
	if ValidateIdentifier(d.AccountID) != nil || ValidateIdentifier(d.ConnectorID) != nil || ValidateIdentifier(d.SessionID) != nil || d.ProcessGeneration == 0 {
		return ErrInvalidInput
	}
	return d.Validate()
}

type AckKind string

const (
	AckSnapshot AckKind = "snapshot"
	AckDelta    AckKind = "delta"
	AckReady    AckKind = "ready"
	AckRenewal  AckKind = "auth_renew"
)

type AckStatus string

const (
	AckApplied          AckStatus = "applied"
	AckDuplicate        AckStatus = "duplicate"
	AckRejected         AckStatus = "rejected"
	AckSnapshotRequired AckStatus = "snapshot_required"
)

type Ack struct {
	AccountID         string    `json:"account_id"`
	TunnelID          string    `json:"tunnel_id"`
	ConnectorID       string    `json:"connector_id"`
	SessionID         string    `json:"session_id"`
	ProcessGeneration uint64    `json:"process_generation"`
	Kind              AckKind   `json:"kind"`
	Status            AckStatus `json:"status"`
	Generation        uint64    `json:"generation"`
	ContentHash       string    `json:"content_hash"`
	Code              Code      `json:"code,omitempty"`
}

func (a Ack) Validate() error {
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.SessionID) != nil || a.ProcessGeneration == 0 || (a.Kind != AckSnapshot && a.Kind != AckDelta && a.Kind != AckReady && a.Kind != AckRenewal) || (a.Status != AckApplied && a.Status != AckDuplicate && a.Status != AckRejected && a.Status != AckSnapshotRequired) || a.Generation == 0 || !ValidContentHash(a.ContentHash) {
		return ErrInvalidInput
	}
	if a.Status == AckRejected && (a.Code == "" || !ValidCode(a.Code)) || a.Status != AckRejected && a.Code != "" {
		return ErrInvalidInput
	}
	return nil
}

type Readiness struct {
	AccountID         string `json:"account_id"`
	SessionID         string `json:"session_id"`
	TunnelID          string `json:"tunnel_id"`
	ConnectorID       string `json:"connector_id"`
	ProcessGeneration uint64 `json:"process_generation"`
	Generation        uint64 `json:"generation"`
	ContentHash       string `json:"content_hash"`
	EdgeReady         bool   `json:"edge_ready"`
	RouteReady        bool   `json:"route_ready"`
	OriginReady       bool   `json:"origin_ready"`
}

func (r Readiness) Validate() error {
	if ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.SessionID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.ConnectorID) != nil || r.ProcessGeneration == 0 || r.Generation == 0 || !ValidContentHash(r.ContentHash) {
		return ErrInvalidInput
	}
	return nil
}

type Heartbeat struct {
	AccountID             string    `json:"account_id"`
	SessionID             string    `json:"session_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	ProcessGeneration     uint64    `json:"process_generation"`
	LastAppliedGeneration uint64    `json:"last_applied_generation"`
	LastAppliedHash       string    `json:"last_applied_content_hash"`
	SentAt                time.Time `json:"sent_at"`
}

func (h Heartbeat) Validate() error {
	if ValidateIdentifier(h.AccountID) != nil || ValidateIdentifier(h.SessionID) != nil || ValidateIdentifier(h.TunnelID) != nil || ValidateIdentifier(h.ConnectorID) != nil || h.ProcessGeneration == 0 || h.LastAppliedGeneration == 0 || !ValidContentHash(h.LastAppliedHash) || h.SentAt.IsZero() {
		return ErrInvalidInput
	}
	return nil
}

type HeartbeatAck struct {
	AccountID         string    `json:"account_id"`
	TunnelID          string    `json:"tunnel_id"`
	ConnectorID       string    `json:"connector_id"`
	SessionID         string    `json:"session_id"`
	ProcessGeneration uint64    `json:"process_generation"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
	ServerTime        time.Time `json:"server_time"`
}

func (h HeartbeatAck) Validate(now time.Time) error {
	if ValidateIdentifier(h.AccountID) != nil || ValidateIdentifier(h.TunnelID) != nil || ValidateIdentifier(h.ConnectorID) != nil || ValidateIdentifier(h.SessionID) != nil || h.ProcessGeneration == 0 || h.LeaseExpiresAt.IsZero() || !h.LeaseExpiresAt.After(now) || h.ServerTime.IsZero() {
		return ErrInvalidInput
	}
	return nil
}

// DrainStatus is the finite state reported by a connector while it stops
// admitting new streams and lets existing streams finish.
type DrainStatus string

const (
	DrainAccepted  DrainStatus = "accepted"
	DrainProgress  DrainStatus = "progress"
	DrainCompleted DrainStatus = "completed"
	DrainForced    DrainStatus = "forced_close"
	DrainRejected  DrainStatus = "rejected"
)

const MaxActiveStreams = 1 << 20

// Drain is a server-to-connector request. The target generation and hash are
// part of the request so a stale process cannot drain a replacement session or
// a different configuration. StopNewStreams is deliberately required: a
// request that only looks like a status probe is not a drain operation.
type Drain struct {
	AccountID          string    `json:"account_id"`
	TunnelID           string    `json:"tunnel_id"`
	ConnectorID        string    `json:"connector_id"`
	SessionID          string    `json:"session_id"`
	ProcessGeneration  uint64    `json:"process_generation"`
	DrainID            string    `json:"drain_id"`
	Generation         uint64    `json:"generation"`
	ContentHash        string    `json:"content_hash"`
	Deadline           time.Time `json:"deadline"`
	StopNewStreams     bool      `json:"stop_new_streams"`
	ForceAfterDeadline bool      `json:"force_after_deadline"`
}

func (d Drain) Validate(now time.Time) error {
	if ValidateIdentifier(d.AccountID) != nil || ValidateIdentifier(d.TunnelID) != nil || ValidateIdentifier(d.ConnectorID) != nil || ValidateIdentifier(d.SessionID) != nil || ValidateIdentifier(d.DrainID) != nil || d.ProcessGeneration == 0 || d.Generation == 0 || !ValidContentHash(d.ContentHash) || d.Deadline.IsZero() || !d.StopNewStreams {
		return ErrInvalidInput
	}
	if !now.IsZero() && (!d.Deadline.After(now) || d.Deadline.Sub(now) > MaxLease) {
		return ErrInvalidInput
	}
	return nil
}

// DrainAck is emitted first when the connector accepts the drain and again as
// streams leave. Completion is explicit. A forced close is represented by a
// distinct status and code, so callers do not have to infer it from a message.
type DrainAck struct {
	AccountID         string      `json:"account_id"`
	TunnelID          string      `json:"tunnel_id"`
	ConnectorID       string      `json:"connector_id"`
	SessionID         string      `json:"session_id"`
	ProcessGeneration uint64      `json:"process_generation"`
	DrainID           string      `json:"drain_id"`
	Generation        uint64      `json:"generation"`
	ContentHash       string      `json:"content_hash"`
	Status            DrainStatus `json:"status"`
	ActiveStreams     uint32      `json:"active_streams"`
	ForcedClose       bool        `json:"forced_close"`
	Code              Code        `json:"code,omitempty"`
}

func (a DrainAck) Validate(now time.Time) error {
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.SessionID) != nil || ValidateIdentifier(a.DrainID) != nil || a.ProcessGeneration == 0 || a.Generation == 0 || !ValidContentHash(a.ContentHash) || a.ActiveStreams > MaxActiveStreams {
		return ErrInvalidInput
	}
	switch a.Status {
	case DrainAccepted, DrainProgress:
		if a.ForcedClose || a.Code != "" {
			return ErrInvalidInput
		}
	case DrainCompleted:
		if a.ActiveStreams != 0 || a.ForcedClose || a.Code != "" {
			return ErrInvalidInput
		}
	case DrainForced:
		if !a.ForcedClose || a.Code != CodeDrainTimeout || a.ActiveStreams != 0 {
			return ErrInvalidInput
		}
	case DrainRejected:
		if a.ForcedClose || a.Code != CodeDrainRejected {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	if a.Code != "" && !ValidCode(a.Code) {
		return ErrInvalidInput
	}
	_ = now
	return nil
}

type Disconnect struct {
	AccountID         string           `json:"account_id"`
	TunnelID          string           `json:"tunnel_id"`
	ConnectorID       string           `json:"connector_id"`
	SessionID         string           `json:"session_id"`
	ProcessGeneration uint64           `json:"process_generation"`
	Reason            DisconnectReason `json:"reason"`
	Retryable         bool             `json:"retryable"`
	Message           string           `json:"message,omitempty"`
}

type Reject struct {
	AccountID         string           `json:"account_id"`
	TunnelID          string           `json:"tunnel_id"`
	ConnectorID       string           `json:"connector_id"`
	SessionID         string           `json:"session_id"`
	ProcessGeneration uint64           `json:"process_generation"`
	Code              Code             `json:"code"`
	Reason            DisconnectReason `json:"reason"`
	Retryable         bool             `json:"retryable"`
	Message           string           `json:"message,omitempty"`
}

func (r Reject) Validate() error {
	if ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.ConnectorID) != nil || ValidateIdentifier(r.SessionID) != nil || r.ProcessGeneration == 0 || !ValidCode(r.Code) || !ValidDisconnectReason(r.Reason) || len(r.Message) > MaxReasonMessage {
		return ErrInvalidInput
	}
	return nil
}

func (d Disconnect) Validate() error {
	if ValidateIdentifier(d.AccountID) != nil || ValidateIdentifier(d.TunnelID) != nil || ValidateIdentifier(d.ConnectorID) != nil || ValidateIdentifier(d.SessionID) != nil || d.ProcessGeneration == 0 || !ValidDisconnectReason(d.Reason) || len(d.Message) > MaxReasonMessage {
		return ErrInvalidInput
	}
	return nil
}

func ValidDisconnectReason(reason DisconnectReason) bool {
	switch reason {
	case ReasonProtocolMismatch, ReasonCapabilityMissing, ReasonMalformed, ReasonAuthentication, ReasonCredentialExpired, ReasonLeaseExpired, ReasonHeartbeatTimeout, ReasonSessionReplaced, ReasonStaleGeneration, ReasonSnapshotRejected, ReasonGenerationGap, ReasonCredentialRotation, ReasonCanceled, ReasonServerShutdown, ReasonProtocolClosed:
		return true
	default:
		return false
	}
}

func ValidCode(code Code) bool {
	switch code {
	case CodeInvalidInput, CodeMalformedFrame, CodeFrameTooLarge, CodeProtocolIncompatible, CodeCapabilityMissing, CodeAuthenticationFailed, CodeCredentialExpired, CodeIdentityMismatch, CodeSessionConflict, CodeSessionClosed, CodeLeaseExpired, CodeHeartbeatTimeout, CodeSnapshotRequired, CodeGenerationGap, CodeStaleGeneration, CodeContentHashMismatch, CodeSnapshotRejected, CodeDeltaRejected, CodeNotReady, CodeCanceled, CodeStaleSession, CodeUnsupportedMessage, CodeDrainRejected, CodeDrainTimeout, CodeCredentialRotationRejected, CodeCredentialRotationNotReady, CodeCredentialRotationFailed:
		return true
	default:
		return false
	}
}

type Clock interface{ Now() time.Time }
type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

// canonicalJSON validates duplicate keys, depth, trailing data and forbidden
// secret-bearing fields before a payload is hashed or handed to an applier.
func canonicalJSON(data []byte, limit int) ([]byte, error) {
	if len(data) == 0 || len(data) > limit || rejectDuplicateKeys(data) != nil {
		return nil, ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidInput
	}
	if err := rejectSecretFields(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > limit {
		return nil, ErrInvalidInput
	}
	return encoded, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > MaxJSONDepth {
		return nil, errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key := keyToken.(string)
				child, err := decodeValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return object, nil
		case '[':
			values := make([]any, 0)
			for decoder.More() {
				child, err := decodeValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				values = append(values, child)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return values, nil
		default:
			return nil, errors.New("invalid delimiter")
		}
	default:
		return token, nil
	}
}

func rejectSecretFields(value any) error {
	var visit func(any) error
	visit = func(current any) error {
		switch value := current.(type) {
		case map[string]any:
			for key, child := range value {
				normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
				switch normalized {
				case "accesstoken", "apikey", "authorization", "clientsecret", "cookie", "headers", "password", "privatekey", "refreshtoken", "requestbody", "requestheaders", "responsebody", "responseheaders", "secret", "sessiontoken", "setcookie", "token":
					return ErrInvalidInput
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := visit(child); err != nil {
					return err
				}
			}
		case string:
			upper := strings.ToUpper(strings.TrimSpace(value))
			if strings.HasPrefix(upper, "BEARER ") || strings.Contains(upper, "BEGIN PRIVATE KEY") {
				return ErrInvalidInput
			}
			if parsed, err := url.Parse(value); err == nil && parsed.IsAbs() {
				if parsed.User != nil {
					return ErrInvalidInput
				}
				for key := range parsed.Query() {
					normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
					switch normalized {
					case "accesstoken", "apikey", "authorization", "clientsecret", "password", "refreshtoken", "secret", "sessiontoken", "token":
						return ErrInvalidInput
					}
				}
			}
		}
		return nil
	}
	return visit(value)
}

func NewOpaqueID(prefix string) (string, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return prefix + "_" + value.String(), nil
}
