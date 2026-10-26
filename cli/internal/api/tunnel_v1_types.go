package api

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

const TunnelV1Schema = "paperboat.preview-tunnel/v1"

var tunnelEndpointUUIDPattern = regexp.MustCompile(`^endpoint_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var (
	ErrUnsafeTunnelResponse = errors.New("paperboat-server returned an unsafe tunnel response")
	// ErrTunnelRedirect is returned before a redirected request is issued. A
	// tunnel response can contain credentials or control data, so following a
	// Location header would be an unsafe cross-origin data exfiltration path.
	ErrTunnelRedirect = errors.New("paperboat-server tunnel request was redirected")
)

const (
	// The v1 resource projections are deliberately bounded. Keep enough room
	// for a full page and nested safe metadata while refusing accidental or
	// hostile multi-megabyte responses.
	maxTunnelResponseBytes = 2 << 20
	maxTunnelRequestBytes  = 1 << 20
)

type TunnelOperation struct {
	Schema        string                 `json:"schema"`
	Kind          string                 `json:"kind"`
	ID            string                 `json:"id"`
	ResourceKind  string                 `json:"resource_kind"`
	ResourceID    string                 `json:"resource_id"`
	Phase         string                 `json:"phase"`
	State         string                 `json:"state"`
	Progress      int                    `json:"progress"`
	Retrying      bool                   `json:"retrying"`
	NextRetryAt   *time.Time             `json:"next_retry_at,omitempty"`
	Error         *PreviewTunnelAPIError `json:"error,omitempty"`
	CorrelationID string                 `json:"correlation_id"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

type Tunnel struct {
	Schema           string     `json:"schema"`
	Kind             string     `json:"kind"`
	ID               string     `json:"id"`
	AccountID        string     `json:"account_id"`
	Name             string     `json:"name"`
	DesiredState     string     `json:"desired_state"`
	AccessMode       string     `json:"access_mode"`
	Generation       int64      `json:"generation"`
	ETag             string     `json:"etag"`
	StableEndpointID string     `json:"stable_endpoint_id"`
	StableEndpoint   string     `json:"stable_endpoint"`
	CreatedByHostID  string     `json:"created_by_host_id"`
	CreatedByActorID string     `json:"created_by_actor_id"`
	ExpiresAt        *time.Time `json:"expires_at"`
	SummaryCode      string     `json:"summary_code"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type TunnelPage struct {
	Items      []Tunnel `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
	// Total counts every tunnel matching the filters, not only this page (PQ-04).
	Total int `json:"total"`
}

type TunnelOriginInput struct {
	Scheme       string  `json:"scheme"`
	Address      string  `json:"address"`
	PreserveHost *bool   `json:"preserve_host,omitempty"`
	HostOverride *string `json:"host_override,omitempty"`
}

type TunnelCreateInput struct {
	Workspace  string            `json:"workspace,omitempty"`
	Name       string            `json:"name"`
	AccessMode string            `json:"access_mode,omitempty"`
	Origin     TunnelOriginInput `json:"origin"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
}

type TunnelPatchInput struct {
	Name       *string    `json:"name,omitempty"`
	AccessMode *string    `json:"access_mode,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

type TunnelMutation struct {
	Tunnel    Tunnel          `json:"tunnel"`
	Operation TunnelOperation `json:"operation"`
	Replayed  bool            `json:"replayed"`
	Changed   bool            `json:"changed"`
}

type TunnelRouteHostMatch struct {
	Type           string `json:"type"`
	Hostname       string `json:"hostname,omitempty"`
	WildcardLabels *int   `json:"wildcard_labels,omitempty"`
}

type TunnelRouteTLS struct {
	Verification              string  `json:"verification"`
	ServerName                *string `json:"server_name,omitempty"`
	CAReference               *string `json:"ca_reference,omitempty"`
	ClientCredentialReference *string `json:"client_credential_reference,omitempty"`
}

type TunnelRouteOrigin struct {
	Scheme       string          `json:"scheme"`
	Address      string          `json:"address"`
	PreserveHost bool            `json:"preserve_host"`
	HostOverride *string         `json:"host_override,omitempty"`
	TLS          *TunnelRouteTLS `json:"tls,omitempty"`
}

type TunnelRoute struct {
	Schema               string               `json:"schema"`
	Kind                 string               `json:"kind"`
	ID                   string               `json:"id"`
	TunnelID             string               `json:"tunnel_id"`
	Name                 string               `json:"name"`
	Protocol             string               `json:"protocol"`
	HostMatch            TunnelRouteHostMatch `json:"host_match"`
	PathPrefix           *string              `json:"path_prefix,omitempty"`
	Origin               TunnelRouteOrigin    `json:"origin"`
	Priority             int32                `json:"priority"`
	ConnectTimeoutMS     int32                `json:"connect_timeout_ms"`
	IdleTimeoutMS        int32                `json:"idle_timeout_ms"`
	MaxConcurrentStreams int32                `json:"max_concurrent_streams"`
	DesiredState         string               `json:"desired_state"`
	Generation           int64                `json:"generation"`
	ETag                 string               `json:"etag"`
	PublicTCPListenerID  string               `json:"public_tcp_listener_id,omitempty"`
	PublicTCPPort        int32                `json:"public_tcp_port,omitempty"`
}

type TunnelRoutePage struct {
	Items      []TunnelRoute `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type TunnelRouteInput struct {
	Name                 string               `json:"name"`
	Protocol             string               `json:"protocol"`
	HostMatch            TunnelRouteHostMatch `json:"host_match"`
	PathPrefix           *string              `json:"path_prefix,omitempty"`
	Origin               TunnelRouteOrigin    `json:"origin"`
	Priority             int32                `json:"priority,omitempty"`
	ConnectTimeoutMS     int32                `json:"connect_timeout_ms,omitempty"`
	IdleTimeoutMS        int32                `json:"idle_timeout_ms,omitempty"`
	MaxConcurrentStreams int32                `json:"max_concurrent_streams,omitempty"`
}

type TunnelRoutePatch struct {
	Name                 *string               `json:"name,omitempty"`
	Protocol             *string               `json:"protocol,omitempty"`
	HostMatch            *TunnelRouteHostMatch `json:"host_match,omitempty"`
	PathPrefix           *string               `json:"-"`
	PathPrefixSet        bool                  `json:"-"`
	Origin               *TunnelRouteOrigin    `json:"origin,omitempty"`
	Priority             *int32                `json:"priority,omitempty"`
	ConnectTimeoutMS     *int32                `json:"connect_timeout_ms,omitempty"`
	IdleTimeoutMS        *int32                `json:"idle_timeout_ms,omitempty"`
	MaxConcurrentStreams *int32                `json:"max_concurrent_streams,omitempty"`
	DesiredState         *string               `json:"desired_state,omitempty"`
}

// MarshalJSON preserves the distinction between an omitted path_prefix and
// an explicit null, which clears an existing route path without inventing a
// sentinel string value.
func (p TunnelRoutePatch) MarshalJSON() ([]byte, error) {
	type alias TunnelRoutePatch
	encoded, err := json.Marshal(alias(p))
	if err != nil {
		return nil, err
	}
	if !p.PathPrefixSet && p.PathPrefix == nil {
		return encoded, nil
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if p.PathPrefix == nil {
		fields["path_prefix"] = nil
	} else {
		fields["path_prefix"] = *p.PathPrefix
	}
	return json.Marshal(fields)
}

type TunnelRouteMutation struct {
	Route     TunnelRoute     `json:"route"`
	Operation TunnelOperation `json:"operation"`
	Replayed  bool            `json:"replayed"`
	Changed   bool            `json:"changed"`
}

type TunnelDomain struct {
	Schema              string                  `json:"schema"`
	Kind                string                  `json:"kind"`
	ID                  string                  `json:"id"`
	TargetKind          string                  `json:"target_kind,omitempty"`
	AccountID           string                  `json:"account_id"`
	TunnelID            string                  `json:"tunnel_id"`
	RouteID             string                  `json:"route_id"`
	Hostname            string                  `json:"hostname"`
	MatchType           string                  `json:"match_type"`
	WildcardLabels      *int                    `json:"wildcard_labels,omitempty"`
	CertificateStrategy string                  `json:"certificate_strategy,omitempty"`
	State               string                  `json:"state"`
	DNS                 TunnelDomainDNS         `json:"dns"`
	Certificate         TunnelDomainCertificate `json:"certificate"`
	Generation          int64                   `json:"generation"`
	ETag                string                  `json:"etag"`
}

type TunnelDomainDNS struct {
	Target          string     `json:"target"`
	ObservedRecords []string   `json:"observed_records,omitempty"`
	LastCheckedAt   *time.Time `json:"last_checked_at,omitempty"`
}

type TunnelDomainCertificate struct {
	State     string         `json:"state"`
	Reference string         `json:"reference,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	Failure   map[string]any `json:"failure,omitempty"`
}

type TunnelDomainPage struct {
	Items      []TunnelDomain `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

type TunnelDomainInput struct {
	Hostname            string `json:"hostname"`
	RouteID             string `json:"route_id"`
	Provider            string `json:"provider,omitempty"`
	CertificateStrategy string `json:"certificate_strategy,omitempty"`
}

type TunnelDomainMutation struct {
	Domain    TunnelDomain    `json:"domain"`
	Operation TunnelOperation `json:"operation"`
	Replayed  bool            `json:"replayed"`
	Changed   bool            `json:"changed"`
}

type TunnelDNSRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

type TunnelDNSInstructions struct {
	Schema              string            `json:"schema"`
	Kind                string            `json:"kind"`
	TunnelID            string            `json:"tunnel_id"`
	DomainID            string            `json:"domain_id"`
	Hostname            string            `json:"hostname"`
	Provider            string            `json:"provider"`
	Records             []TunnelDNSRecord `json:"records"`
	CertificateStrategy string            `json:"certificate_strategy"`
	VerificationState   string            `json:"verification_state"`
	PublicTCPPort       int32             `json:"public_tcp_port,omitempty"`
	Note                string            `json:"note"`
}

type TunnelEventActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type TunnelEvent struct {
	Schema        string           `json:"schema"`
	Kind          string           `json:"kind"`
	ID            string           `json:"id"`
	Cursor        string           `json:"cursor"`
	EventType     string           `json:"event_type"`
	ResourceKind  string           `json:"resource_kind"`
	ResourceID    string           `json:"resource_id"`
	OccurredAt    time.Time        `json:"occurred_at"`
	Actor         TunnelEventActor `json:"actor"`
	CorrelationID string           `json:"correlation_id"`
	SafeMetadata  map[string]any   `json:"safe_metadata"`
}

type TunnelEventPage struct {
	Items      []TunnelEvent `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// TunnelPrivateAccessAdmission is the server-authoritative prerequisite set
// needed by a stable host before it can open a private route. It contains
// public routing and generation fences only, never a reusable grant.
type TunnelPrivateAccessAdmission struct {
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
	EdgeCarrierServerSPKISHA256          string    `json:"edge_carrier_server_spki_sha256"`
	EdgeCarrierServerCertificateChainPEM string    `json:"edge_carrier_server_certificate_chain_pem"`
	Protocol                             string    `json:"protocol"`
	Hostname                             string    `json:"hostname,omitempty"`
	MatchType                            string    `json:"match_type"`
	WildcardSuffix                       string    `json:"wildcard_suffix,omitempty"`
	EdgeEndpoints                        []string  `json:"edge_endpoints"`
	ExpiresAt                            time.Time `json:"expires_at"`
	TunnelID                             string    `json:"tunnel_id"`
	CarrierConnectorID                   string    `json:"carrier_connector_id"`
}

type TunnelPrivateAccessSnapshot struct {
	Schema     string                         `json:"schema"`
	Kind       string                         `json:"kind"`
	Complete   bool                           `json:"complete"`
	Admissions []TunnelPrivateAccessAdmission `json:"admissions"`
}

type TunnelHealthDimension struct {
	Status string `json:"status"`
	Code   string `json:"code"`
}

type TunnelHealthDimensions struct {
	Service     TunnelHealthDimension `json:"service"`
	Edge        TunnelHealthDimension `json:"edge"`
	Config      TunnelHealthDimension `json:"config"`
	Route       TunnelHealthDimension `json:"route"`
	Origin      TunnelHealthDimension `json:"origin"`
	DNS         TunnelHealthDimension `json:"dns"`
	Certificate TunnelHealthDimension `json:"certificate"`
	Access      TunnelHealthDimension `json:"access"`
	Update      TunnelHealthDimension `json:"update"`
}

type TunnelHealth struct {
	Schema        string                 `json:"schema"`
	Kind          string                 `json:"kind"`
	ResourceKind  string                 `json:"resource_kind"`
	ResourceID    string                 `json:"resource_id"`
	OverallCode   string                 `json:"overall_code"`
	Dimensions    TunnelHealthDimensions `json:"dimensions"`
	Summary       string                 `json:"summary"`
	Since         time.Time              `json:"since"`
	Retrying      bool                   `json:"retrying"`
	NextRetryAt   *time.Time             `json:"next_retry_at,omitempty"`
	RepairAction  string                 `json:"repair_action"`
	CorrelationID string                 `json:"correlation_id"`
}

type TunnelLogEntry struct {
	Schema        string         `json:"schema"`
	Kind          string         `json:"kind"`
	ID            string         `json:"id"`
	TunnelID      string         `json:"tunnel_id"`
	PreviewID     string         `json:"preview_id,omitempty"`
	RouteID       string         `json:"route_id,omitempty"`
	ConnectorID   string         `json:"connector_id,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	Level         string         `json:"level"`
	Component     string         `json:"component"`
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	Metadata      map[string]any `json:"metadata"`
	CorrelationID string         `json:"correlation_id"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Cursor        string         `json:"cursor"`
}

type TunnelLogPage struct {
	Items      []TunnelLogEntry `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}
