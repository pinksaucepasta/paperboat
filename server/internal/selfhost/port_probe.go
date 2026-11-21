package selfhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// ErrPortProbeInvalid reports a probe request outside what the probe may do: a public TCP port without a
// 16-64 URL-safe character nonce, a node port with one, or any other transport and port.
var ErrPortProbeInvalid = errors.New("port probe request is invalid")

const (
	portProbeTimeout = 4 * time.Second // one dial
	portProbeBudget  = 8 * time.Second // resolution, dial and every read and write of one probe
)

// PortProbe is what the control plane saw when it connected to a node's own public address, for pbh
// doctor to turn into the missing rule: "refused" and "nonce_mismatch" mean the host answered (a
// firewall rule is open), "unreachable" a host firewall REJECT, "timeout" a cloud firewall drop.
type PortProbe struct {
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail"`
}

// ProbePort connects from the control plane to the authenticated node's registered endpoint host. On
// transport "tcp" with a port in the public range it expects the node's test listener to write nonce
// back; on a node port (edgeports.NodeMin-NodeMax, held by the edge or relay, so pbh cannot listen
// there) an accepted connection counts, with no nonce. On transport "udp" only the relay's STUN port
// (edgeports.RelayWSS) is asked one STUN binding request. Only the credential's own node is ever
// dialed, only a public address (a private one when the deployment allows it), and only these ports,
// so the probe cannot scan anything else.
func (s *Service) ProbePort(ctx context.Context, credential, transport string, port int, nonce string) (PortProbe, error) {
	validNonce := len(nonce) >= 16 && len(nonce) <= 64 && strings.Trim(nonce, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") == ""
	var check func(context.Context, net.Conn) PortProbe
	switch {
	case transport == "tcp" && edgeports.PublicTCP(port) && validNonce:
		check = func(ctx context.Context, c net.Conn) PortProbe { return echoesNonce(ctx, c, nonce) }
	case transport == "tcp" && port >= edgeports.NodeMin && port <= edgeports.NodeMax && nonce == "":
		check = func(context.Context, net.Conn) PortProbe { return PortProbe{Reachable: true, Detail: "connected"} }
	case transport == "udp" && port == edgeports.RelayWSS && nonce == "":
		check = answersSTUN
	default:
		return PortProbe{}, ErrPortProbeInvalid
	}
	if !s.available() {
		return PortProbe{}, ErrRuntimeReadinessUnavailable
	}
	if len(credential) < 32 || len(credential) > 4096 {
		return PortProbe{}, ErrUnauthorized
	}
	var host string
	err := s.database.SQL().QueryRowContext(ctx, `SELECT n.endpoint_host
FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
WHERE i.runtime_hash=$1 AND i.state='active' AND i.claim_complete
AND(i.expires_at IS NULL OR i.expires_at>$2)`, digest(credential), s.now().UTC()).Scan(&host)
	if errors.Is(err, sql.ErrNoRows) {
		return PortProbe{}, ErrUnauthorized
	}
	if err != nil {
		return PortProbe{}, err
	}
	dial := s.PortProbeDial
	if dial == nil {
		dial = (&net.Dialer{Timeout: portProbeTimeout}).DialContext
	}
	return probeNodePort(ctx, host, transport, port, check, s.ingressIP, dial), nil
}

// probeNodePort resolves host once, dials the first allowed address and runs check on the connection, all
// within portProbeBudget. Canceling ctx closes the connection, so blocked reads and writes return at once.
func probeNodePort(ctx context.Context, host, transport string, port int, check func(context.Context, net.Conn) PortProbe, allowed func(netip.Addr) bool, dial func(context.Context, string, string) (net.Conn, error)) PortProbe {
	ctx, cancel := context.WithTimeout(ctx, portProbeBudget)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
		addresses, err = []netip.Addr{ip}, nil
	}
	if err != nil {
		return PortProbe{Detail: "unresolved"}
	}
	for _, ip := range addresses {
		if !allowed(ip) {
			continue
		}
		connection, err := dial(ctx, transport, net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(port)))
		if err != nil {
			return PortProbe{Detail: classifyDialError(err)}
		}
		defer connection.Close()
		defer context.AfterFunc(ctx, func() { _ = connection.Close() })()
		result := check(ctx, connection)
		if !result.Reachable && ctx.Err() != nil {
			return PortProbe{Detail: "timeout"}
		}
		return result
	}
	return PortProbe{Detail: "not_public"}
}

// stepDeadline is now+d, or ctx's deadline when that is sooner.
func stepDeadline(ctx context.Context, d time.Duration) time.Time {
	t := time.Now().Add(d)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(t) {
		return deadline
	}
	return t
}

func echoesNonce(ctx context.Context, connection net.Conn, nonce string) PortProbe {
	_ = connection.SetDeadline(stepDeadline(ctx, 3*time.Second))
	got := make([]byte, len(nonce))
	if n, _ := io.ReadFull(connection, got); n == len(nonce) && string(got) == nonce {
		return PortProbe{Reachable: true, Detail: "connected"}
	}
	return PortProbe{Detail: "nonce_mismatch"}
}

// stunResponseMax bounds the one datagram read per try; a datagram that fills the buffer is refused as
// possibly truncated.
const stunResponseMax = 1024

// answersSTUN sends an RFC 5389 binding request, at most twice with 2 s each (never after ctx ends), and
// counts only a well-formed binding success that echoes the magic cookie and transaction id. The layout
// follows ~/ref/tailscale/net/stun/stun.go (Request, ParseResponse), which netcheck uses the same way to
// probe a DERP's STUN port.
func answersSTUN(ctx context.Context, connection net.Conn) PortProbe {
	request := make([]byte, 20)
	binary.BigEndian.PutUint16(request[0:], 0x0001) // binding request, no attributes
	binary.BigEndian.PutUint32(request[4:], 0x2112A442)
	_, _ = rand.Read(request[8:20])
	response := make([]byte, stunResponseMax+1)
	detail := "timeout"
	for try := 0; try < 2 && ctx.Err() == nil; try++ {
		_ = connection.SetDeadline(stepDeadline(ctx, 2*time.Second))
		if _, err := connection.Write(request); err != nil {
			return PortProbe{Detail: classifyDialError(err)}
		}
		n, err := connection.Read(response)
		if err != nil {
			// A closed UDP port answers with ICMP, which the next read reports as refused.
			if detail = classifyDialError(err); detail != "timeout" {
				return PortProbe{Detail: detail}
			}
			continue
		}
		if stunBindingSuccess(response[:n], request) {
			return PortProbe{Reachable: true, Detail: "connected"}
		}
		return PortProbe{Detail: "nonce_mismatch"}
	}
	return PortProbe{Detail: detail}
}

// stunBindingSuccess reports whether packet is exactly one binding success header plus its declared,
// 4-byte aligned attributes, with the request's magic cookie and transaction id.
func stunBindingSuccess(packet, request []byte) bool {
	if len(packet) < 20 || len(packet) > stunResponseMax {
		return false
	}
	length := int(binary.BigEndian.Uint16(packet[2:4]))
	return binary.BigEndian.Uint16(packet) == 0x0101 && length%4 == 0 && len(packet) == 20+length && bytes.Equal(packet[4:20], request[4:20])
}

func classifyDialError(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	}
	return "error"
}
