// Package tunnelv1 is the durable-tunnel resource wire the control plane serves and the CLI reads: tunnels, routes,
// domains, health, logs, events and operations. The server's output defines every document; the CLI decodes them
// tolerantly (shared/wirejson) and validates what it uses. Request inputs stay per side: the server's are flattened and
// carry the idempotency mutation, the CLI's nested and optional.
package tunnelv1

import (
	"time"

	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
)

// Tunnel is the safe durable tunnel representation. It contains no
// reusable credential material.
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
	Total      int      `json:"total"` // every tunnel the cursor pages through with these filters
}

type HealthDimension struct {
	Status string `json:"status"`
	Code   string `json:"code"`
}

type HealthDimensions struct {
	Service     HealthDimension `json:"service"`
	Edge        HealthDimension `json:"edge"`
	Config      HealthDimension `json:"config"`
	Route       HealthDimension `json:"route"`
	Origin      HealthDimension `json:"origin"`
	DNS         HealthDimension `json:"dns"`
	Certificate HealthDimension `json:"certificate"`
	Access      HealthDimension `json:"access"`
	Update      HealthDimension `json:"update"`
}

type Health struct {
	Schema        string           `json:"schema"`
	Kind          string           `json:"kind"`
	ResourceKind  string           `json:"resource_kind"`
	ResourceID    string           `json:"resource_id"`
	OverallCode   string           `json:"overall_code"`
	Dimensions    HealthDimensions `json:"dimensions"`
	Summary       string           `json:"summary"`
	Since         time.Time        `json:"since"`
	Retrying      bool             `json:"retrying"`
	NextRetryAt   *time.Time       `json:"next_retry_at,omitempty"`
	RepairAction  string           `json:"repair_action"`
	CorrelationID string           `json:"correlation_id"`
	// FloodProtection is set when Paperboat's own edge limits refused some of this tunnel's traffic
	// recently, so the owner learns why visitors saw "busy" (edge abuse plan 6.4).
	FloodProtection *FloodProtection `json:"flood_protection,omitempty"`
}

// FloodProtection counts what the edges refused for flood protection: client addresses over their
// per-address limit, or the link over its share of an edge.
type FloodProtection struct {
	Refused int64     `json:"refused"`
	Since   time.Time `json:"since"`
	Summary string    `json:"summary"`
}

type RouteHostMatch struct {
	Type           string `json:"type"`
	Hostname       string `json:"hostname,omitempty"`
	WildcardLabels *int   `json:"wildcard_labels,omitempty"`
}

type RouteTLS struct {
	Verification              string  `json:"verification"`
	ServerName                *string `json:"server_name,omitempty"`
	CA                        *string `json:"ca_pem,omitempty"`
	ClientCredentialReference *string `json:"client_credential_reference,omitempty"`
	Pin                       *string `json:"pin,omitempty"`
}

type RouteOrigin struct {
	Scheme       string    `json:"scheme"`
	Address      string    `json:"address"`
	PreserveHost bool      `json:"preserve_host"`
	HostOverride *string   `json:"host_override,omitempty"`
	TLS          *RouteTLS `json:"tls,omitempty"`
}

type Route struct {
	Schema               string         `json:"schema"`
	Kind                 string         `json:"kind"`
	ID                   string         `json:"id"`
	TunnelID             string         `json:"tunnel_id"`
	Name                 string         `json:"name"`
	Protocol             string         `json:"protocol"`
	HostMatch            RouteHostMatch `json:"host_match"`
	PathPrefix           *string        `json:"path_prefix,omitempty"`
	Origin               RouteOrigin    `json:"origin"`
	Priority             int32          `json:"priority"`
	ConnectTimeoutMS     int32          `json:"connect_timeout_ms"`
	IdleTimeoutMS        int32          `json:"idle_timeout_ms"`
	MaxConcurrentStreams int32          `json:"max_concurrent_streams"`
	DesiredState         string         `json:"desired_state"`
	Generation           int64          `json:"generation"`
	ETag                 string         `json:"etag"`
	PublicTCPListenerID  string         `json:"public_tcp_listener_id,omitempty"`
	PublicTCPPort        int32          `json:"public_tcp_port,omitempty"`
	// RewriteRules are the route's header, path and redirect rules, omitted when there are none.
	RewriteRules *tunnelrouterules.Rules `json:"rewrite_rules,omitempty"`
}

type RoutePage struct {
	Items      []Route `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type DomainDNS struct {
	Target          string     `json:"target"`
	ObservedRecords []string   `json:"observed_records"`
	LastCheckedAt   *time.Time `json:"last_checked_at,omitempty"`
}

type DomainCertificate struct {
	State     string         `json:"state"`
	Reference string         `json:"reference,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	Failure   map[string]any `json:"failure,omitempty"`
}

type Domain struct {
	Schema              string            `json:"schema"`
	Kind                string            `json:"kind"`
	ID                  string            `json:"id"`
	AccountID           string            `json:"account_id"`
	TunnelID            string            `json:"tunnel_id"`
	RouteID             string            `json:"route_id"`
	Hostname            string            `json:"hostname"`
	MatchType           string            `json:"match_type"`
	WildcardLabels      *int              `json:"wildcard_labels,omitempty"`
	CertificateStrategy string            `json:"certificate_strategy"`
	State               string            `json:"state"`
	DNS                 DomainDNS         `json:"dns"`
	Certificate         DomainCertificate `json:"certificate"`
	Generation          int64             `json:"generation"`
	ETag                string            `json:"etag"`
}

type DomainPage struct {
	Items      []Domain `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type LogEntry struct {
	Schema        string         `json:"schema"`
	Kind          string         `json:"kind"`
	ID            string         `json:"id"`
	TunnelID      string         `json:"tunnel_id,omitempty"`
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

type LogPage struct {
	Items      []LogEntry `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type Operation struct {
	Schema        string     `json:"schema"`
	Kind          string     `json:"kind"`
	ID            string     `json:"id"`
	ResourceKind  string     `json:"resource_kind"`
	ResourceID    string     `json:"resource_id"`
	Phase         string     `json:"phase"`
	State         string     `json:"state"`
	Progress      int        `json:"progress"`
	Retrying      bool       `json:"retrying"`
	NextRetryAt   *time.Time `json:"next_retry_at,omitempty"`
	Error         *APIError  `json:"error,omitempty"`
	CorrelationID string     `json:"correlation_id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type EventActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Event struct {
	Schema        string         `json:"schema"`
	Kind          string         `json:"kind"`
	ID            string         `json:"id"`
	Cursor        string         `json:"cursor"`
	EventType     string         `json:"event_type"`
	ResourceKind  string         `json:"resource_kind"`
	ResourceID    string         `json:"resource_id"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Actor         EventActor     `json:"actor"`
	CorrelationID string         `json:"correlation_id"`
	SafeMetadata  map[string]any `json:"safe_metadata"`
}

type EventPage struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// APIError is the shared error contract. Message is safe for users and all
// branching fields are typed so callers never inspect the message text.
type APIError struct {
	Schema        string         `json:"schema"`
	Kind          string         `json:"kind"`
	Code          string         `json:"code"`
	Component     string         `json:"component"`
	Message       string         `json:"message"`
	Outcome       string         `json:"outcome"`
	Retryable     bool           `json:"retryable"`
	RetryAt       *time.Time     `json:"retry_at,omitempty"`
	RepairAction  string         `json:"repair_action,omitempty"`
	RequestID     string         `json:"request_id"`
	CorrelationID string         `json:"correlation_id"`
	Details       map[string]any `json:"details,omitempty"`
}
