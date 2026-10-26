package selfhost

import (
	"context"
	"database/sql"
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

// ErrPortProbeInvalid reports a probe request outside what the probe may do: a port outside the public
// TCP range or a nonce that is not 16-64 URL-safe characters.
var ErrPortProbeInvalid = errors.New("port probe request is invalid")

const portProbeTimeout = 4 * time.Second

// PortProbe is what the control plane saw when it connected to a node's own public address, for pbh
// doctor to turn into the missing rule: "refused" and "nonce_mismatch" mean the host answered (a
// firewall rule is open), "unreachable" a host firewall REJECT, "timeout" a cloud firewall drop.
type PortProbe struct {
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail"`
}

// ProbePort connects from the control plane to the authenticated node's registered endpoint host on
// port and expects the node's test listener to write nonce back. Only the credential's own node is
// ever dialed, only a public address (a private one when the deployment allows it), only a port in
// the public TCP range, and only a peer that echoes the nonce counts as reachable, so the probe
// cannot scan anything else.
func (s *Service) ProbePort(ctx context.Context, credential string, port int, nonce string) (PortProbe, error) {
	if !edgeports.PublicTCP(port) || len(nonce) < 16 || len(nonce) > 64 || strings.Trim(nonce, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
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
	return probeNodePort(ctx, host, port, nonce, s.ingressIP, (&net.Dialer{Timeout: portProbeTimeout}).DialContext), nil
}

// probeNodePort resolves host once, dials the first allowed address and checks the nonce echo.
func probeNodePort(ctx context.Context, host string, port int, nonce string, allowed func(netip.Addr) bool, dial func(context.Context, string, string) (net.Conn, error)) PortProbe {
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
		ctx, cancel := context.WithTimeout(ctx, portProbeTimeout)
		defer cancel()
		connection, err := dial(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(port)))
		if err != nil {
			return PortProbe{Detail: classifyDialError(err)}
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(nonce))
		if n, _ := io.ReadFull(connection, got); n == len(nonce) && string(got) == nonce {
			return PortProbe{Reachable: true, Detail: "connected"}
		}
		return PortProbe{Detail: "nonce_mismatch"}
	}
	return PortProbe{Detail: "not_public"}
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
