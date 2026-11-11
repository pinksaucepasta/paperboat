package peernetwork

import (
	"encoding/base64"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// The signed regional candidate set: the relays one endpoint may use. The server builds it, the mint signs it, every
// CLI and daemon checks it with Check; per-node health (state, capacity, observations) is the client's selection,
// which skips a node, never the set.
const (
	RegionalCandidatesType     = "paperboat-regional-candidates+jwt"
	RegionalCandidatesSchema   = "paperboat.regional-candidates.v1"
	RegionalCandidatesAudience = "paperboat-regional-candidates"
	// MaxRegionalCandidates bounds the nodes one set names.
	MaxRegionalCandidates = 32
	// HostingManaged and HostingSelfHosted say who runs a relay; absent is managed. Unlimited file transfers may
	// use only self-hosted relays.
	HostingManaged    = "managed"
	HostingSelfHosted = "self_hosted"
)

// RegionalNode is one relay of a regional candidate set.
type RegionalNode struct {
	TLSSPKISHA256      string   `json:"tls_spki_sha256,omitempty"`
	NodeID             string   `json:"node_id"`
	NodeGeneration     uint64   `json:"node_generation"`
	ProcessEpoch       string   `json:"process_epoch"`
	Region             string   `json:"region"`
	FailureDomain      string   `json:"failure_domain"`
	Roles              []string `json:"roles"`
	Transports         []string `json:"transports"`
	EndpointHost       string   `json:"endpoint_host"`
	EndpointTCPPort    uint16   `json:"endpoint_tcp_port"`
	EndpointQUICPort   uint16   `json:"endpoint_quic_port"`
	State              string   `json:"state"`
	ObservedAt         int64    `json:"observed_at"`
	ExpiresAt          int64    `json:"expires_at"`
	CapacityLimit      uint64   `json:"capacity_limit"`
	CapacityUsed       uint64   `json:"capacity_used"`
	CapacityObservedAt int64    `json:"capacity_observed_at"`
	DrainDeadline      *int64   `json:"drain_deadline,omitempty"`
	Hosting            string   `json:"hosting,omitempty"`
}

// SelfHosted reports whether the account runs this relay itself. A node that does not say is managed.
func (n RegionalNode) SelfHosted() bool { return n.Hosting == HostingSelfHosted }

// RegionalCandidates is a signed regional candidate set's claims.
type RegionalCandidates struct {
	Schema                  string         `json:"schema"`
	Issuer                  string         `json:"iss"`
	Audience                string         `json:"aud"`
	AccountID               string         `json:"account_id"`
	EndpointID              string         `json:"endpoint_id"`
	AuthorizationGeneration uint64         `json:"authorization_generation"`
	Generation              uint64         `json:"generation"`
	IssuedAt                int64          `json:"iat"`
	ExpiresAt               int64          `json:"exp"`
	Nodes                   []RegionalNode `json:"nodes"`
}

// Check applies every rule of a set that does not depend on who verifies it: clients after the signature, the mint
// before signing. Node health and endpoint eligibility are checked per node by the selector;
// an unhealthy node must not invalidate healthy alternatives. Expiry against now is the caller's.
func (c RegionalCandidates) Check(now time.Time) error {
	switch {
	case c.Schema != RegionalCandidatesSchema || c.Issuer == "" || c.Audience != RegionalCandidatesAudience || !ValidID(c.AccountID) || !ValidID(c.EndpointID):
		return invalid("regional candidates issuer")
	case c.Generation == 0 || c.Generation > MaxSafeInteger || c.AuthorizationGeneration == 0 || c.AuthorizationGeneration > MaxSafeInteger || c.IssuedAt <= 0 || c.IssuedAt > now.Add(MaxClockSkew).Unix() || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > int64(relaylimits.MaxGrantLifetime/time.Second):
		return invalid("regional candidates lifetime")
	case len(c.Nodes) > MaxRegionalCandidates:
		return invalid("regional candidates count")
	}
	for _, n := range c.Nodes {
		if n.Hosting != "" && n.Hosting != HostingManaged && n.Hosting != HostingSelfHosted {
			return invalid("regional candidate hosting")
		}
		if n.TLSSPKISHA256 != "" {
			pin, err := base64.RawURLEncoding.Strict().DecodeString(n.TLSSPKISHA256)
			if err != nil || len(pin) != 32 || base64.RawURLEncoding.EncodeToString(pin) != n.TLSSPKISHA256 {
				return invalid("regional candidate pin")
			}
		}
	}
	return nil
}
