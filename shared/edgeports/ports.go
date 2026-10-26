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

// ErrOverlap reports a public range that reaches into the outgoing range.
var ErrOverlap = errors.New("public TCP range overlaps the host's outgoing port range")

// PublicTCP reports whether port may carry a public TCP tunnel.
func PublicTCP(port int) bool { return port >= PublicTCPMin && port <= PublicTCPMax }

// Check proves the two ranges are valid and disjoint.
func Check() error {
	if PublicTCPMin < 1024 || PublicTCPMin > PublicTCPMax || OutgoingMin > OutgoingMax || OutgoingMax > 65535 {
		return errors.New("edge port ranges are invalid")
	}
	if PublicTCPMax >= OutgoingMin && OutgoingMax >= PublicTCPMin {
		return ErrOverlap
	}
	return nil
}
