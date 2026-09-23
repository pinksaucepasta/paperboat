package tailnet

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"time"
)

const MaxRegionalCandidates = 32

var ErrRegionalAuthority = errors.New("regional candidate authority is invalid or expired")

type Redundancy string

const (
	RedundancyNone      Redundancy = "none"
	RedundancyReduced   Redundancy = "reduced"
	RedundancyAvailable Redundancy = "available"
)

type RegionalNode struct {
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
}
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

func (a *Authority) ApplyRegionalCandidates(ctx context.Context, token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > MaxConfigurationBytes {
		return ErrRegionalAuthority
	}
	decode := func(s string) ([]byte, error) {
		b, e := base64.RawURLEncoding.Strict().DecodeString(s)
		if e != nil || base64.RawURLEncoding.EncodeToString(b) != s {
			return nil, ErrRegionalAuthority
		}
		return b, nil
	}
	header, e1 := decode(parts[0])
	body, e2 := decode(parts[1])
	sig, e3 := decode(parts[2])
	var h struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	var c RegionalCandidates
	if e1 != nil || e2 != nil || e3 != nil || strictDecode(header, &h) != nil || h.Algorithm != "EdDSA" || h.Type != "paperboat-regional-candidates+jwt" || !validID(h.KeyID) || strictDecode(body, &c) != nil {
		return ErrRegionalAuthority
	}
	pub, found, err := a.options.Keys.Lookup(ctx, h.KeyID)
	if err != nil || !found || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return ErrRegionalAuthority
	}
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil || c.AuthorizationGeneration != a.current.Generation {
		return ErrRegionalAuthority
	}
	if c.Schema != "paperboat.regional-candidates.v1" || c.Issuer != a.options.Issuer || c.Audience != "paperboat-regional-candidates" || c.AccountID != a.options.Self.AccountID || c.EndpointID != a.options.Self.EndpointID || c.Generation == 0 || c.IssuedAt > now.Unix() || c.ExpiresAt <= now.Unix() || c.ExpiresAt-c.IssuedAt > 60 || len(c.Nodes) > MaxRegionalCandidates {
		return ErrRegionalAuthority
	}
	if a.regional != nil && c.Generation < a.regional.Generation {
		return ErrStaleAuthority
	}
	a.regional = &c
	return nil
}

func (c *RegionalCandidates) Eligible(role, transport string, regions map[string]bool, now time.Time) ([]RegionalNode, Redundancy, error) {
	if c == nil || c.ExpiresAt <= now.Unix() {
		return nil, RedundancyNone, ErrRegionalAuthority
	}
	out := make([]RegionalNode, 0, len(c.Nodes))
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		if !validID(n.NodeID) || !validID(n.ProcessEpoch) || !validID(n.Region) || !validID(n.FailureDomain) || seen[n.NodeID] || n.NodeGeneration == 0 || n.State != "ready" || n.ObservedAt > now.Unix() || now.Unix()-n.ObservedAt > 15 || n.ExpiresAt <= now.Unix() || n.CapacityObservedAt > now.Unix() || now.Unix()-n.CapacityObservedAt > 15 || n.CapacityLimit == 0 || n.CapacityUsed >= n.CapacityLimit-n.CapacityLimit/10 || n.DrainDeadline != nil {
			continue
		}
		if len(regions) > 0 && !regions[n.Region] || !contains(n.Roles, role) || !contains(n.Transports, transport) || !validEndpointHost(n.EndpointHost) || (n.EndpointTCPPort == 0 && n.EndpointQUICPort == 0) {
			continue
		}
		seen[n.NodeID] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Region != out[j].Region {
			return out[i].Region < out[j].Region
		}
		return out[i].NodeID < out[j].NodeID
	})
	redundancy := RedundancyNone
	if len(out) == 1 {
		redundancy = RedundancyReduced
	}
	if len(out) > 1 {
		for i := 1; i < len(out); i++ {
			if out[i].FailureDomain != out[0].FailureDomain {
				redundancy = RedundancyAvailable
				break
			}
		}
		if redundancy == RedundancyNone {
			redundancy = RedundancyReduced
		}
	}
	return out, redundancy, nil
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func validEndpointHost(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == ':' || r == '-') {
			return false
		}
	}
	return true
}
