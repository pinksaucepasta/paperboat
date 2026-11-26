// Package edgeports owns the TCP port ranges of an edge host: the range public TCP tunnels are
// allocated from (server) and may listen on (edge), and the range the host keeps for its own
// outgoing connections (pbh moves it above the public range). One value, so the allocator, the
// listener, the host firewall rule and the doctor check cannot drift apart.
package edgeports

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
)

const (
	// PublicTCPMin and PublicTCPMax bound the ports a public TCP tunnel gets, on every edge.
	PublicTCPMin = 10000
	PublicTCPMax = 59999
	// OutgoingMin and OutgoingMax are the host's ephemeral (net.ipv4.ip_local_port_range) ports,
	// kept clear of the public range so an outgoing connection never takes a tunnel's port.
	OutgoingMin = 60000
	OutgoingMax = 65535
)

// Node ports: where the edge, relay and pbh of a node listen. All sit below PublicTCPMin so a
// public tunnel can never take one. 80 and 443 are the public web ports and are not listed.
const (
	CarrierTCP       = 7272 // edge carrier, TCP
	CarrierQUIC      = 7273 // edge carrier, UDP
	RelayWSS         = 7274 // relay WSS (TCP) and STUN (UDP) when it shares a node with an edge; relay-only uses 443
	RelayQUIC        = 7275 // relay QUIC, UDP
	RelayMesh        = 7276 // relay-to-relay mesh, UDP default
	Claim            = 7277 // pbh claim/enroll HTTPS, TCP
	EdgeHealth       = 7278 // edge health and metrics, loopback only
	EdgePrivateHTTPS = 7279 // edge private HTTPS, loopback only
)

// NodeMin and NodeMax bound the contiguous carrier and relay listeners (CarrierTCP to RelayQUIC). The host
// firewall opens them and the edge guard limits them like 443 and the public range; both used to cover them
// because they sat inside the public range.
const (
	NodeMin = CarrierTCP
	NodeMax = RelayQUIC
)

// ErrOverlap reports a public range that reaches into the outgoing range.
var ErrOverlap = errors.New("public TCP range overlaps the host's outgoing port range")

// PublicTCP reports whether port may carry a public TCP tunnel.
func PublicTCP(port int) bool { return port >= PublicTCPMin && port <= PublicTCPMax }

// Check proves the two ranges are valid and disjoint.
func Check() error {
	if NodeMin < 1024 || NodeMin > NodeMax || NodeMax >= PublicTCPMin || Claim >= PublicTCPMin || EdgeHealth >= PublicTCPMin || EdgePrivateHTTPS >= PublicTCPMin {
		return errors.New("edge node ports are invalid")
	}
	if PublicTCPMin < 1024 || PublicTCPMin > PublicTCPMax || OutgoingMin > OutgoingMax || OutgoingMax > 65535 {
		return errors.New("edge port ranges are invalid")
	}
	if PublicTCPMax >= OutgoingMin && OutgoingMax >= PublicTCPMin {
		return ErrOverlap
	}
	return nil
}

// Component is one service a node runs and the ports it listens on.
type Component struct {
	Capability string // "relay" or "tunnel" (an edge)
	TCP, QUIC  int
}

// NodeComponents is what a node of a role ("relay", "tunnel" or "both") runs. A relay alone takes 443;
// next to an edge, which owns 443, it moves to RelayWSS.
func NodeComponents(role string) []Component {
	var c []Component
	if role == "relay" {
		c = append(c, Component{"relay", 443, RelayQUIC})
	}
	if role == "both" {
		c = append(c, Component{"relay", RelayWSS, RelayQUIC})
	}
	if role == "tunnel" || role == "both" {
		c = append(c, Component{"tunnel", CarrierTCP, CarrierQUIC})
	}
	return c
}

// Rule is one inbound allow rule: "tcp" or "udp", and a port or a "low-high" range.
type Rule struct {
	Protocol string `json:"protocol"`
	Ports    string `json:"ports"`
}

// InboundRules is what a node's firewall must let in: the claim port; a relay's WSS (TCP), STUN (UDP,
// same port) and QUIC; an edge's 443 (TCP, and UDP for HTTP/3), carrier TCP and QUIC, and the public
// tunnel range.
func InboundRules(role string, claim int) []Rule {
	var rules []Rule
	add := func(proto string, ports string) {
		if r := (Rule{proto, ports}); !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	add("tcp", strconv.Itoa(claim))
	for _, v := range NodeComponents(role) {
		if v.Capability == "tunnel" {
			add("tcp", "443")
			add("udp", "443")
			add("tcp", strconv.Itoa(v.TCP))
			add("udp", strconv.Itoa(v.QUIC))
			add("tcp", fmt.Sprintf("%d-%d", PublicTCPMin, PublicTCPMax))
			continue
		}
		add("tcp", strconv.Itoa(v.TCP))
		add("udp", strconv.Itoa(v.TCP))
		add("udp", strconv.Itoa(v.QUIC))
	}
	return rules
}
