//go:build darwin || windows

package machineguard

import (
	"context"
	"errors"
	"net"
)

// The gateway is routed by uid only where the guard can attribute a connection's owner (Linux; macOS is F23-04).
// Elsewhere one OS user's daemon owns each gateway port.
const gatewayRouting = false

type sharedGateway struct{}

func (g *guardServer) routeGateway(context.Context, string, int) (net.Listener, error) {
	return nil, errors.New("this machine guard does not route the gateway by user")
}
