package api

import (
	"encoding/json"
	"errors"
	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
	"github.com/pinksaucepasta/paperboat/shared/tunnelv1"
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
	maxTunnelResponseBytes = clientapi.MaxResponseBytes
	maxTunnelRequestBytes  = 1 << 20
)

type TunnelOperation = tunnelv1.Operation

type Tunnel = tunnelv1.Tunnel

type TunnelPage = tunnelv1.TunnelPage

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

type TunnelRouteHostMatch = tunnelv1.RouteHostMatch

type TunnelRouteTLS = tunnelv1.RouteTLS

type TunnelRouteOrigin = tunnelv1.RouteOrigin

type TunnelRoute = tunnelv1.Route

type TunnelRoutePage = tunnelv1.RoutePage

type TunnelRouteInput struct {
	Name                 string                  `json:"name"`
	Protocol             string                  `json:"protocol"`
	HostMatch            TunnelRouteHostMatch    `json:"host_match"`
	PathPrefix           *string                 `json:"path_prefix,omitempty"`
	Origin               TunnelRouteOrigin       `json:"origin"`
	Priority             int32                   `json:"priority,omitempty"`
	ConnectTimeoutMS     int32                   `json:"connect_timeout_ms,omitempty"`
	IdleTimeoutMS        int32                   `json:"idle_timeout_ms,omitempty"`
	MaxConcurrentStreams int32                   `json:"max_concurrent_streams,omitempty"`
	RewriteRules         *tunnelrouterules.Rules `json:"rewrite_rules,omitempty"`
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
	// RewriteRules replaces the route's rules when RewriteRulesSet; no rules (nil or empty) clears them.
	RewriteRules    *tunnelrouterules.Rules `json:"-"`
	RewriteRulesSet bool                    `json:"-"`
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
	if !p.PathPrefixSet && p.PathPrefix == nil && !p.RewriteRulesSet {
		return encoded, nil
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if p.PathPrefixSet || p.PathPrefix != nil {
		if p.PathPrefix == nil {
			fields["path_prefix"] = nil
		} else {
			fields["path_prefix"] = *p.PathPrefix
		}
	}
	if p.RewriteRulesSet {
		if p.RewriteRules == nil {
			fields["rewrite_rules"] = tunnelrouterules.Rules{}
		} else {
			fields["rewrite_rules"] = p.RewriteRules
		}
	}
	return json.Marshal(fields)
}

type TunnelRouteMutation struct {
	Route     TunnelRoute     `json:"route"`
	Operation TunnelOperation `json:"operation"`
	Replayed  bool            `json:"replayed"`
	Changed   bool            `json:"changed"`
}

type TunnelDomain = tunnelv1.Domain

type TunnelDomainDNS = tunnelv1.DomainDNS

type TunnelDomainCertificate = tunnelv1.DomainCertificate

type TunnelDomainPage = tunnelv1.DomainPage

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

type TunnelEventActor = tunnelv1.EventActor

type TunnelEvent = tunnelv1.Event

type TunnelEventPage = tunnelv1.EventPage

type TunnelHealthDimension = tunnelv1.HealthDimension

type TunnelHealthDimensions = tunnelv1.HealthDimensions

type TunnelHealth = tunnelv1.Health

type TunnelFloodProtection = tunnelv1.FloodProtection

type TunnelLogEntry = tunnelv1.LogEntry

type TunnelLogPage = tunnelv1.LogPage
