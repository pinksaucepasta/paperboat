// Package edgeports owns the TCP port ranges of an edge host: the range public TCP tunnels are
// allocated from (server) and may listen on (edge), and the range the host keeps for its own
// outgoing connections (pbh moves it above the public range). One value, so the allocator, the
// listener, the host firewall rule and the doctor check cannot drift apart.
package edgeports

import "errors"

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
