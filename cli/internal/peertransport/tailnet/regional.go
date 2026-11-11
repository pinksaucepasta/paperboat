package tailnet

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

const MaxRegionalCandidates = peernetwork.MaxRegionalCandidates

var ErrRegionalAuthority = errors.New("regional candidate authority is invalid or expired")

type Redundancy string

const (
	RedundancyNone      Redundancy = "none"
	RedundancyReduced   Redundancy = "reduced"
	RedundancyAvailable Redundancy = "available"
)

// RegionalNode and RegionalCandidates are the shared signed candidate set types (shared/peernetwork).
type RegionalNode = peernetwork.RegionalNode
type RegionalCandidates = peernetwork.RegionalCandidates

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
	if e1 != nil || e2 != nil || e3 != nil || peernetwork.DecodeSigned(header, &h) != nil || h.Algorithm != "EdDSA" || h.Type != peernetwork.RegionalCandidatesType || !peernetwork.ValidID(h.KeyID) || peernetwork.DecodeSigned(body, &c) != nil {
		return ErrRegionalAuthority
	}
	pub, found, err := a.options.Keys.Lookup(ctx, h.KeyID)
	if err != nil || !found || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return ErrRegionalAuthority
	}
	now := serverclock.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil || c.AuthorizationGeneration != a.current.Generation {
		return ErrRegionalAuthority
	}
	if c.Check(now) != nil || c.Issuer != a.options.Issuer || c.AccountID != a.options.Self.AccountID || c.EndpointID != a.options.Self.EndpointID || c.ExpiresAt <= now.Unix() {
		return ErrRegionalAuthority
	}
	if a.regional != nil && c.Generation < a.regional.Generation {
		return ErrStaleAuthority
	}
	a.regional = &c
	return nil
}

// RegionalNodes returns the currently verified control-plane candidates.
// A node's reported state is not evidence of a live data connection.
func (a *Authority) RegionalNodes(now time.Time) ([]RegionalNode, error) {
	if a == nil {
		return nil, ErrRegionalAuthority
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.regional == nil || a.regional.ExpiresAt <= now.UTC().Unix() {
		return nil, ErrRegionalAuthority
	}
	nodes := make([]RegionalNode, len(a.regional.Nodes))
	for index, node := range a.regional.Nodes {
		nodes[index] = node
		nodes[index].Roles = append([]string(nil), node.Roles...)
		nodes[index].Transports = append([]string(nil), node.Transports...)
		if node.DrainDeadline != nil {
			deadline := *node.DrainDeadline
			nodes[index].DrainDeadline = &deadline
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Region != nodes[j].Region {
			return nodes[i].Region < nodes[j].Region
		}
		return nodes[i].NodeID < nodes[j].NodeID
	})
	return nodes, nil
}

// regionalObservationMaxAge is how old a signed health or capacity observation may be, when the candidate set was
// signed, for its node to be selected. A set is the control plane's statement as of IssuedAt and stays usable until
// it expires (relaylimits.MaxGrantLifetime): with Paperboat unreachable no fresher statement arrives, and an
// endpoint that dropped its relay carrier (a restart, a network change) still has to open a new one through the
// last known relays (offline until revoked). Judged against the present, every relayed path ended 15 s into an outage.
// A relay that failed since is not selected by the next renewal, which only lists relays that heartbeat.
const regionalObservationMaxAge = 15 * time.Second

func eligible(c *RegionalCandidates, role, transport string, regions map[string]bool, now time.Time) ([]RegionalNode, Redundancy, error) {
	if c == nil || c.ExpiresAt <= now.Unix() {
		return nil, RedundancyNone, ErrRegionalAuthority
	}
	// The nodes' observation age and lease are judged as of signing, not now; "observed in the future" stays a
	// check against now (a heartbeat can land a second after the set's issue time).
	at := now
	if signed := time.Unix(c.IssuedAt, 0); signed.Before(now) {
		at = signed
	}
	out := make([]RegionalNode, 0, len(c.Nodes))
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		if !peernetwork.ValidID(n.NodeID) || !peernetwork.ValidID(n.ProcessEpoch) || !peernetwork.ValidID(n.Region) || !peernetwork.ValidID(n.FailureDomain) || seen[n.NodeID] || n.NodeGeneration == 0 || n.State != "ready" || n.ObservedAt > now.Unix() || at.Unix()-n.ObservedAt > int64(regionalObservationMaxAge/time.Second) || n.ExpiresAt <= at.Unix() || n.CapacityObservedAt > now.Unix() || at.Unix()-n.CapacityObservedAt > int64(regionalObservationMaxAge/time.Second) || n.CapacityLimit == 0 || n.CapacityUsed >= n.CapacityLimit-n.CapacityLimit/10 || n.DrainDeadline != nil {
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
