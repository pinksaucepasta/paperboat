package connectorprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

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
		return codeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
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
	if err := validateConfigSnapshotPayload(canonical, tunnelID, generation); err != nil {
		return Snapshot{}, err
	}
	digest := sha256.Sum256(canonical)
	return Snapshot{TunnelID: tunnelID, Generation: generation, ContentHash: "sha256:" + hex.EncodeToString(digest[:]), Payload: canonical}, nil
}

func (s Snapshot) Validate() error {
	if ValidateIdentifier(s.TunnelID) != nil || s.AccountID != "" && ValidateIdentifier(s.AccountID) != nil || s.SessionID != "" && ValidateIdentifier(s.SessionID) != nil || s.Generation == 0 || !hashPattern.MatchString(s.ContentHash) || len(s.Payload) == 0 || len(s.Payload) > MaxSnapshotBytes {
		return ErrInvalidInput
	}
	canonical, err := canonicalJSON(s.Payload, MaxSnapshotBytes)
	if err != nil || !bytes.Equal(canonical, s.Payload) {
		return codeError(ErrContentHashMismatch, ReasonSnapshotRejected, false, err)
	}
	digest := sha256.Sum256(canonical)
	if s.ContentHash != "sha256:"+hex.EncodeToString(digest[:]) {
		return codeError(ErrContentHashMismatch, ReasonSnapshotRejected, false, nil)
	}
	if err := validateConfigSnapshotPayload(canonical, s.TunnelID, s.Generation); err != nil {
		return codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, err)
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

func validateConfigSnapshotPayload(payload []byte, tunnelID string, generation uint64) error {
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
		return Delta{}, codeError(ErrGenerationGap, ReasonGenerationGap, false, err)
	}
	snapshot, err := NewSnapshot(tunnelID, generation, payload)
	if err != nil {
		return Delta{}, err
	}
	return Delta{TunnelID: tunnelID, PreviousGeneration: previous.Generation, Generation: generation, PreviousContentHash: previous.ContentHash, ContentHash: snapshot.ContentHash, Payload: snapshot.Payload}, nil
}

func (d Delta) Validate() error {
	if ValidateIdentifier(d.TunnelID) != nil || d.AccountID != "" && ValidateIdentifier(d.AccountID) != nil || d.SessionID != "" && ValidateIdentifier(d.SessionID) != nil || !hashPattern.MatchString(d.PreviousContentHash) || !hashPattern.MatchString(d.ContentHash) || len(d.Payload) == 0 || len(d.Payload) > MaxDeltaBytes {
		return ErrInvalidInput
	}
	if d.PreviousGeneration == 0 || d.Generation != d.PreviousGeneration+1 {
		return codeError(ErrGenerationGap, ReasonGenerationGap, true, nil)
	}
	canonical, err := canonicalJSON(d.Payload, MaxDeltaBytes)
	if err != nil || !bytes.Equal(canonical, d.Payload) {
		return codeError(ErrContentHashMismatch, ReasonGenerationGap, false, err)
	}
	digest := sha256.Sum256(canonical)
	if d.ContentHash != "sha256:"+hex.EncodeToString(digest[:]) {
		return codeError(ErrContentHashMismatch, ReasonGenerationGap, false, nil)
	}
	if err := validateConfigSnapshotPayload(canonical, d.TunnelID, d.Generation); err != nil {
		return codeError(ErrDeltaRejected, ReasonGenerationGap, false, err)
	}
	return nil
}

func (d Delta) ValidateBound() error {
	if ValidateIdentifier(d.AccountID) != nil || ValidateIdentifier(d.ConnectorID) != nil || ValidateIdentifier(d.SessionID) != nil || d.ProcessGeneration == 0 {
		return ErrInvalidInput
	}
	return d.Validate()
}
