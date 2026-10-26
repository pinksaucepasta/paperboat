// Package hoststate owns the durable local cache used by the Paperboat host
// runtime to resume tunnel reconciliation. The server remains authoritative.
package hoststate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	Schema        = "paperboat.host-state"
	SchemaVersion = 1
	MaxStateBytes = 8 << 20

	maxTunnels           = 1_000
	maxRoutes            = 20_000
	maxConnectors        = 10_000
	maxJournalEntries    = 2_000
	maxSnapshotBytes     = 4 << 20
	maxCredentialRefSize = 512
	maxJSONDepth         = 64
)

var (
	ErrInvalidState         = errors.New("invalid host state")
	ErrCredentialMaterial   = errors.New("reusable credential material is forbidden in host state")
	idPattern               = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	codePattern             = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)
	hostnamePattern         = regexp.MustCompile(`^[a-z0-9.-]+$`)
	stableEndpointIDPattern = regexp.MustCompile(`^endpoint_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

type ConfigSnapshot struct {
	TunnelID    string          `json:"tunnel_id"`
	Generation  uint64          `json:"generation"`
	ContentHash string          `json:"content_hash"`
	Payload     json.RawMessage `json:"payload"`
}

// TunnelConfigSnapshot is the only configuration payload accepted by the
// durable host cache. It mirrors the connector-v1 wire vocabulary emitted by
// paperboat-server. References may identify a platform credential, but never
// contain reusable credential material.
type TunnelConfigSnapshot struct {
	Schema           string              `json:"schema"`
	Kind             string              `json:"kind"`
	TunnelID         string              `json:"tunnel_id"`
	Generation       uint64              `json:"generation"`
	Name             string              `json:"name"`
	DesiredState     string              `json:"desired_state"`
	AccessMode       string              `json:"access_mode"`
	StableEndpointID string              `json:"stable_endpoint_id"`
	StableEndpoint   string              `json:"stable_endpoint"`
	ExpiresAt        *time.Time          `json:"expires_at"`
	Routes           []TunnelConfigRoute `json:"routes"`
}

type TunnelConfigRoute struct {
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

// NewConfigSnapshot canonicalizes the JSON before hashing it. It rejects
// preview leases and secret-bearing fields so durable host state cannot become
// an alternate plaintext credential store.
func NewConfigSnapshot(tunnelID string, generation uint64, payload []byte) (ConfigSnapshot, error) {
	if !validID(tunnelID) || generation == 0 || len(payload) == 0 || len(payload) > maxSnapshotBytes {
		return ConfigSnapshot{}, ErrInvalidState
	}
	canonical, err := canonicalSafeJSON(payload)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if _, err := parseTunnelConfigSnapshot(canonical, tunnelID, generation); err != nil {
		return ConfigSnapshot{}, err
	}
	digest := sha256.Sum256(canonical)
	return ConfigSnapshot{
		TunnelID: tunnelID, Generation: generation,
		ContentHash: "sha256:" + hex.EncodeToString(digest[:]),
		Payload:     append(json.RawMessage(nil), canonical...),
	}, nil
}

// ParseTunnelConfigSnapshot strictly decodes the canonical server snapshot
// and verifies the expected tunnel and generation. Callers should hash the
// returned canonical bytes through NewConfigSnapshot rather than re-encoding
// the decoded struct.
func ParseTunnelConfigSnapshot(payload []byte, tunnelID string, generation uint64) (TunnelConfigSnapshot, error) {
	if !validID(tunnelID) || generation == 0 || len(payload) == 0 || len(payload) > maxSnapshotBytes {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	canonical, err := canonicalSafeJSON(payload)
	if err != nil {
		return TunnelConfigSnapshot{}, err
	}
	return parseTunnelConfigSnapshot(canonical, tunnelID, generation)
}

func parseTunnelConfigSnapshot(canonical []byte, tunnelID string, generation uint64) (TunnelConfigSnapshot, error) {
	if len(canonical) == 0 {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &fields); err != nil {
		return TunnelConfigSnapshot{}, safeStoreFailure("tunnel configuration snapshot shape is invalid", ErrInvalidState, err)
	}
	if fields == nil {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	for _, field := range []string{"schema", "kind", "tunnel_id", "generation", "name", "desired_state", "access_mode", "stable_endpoint_id", "stable_endpoint", "expires_at", "routes"} {
		if _, ok := fields[field]; !ok {
			return TunnelConfigSnapshot{}, fmt.Errorf("%w: snapshot field %s is required", ErrInvalidState, field)
		}
	}
	var snapshot TunnelConfigSnapshot
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return TunnelConfigSnapshot{}, safeStoreFailure("tunnel configuration snapshot shape is invalid", ErrInvalidState, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return TunnelConfigSnapshot{}, safeStoreFailure("tunnel configuration snapshot has trailing data", ErrInvalidState, err)
		}
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	if snapshot.Schema != "paperboat.preview-tunnel/v1" || snapshot.Kind != "tunnel_config_snapshot" || snapshot.TunnelID != tunnelID || snapshot.Generation != generation {
		return TunnelConfigSnapshot{}, fmt.Errorf("%w: snapshot identity mismatch", ErrInvalidState)
	}
	if strings.TrimSpace(snapshot.Name) != snapshot.Name || len(snapshot.Name) == 0 || len(snapshot.Name) > 80 || strings.TrimSpace(snapshot.StableEndpoint) != snapshot.StableEndpoint {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	if snapshot.DesiredState != "active" && snapshot.DesiredState != "paused" && snapshot.DesiredState != "deleted" {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	if snapshot.AccessMode != "public" && snapshot.AccessMode != "private" && snapshot.AccessMode != "team" {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	if !validStableEndpoint(snapshot.StableEndpoint) || snapshot.Routes == nil {
		return TunnelConfigSnapshot{}, ErrInvalidState
	}
	if err := ValidateStableEndpointID(snapshot.StableEndpointID); err != nil {
		return TunnelConfigSnapshot{}, err
	}
	for index, route := range snapshot.Routes {
		routeFields, ok := fields["routes"]
		if !ok || routeFields == nil {
			return TunnelConfigSnapshot{}, ErrInvalidState
		}
		var rawRoutes []json.RawMessage
		if err := json.Unmarshal(routeFields, &rawRoutes); err != nil {
			return TunnelConfigSnapshot{}, safeStoreFailure("tunnel configuration snapshot routes are invalid", ErrInvalidState, err)
		}
		if len(rawRoutes) != len(snapshot.Routes) {
			return TunnelConfigSnapshot{}, ErrInvalidState
		}
		var routeObject map[string]json.RawMessage
		if err := json.Unmarshal(rawRoutes[index], &routeObject); err != nil {
			return TunnelConfigSnapshot{}, safeStoreFailure("tunnel configuration snapshot route is invalid", ErrInvalidState, err)
		}
		if routeObject == nil {
			return TunnelConfigSnapshot{}, ErrInvalidState
		}
		for _, field := range []string{"id", "name", "protocol", "match_type", "path_prefix", "origin_scheme", "origin_address", "preserve_host", "host_override", "tls_verification", "tls_server_name", "ca_reference", "mtls_credential_reference", "connect_timeout_ms", "idle_timeout_ms", "max_concurrent_streams", "desired_state"} {
			if _, ok := routeObject[field]; !ok {
				return TunnelConfigSnapshot{}, fmt.Errorf("%w: route field %s is required", ErrInvalidState, field)
			}
		}
		if err := validateTunnelConfigRoute(route); err != nil {
			return TunnelConfigSnapshot{}, safeStoreFailure("tunnel configuration snapshot route is invalid", ErrInvalidState, err)
		}
	}
	return snapshot, nil
}

func validStableEndpoint(value string) bool {
	if len(value) < len("https://a") || len(value) > 264 {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" || parsed.Host != strings.ToLower(parsed.Host) || !hostnamePattern.MatchString(parsed.Host) {
		return false
	}
	return true
}

// ValidateStableEndpointID accepts only the canonical endpoint_UUIDv4 used as
// the immutable managed tunnel endpoint identity. Host runtime state must not
// invent this identity from a display name, host name, or endpoint hash.
func ValidateStableEndpointID(value string) error {
	if !stableEndpointIDPattern.MatchString(value) {
		return ErrInvalidState
	}
	return nil
}

func validateTunnelConfigRoute(route TunnelConfigRoute) error {
	if !validID(route.ID) || strings.TrimSpace(route.Name) != route.Name || len(route.Name) == 0 || len(route.Name) > 80 || strings.TrimSpace(route.OriginAddress) != route.OriginAddress || len(route.OriginAddress) == 0 || len(route.OriginAddress) > 512 || strings.ContainsAny(route.OriginAddress, "\r\n@") {
		return ErrInvalidState
	}
	switch route.Protocol {
	case "http":
		if route.OriginScheme == "tcp" {
			return ErrInvalidState
		}
	case "tcp_private", "tcp", "tls":
		if route.OriginScheme != "tcp" {
			return ErrInvalidState
		}
	default:
		return ErrInvalidState
	}
	switch route.MatchType {
	case "managed_exact", "exact":
		if route.MatchHostname == "" || route.WildcardSuffix != "" || !validMatchHostname(route.MatchHostname) {
			return ErrInvalidState
		}
	case "one_label_wildcard":
		if route.MatchHostname != "" || route.WildcardSuffix == "" || !validMatchHostname(route.WildcardSuffix) {
			return ErrInvalidState
		}
	case "catch_all":
		if route.MatchHostname != "" || route.WildcardSuffix != "" {
			return ErrInvalidState
		}
	default:
		return ErrInvalidState
	}
	switch route.OriginScheme {
	case "http", "https", "h2c", "unix", "tcp":
	default:
		return ErrInvalidState
	}
	if route.TLSVerification != "not_applicable" && route.TLSVerification != "system" && route.TLSVerification != "custom_ca" && route.TLSVerification != "insecure_development" {
		return ErrInvalidState
	}
	if route.OriginScheme != "https" && route.TLSVerification != "not_applicable" {
		return ErrInvalidState
	}
	if route.OriginScheme == "https" {
		if route.TLSVerification == "not_applicable" || route.TLSVerification == "custom_ca" && route.CAReference == nil || route.TLSVerification != "custom_ca" && route.CAReference != nil {
			return ErrInvalidState
		}
	} else if route.CAReference != nil || route.MTLSCredentialReference != nil || route.TLSServerName != nil {
		return ErrInvalidState
	}
	if route.PathPrefix != nil && (len(*route.PathPrefix) == 0 || len(*route.PathPrefix) > 512 || !strings.HasPrefix(*route.PathPrefix, "/") || strings.ContainsAny(*route.PathPrefix, "\r\n")) {
		return ErrInvalidState
	}
	if route.HostOverride != nil && (len(*route.HostOverride) == 0 || len(*route.HostOverride) > 253 || strings.TrimSpace(*route.HostOverride) != *route.HostOverride) {
		return ErrInvalidState
	}
	if route.TLSServerName != nil && (len(*route.TLSServerName) == 0 || len(*route.TLSServerName) > 253 || strings.TrimSpace(*route.TLSServerName) != *route.TLSServerName || strings.ContainsAny(*route.TLSServerName, "\r\n")) {
		return ErrInvalidState
	}
	for _, reference := range map[string]*string{"ca_reference": route.CAReference, "mtls_credential_reference": route.MTLSCredentialReference} {
		if reference != nil && !safeSnapshotCredentialReference(*reference) {
			return safeStoreFailure("tunnel configuration contains an unsafe credential reference", ErrCredentialMaterial)
		}
	}
	if route.ConnectTimeoutMs < 100 || route.ConnectTimeoutMs > 120000 || route.IdleTimeoutMs < 1000 || route.IdleTimeoutMs > 3600000 || route.MaxConcurrentStreams < 1 || route.MaxConcurrentStreams > 100000 {
		return ErrInvalidState
	}
	if route.DesiredState != "active" && route.DesiredState != "disabled" && route.DesiredState != "deleted" {
		return ErrInvalidState
	}
	return nil
}

func validMatchHostname(value string) bool {
	return len(value) <= 253 && strings.TrimSpace(value) == value && value == strings.ToLower(value) && !strings.HasSuffix(value, ".") && !strings.Contains(value, "*") && hostnamePattern.MatchString(value)
}

func canonicalSafeJSON(payload []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder, 0)
	if err != nil {
		return nil, safeStoreFailure("tunnel configuration snapshot JSON is invalid", ErrInvalidState, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, safeStoreFailure("tunnel configuration snapshot has trailing data", ErrInvalidState, err)
		}
		return nil, ErrInvalidState
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		return nil, ErrInvalidState
	}
	if err := rejectCredentialMaterial(value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, safeStoreFailure("tunnel configuration snapshot could not be canonicalized", ErrInvalidState, err)
	}
	if len(canonical) > maxSnapshotBytes {
		return nil, ErrInvalidState
	}
	return canonical, nil
}

func decodeJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, fmt.Errorf("JSON nesting exceeds %d", maxJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, ErrInvalidState
			}
			if _, duplicate := object[key]; duplicate {
				return nil, ErrInvalidState
			}
			child, err := decodeJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		if token, err := decoder.Token(); err != nil {
			return nil, err
		} else if token != json.Delim('}') {
			return nil, ErrInvalidState
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			child, err := decodeJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		if token, err := decoder.Token(); err != nil {
			return nil, err
		} else if token != json.Delim(']') {
			return nil, ErrInvalidState
		}
		return array, nil
	default:
		return nil, ErrInvalidState
	}
}

func rejectCredentialMaterial(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if err := rejectCredentialMaterial(child); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if normalized == "kind" && child == "preview_lease" {
				return fmt.Errorf("%w: preview leases are not durable", ErrInvalidState)
			}
			if secretReferenceKey(normalized) {
				if child == nil {
					continue
				}
				reference, ok := child.(string)
				if !ok || !safeSnapshotCredentialReference(reference) {
					return ErrCredentialMaterial
				}
			}
			if forbiddenCredentialKey(normalized) {
				return ErrCredentialMaterial
			}
			if err := rejectCredentialMaterial(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func safeSnapshotCredentialReference(reference string) bool {
	if len(reference) == 0 || len(reference) > maxCredentialRefSize || strings.TrimSpace(reference) != reference {
		return false
	}
	parsed, err := url.Parse(reference)
	if err != nil || parsed.User != nil || parsed.Opaque != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host != "paperboat" || parsed.Path == "" || parsed.Path == "/" || path.Clean(parsed.Path) != parsed.Path {
		return false
	}
	switch parsed.Scheme {
	case "keychain", "credential-manager", "secret-service", "protected-file", "tpm":
		return true
	}
	return false
}

func secretReferenceKey(key string) bool {
	if strings.Contains(key, "credential_reference") {
		return true
	}
	base, found := strings.CutSuffix(key, "_reference")
	return found && forbiddenCredentialKey(base)
}

func forbiddenCredentialKey(key string) bool {
	if strings.HasSuffix(key, "_reference") {
		return false
	}
	switch key {
	case "authorization", "proxy_authorization", "token", "access_token", "refresh_token", "bearer_token", "api_key", "password", "secret", "credential", "credential_secret", "private_key", "client_secret":
		return true
	}
	return strings.HasSuffix(key, "_token") || strings.HasSuffix(key, "_password") || strings.HasSuffix(key, "_secret") || strings.HasSuffix(key, "_private_key")
}

func validID(value string) bool { return idPattern.MatchString(value) }
