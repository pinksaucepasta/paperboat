package runtimeattachment

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// ErrRouteMoving: the host is a machine's runtime host, but no edge holds a carrier of it that can
// take a new request (for example while one edge restarts and the other carrier is not valid yet).
// Nothing was sent; the caller may retry.
var ErrRouteMoving = errors.New("machine route is moving to another edge")

// EdgeDialer dials the server's own requests to a machine's runtime host at an edge that can serve
// them now, instead of whichever edge DNS returns: an edge whose carrier of the machine is valid
// and whose node is not draining, the active carrier first. A machine holds a carrier on each edge
// of its pair, so a restarting edge costs nothing when the other carrier is ready. Other hosts are
// dialed unchanged. TLS still verifies the runtime host.
type EdgeDialer struct {
	DB   *db.DB
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Wait bounds how long a runtime host without a usable edge is re-checked before ErrRouteMoving.
	Wait time.Duration
}

const edgeDialerPoll = 250 * time.Millisecond

func (d EdgeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(d.Wait)
	for {
		runtimeHost, edges, err := d.edges(ctx, host)
		if err != nil {
			return nil, err
		}
		if !runtimeHost {
			return d.Dial(ctx, network, addr)
		}
		var failures error
		for _, edge := range edges {
			conn, err := d.Dial(ctx, network, net.JoinHostPort(edge, port))
			if err == nil {
				return conn, nil
			}
			failures = errors.Join(failures, err)
		}
		if failures != nil {
			return nil, failures
		}
		if !time.Now().Before(deadline) {
			return nil, ErrRouteMoving
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ErrRouteMoving, ctx.Err())
		case <-time.After(edgeDialerPoll):
		}
	}
}

// edges reports whether host is a machine runtime host and the ingress addresses of the edges that
// can serve it now, best first.
func (d EdgeDialer) edges(ctx context.Context, host string) (bool, []string, error) {
	rows, err := d.DB.Pool().Query(ctx, `SELECT host(n.public_ingress_ipv4),host(n.public_ingress_ipv6)
FROM control_routes r
LEFT JOIN runtime_carrier_attachments a ON a.route_id=r.id
LEFT JOIN control_tunnel_nodes n ON n.id=a.edge_node_id AND n.process_epoch=a.edge_process_epoch AND n.state<>'draining'
 AND n.public_ingress_verified_at IS NOT NULL AND paperboat.runtime_carrier_valid(r.id,n.id,n.process_epoch,true)
WHERE r.kind='runtime_https_wss' AND r.public_host=$1
ORDER BY a.active DESC NULLS LAST,n.id`, host)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	runtimeHost := false
	var out []string
	for rows.Next() {
		runtimeHost = true
		var v4, v6 *string
		if err := rows.Scan(&v4, &v6); err != nil {
			return false, nil, err
		}
		for _, address := range []*string{v4, v6} {
			if address != nil && *address != "" {
				out = append(out, *address)
			}
		}
	}
	return runtimeHost, out, rows.Err()
}
