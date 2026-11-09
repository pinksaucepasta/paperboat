package tunnelmanager

import (
	"context"
	"errors"
	"net"
	"strings"
)

// LocalhostDialer is the default origin dialer. It dials "localhost" as the two
// loopback addresses, 127.0.0.1 first and then ::1, instead of trusting the
// resolver: Windows resolves localhost to ::1 first and a dev server often
// listens on only one family, while some Linux hosts list only 127.0.0.1.
// Every other address is dialed unchanged.
type LocalhostDialer struct{ net.Dialer }

func (d *LocalhostDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, "localhost") || !strings.HasPrefix(network, "tcp") {
		return d.Dialer.DialContext(ctx, network, address)
	}
	var failures error
	for _, ip := range []string{"127.0.0.1", "::1"} {
		connection, err := d.Dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return connection, nil
		}
		failures = errors.Join(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, failures
}
