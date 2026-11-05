package daemoncmd

import (
	"fmt"
	"net"
	"strconv"
)

// Drain hands a draining daemon generation's singletons to the next one (SIGQUIT from pb install):
// the RPC socket, the guard connection and every route listener. Accepted forwards keep running on
// their routes, now draining, until they end or Stop cuts them. The guard holds the leases until the
// next generation takes the same listeners. Web routes are left as they are.
func (c *Coordinator) Drain() error {
	c.resetReconcileRetry()
	c.mu.Lock()
	server, listener, nameClient := c.rpcServer, c.rpcListener, c.nameClient
	c.rpcServer, c.rpcListener, c.nameClient = nil, nil, nil
	routes := make([]*guardedRoute, 0, len(c.routes))
	for key, route := range c.routes {
		routes = append(routes, route)
		c.draining = append(c.draining, route)
		delete(c.routes, key)
	}
	c.mu.Unlock()
	var causes []error
	for _, route := range routes {
		causes = append(causes, route.stopAccepting())
	}
	if nameClient != nil {
		causes = append(causes, lifecycleCleanup(nameClient.Close()))
	}
	if server != nil {
		server.Stop()
	}
	if listener != nil {
		causes = append(causes, lifecycleCleanup(listener.Close()))
	}
	return coordinatorFailure("local access hand-over to the next daemon generation failed", causes...)
}

// DrainingStreams names each draining route that still carries forwards: its lease address and
// port, the machine and port it reaches, and how many streams it holds.
func (c *Coordinator) DrainingStreams() []string {
	c.mu.Lock()
	draining := append([]*guardedRoute(nil), c.draining...)
	c.mu.Unlock()
	var streams []string
	for _, route := range draining {
		route.mu.Lock()
		active := route.active
		route.mu.Unlock()
		if active > 0 {
			local := net.JoinHostPort(route.address.String(), strconv.Itoa(route.port))
			streams = append(streams, fmt.Sprintf("%s -> %s:%d (%d streams)", local, route.machineID, route.port, active))
		}
	}
	return streams
}
