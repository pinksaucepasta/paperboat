package preview

import (
	"context"
	"errors"
	"sync"
	"time"
)

// AgentCarriers are the runtime carriers this machine's agent holds to the edges of its workspace
// (PLAN 3a), each as a stream hub. Hubs returns the current hubs and a channel that is closed when
// that set changes (a carrier connects, is replaced or closes).
type AgentCarriers interface {
	Hubs() ([]*DataCarrierPreviewHub, <-chan struct{})
	// EndpointsServed reports whether every edge of the agent's attachment admits endpoint
	// routes; until then no preview can run. It waits, bounded by ctx, for the
	// agent's first attachment answer after a start.
	EndpointsServed(ctx context.Context) bool
}

// endpointReadyTimeout bounds how long a preview waits for the first edge to serve its route.
const endpointReadyTimeout = 90 * time.Second

// EndpointCarrier serves a preview as an endpoint route on the agent's runtime carriers: the
// preview never dials a carrier of its own. The route id is the preview id. Every hub runs one
// route carrier (origin probe, stream forwarding, inspector); the preview is reported ready once,
// after the server has seen an edge serve the route.
type EndpointCarrier struct {
	carriers AgentCarriers
	newRoute func(*DataCarrierPreviewHub) (*DataCarrierPreviewCarrier, error)
	// attach, when set, runs while a route serves on a hub (owner-local private access); it
	// returns the function that undoes it.
	attach func(*DataCarrierPreviewHub, Lease) (func(), error)

	mu     sync.Mutex
	closed bool
	routes map[*DataCarrierPreviewHub]*DataCarrierPreviewCarrier
}

func NewEndpointCarrier(carriers AgentCarriers, newRoute func(*DataCarrierPreviewHub) (*DataCarrierPreviewCarrier, error)) (*EndpointCarrier, error) {
	if carriers == nil || newRoute == nil {
		return nil, ErrDataCarrierPreviewInvalid
	}
	return &EndpointCarrier{carriers: carriers, newRoute: newRoute, routes: map[*DataCarrierPreviewHub]*DataCarrierPreviewCarrier{}}, nil
}

func (c *EndpointCarrier) Run(ctx context.Context, lease Lease, ready func(Lease) error) error {
	if c == nil || ctx == nil || ready == nil {
		return ErrDataCarrierPreviewInvalid
	}
	runCtx, cancel := context.WithCancel(ctx)
	failures := make(chan error, 1)
	// Only a success is final: a failed attempt (its hub closed, say) must be retried by the
	// next hub's route, or the session never sees the preview ready.
	var readyMu sync.Mutex
	var reported bool
	reportReady := func(observed Lease) error {
		readyMu.Lock()
		defer readyMu.Unlock()
		if reported {
			return nil
		}
		err := readyWhenServed(runCtx, observed, ready)
		reported = err == nil
		return err
	}
	var wg sync.WaitGroup
	// Stop every route before waiting for them.
	defer func() { cancel(); wg.Wait() }()
	for {
		hubs, changed := c.carriers.Hubs()
		for _, hub := range hubs {
			route, start, err := c.routeFor(hub)
			if err != nil {
				return err
			}
			if !start {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c.attach != nil {
					detach, attachErr := c.attach(hub, lease)
					if attachErr != nil {
						select {
						case failures <- attachErr:
						default:
						}
						c.forget(hub, route)
						return
					}
					defer detach()
				}
				err := route.Run(runCtx, lease, reportReady)
				// A closed hub (its carrier reconnects or leaves) only drops that route: the set
				// change brings the replacement. Anything else, including an unreachable origin,
				// ends the preview's run so the session applies its usual retry.
				if err != nil && runCtx.Err() == nil && !hubClosed(hub) && !errors.Is(err, ErrDataCarrierPreviewHubClosed) {
					select {
					case failures <- err:
					default:
					}
				}
				c.forget(hub, route)
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case <-changed:
		}
		if c.isClosed() {
			return ErrDataCarrierPreviewClosed
		}
	}
}

// RunWithLease serves the lease the session holds now; the route and its target do not change
// over the lease's life, only its deadline, which the session renews.
func (c *EndpointCarrier) RunWithLease(ctx context.Context, currentLease func() Lease, _ LeaseMutation, ready func(Lease) error) error {
	if currentLease == nil {
		return ErrDataCarrierPreviewInvalid
	}
	return c.Run(ctx, currentLease(), ready)
}

// readyWhenServed reports readiness, retrying while no edge serves the route yet.
func readyWhenServed(ctx context.Context, observed Lease, ready func(Lease) error) error {
	deadline := time.Now().Add(endpointReadyTimeout)
	wait := 250 * time.Millisecond
	for {
		err := ready(observed)
		// Not served yet, or the server briefly unavailable: retry until the deadline.
		transient := errors.Is(err, ErrDispatchNotServed) || errors.Is(err, ErrDispatchUnavailable)
		if err == nil || !transient || time.Now().Add(wait).After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if wait *= 2; wait > 2*time.Second {
			wait = 2 * time.Second
		}
	}
}

// routeFor returns the route carrier for a hub, creating it when the hub is new.
func (c *EndpointCarrier) routeFor(hub *DataCarrierPreviewHub) (*DataCarrierPreviewCarrier, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false, ErrDataCarrierPreviewClosed
	}
	if route := c.routes[hub]; route != nil {
		return route, false, nil
	}
	if hubClosed(hub) {
		return nil, false, nil
	}
	route, err := c.newRoute(hub)
	if err != nil {
		return nil, false, err
	}
	c.routes[hub] = route
	return route, true, nil
}

func (c *EndpointCarrier) forget(hub *DataCarrierPreviewHub, route *DataCarrierPreviewCarrier) {
	c.mu.Lock()
	if c.routes[hub] == route {
		delete(c.routes, hub)
	}
	c.mu.Unlock()
	_ = route.Close(context.Background())
}

func (c *EndpointCarrier) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *EndpointCarrier) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	routes := make([]*DataCarrierPreviewCarrier, 0, len(c.routes))
	for _, route := range c.routes {
		routes = append(routes, route)
	}
	c.mu.Unlock()
	var err error
	for _, route := range routes {
		err = errors.Join(err, route.Close(ctx))
	}
	return err
}

func hubClosed(hub *DataCarrierPreviewHub) bool {
	select {
	case <-hub.Done():
		return true
	default:
		return false
	}
}
