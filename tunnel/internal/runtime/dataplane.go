package runtime

import (
	"context"
	"errors"
	"sync"
)

type DataPlaneSpec struct {
	Persistence Component
	Control     Component
	Carrier     Component
	// Certificates owns the authenticated server-to-edge TLS certificate
	// distribution loop. It is optional for a legacy/static deployment, but
	// when supplied it must be stopped before carrier listeners are closed.
	Certificates    Component
	Runtime         Component
	Preview         Component
	Node            Component
	Routes          Component
	PublicTCP       Component
	Gateway         Component
	PrivateIngress  Component
	RedirectIngress Component
	HTTP3Ingress    Component
	Usage           Component
}

type DataPlane struct {
	spec    DataPlaneSpec
	mu      sync.Mutex
	started []Component
	closed  bool
}

func NewDataPlane(spec DataPlaneSpec) (*DataPlane, error) {
	if spec.Persistence == nil || spec.Control == nil || spec.Node == nil || spec.Routes == nil || spec.Gateway == nil || spec.PrivateIngress == nil || spec.RedirectIngress == nil || spec.HTTP3Ingress == nil || spec.Usage == nil {
		return nil, ErrProcessInvalid
	}
	return &DataPlane{spec: spec}, nil
}

func (d *DataPlane) Start(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.started) != 0 || d.closed {
		return ErrProcessInvalid
	}
	// Bind native ingress before any control-plane publication so routes are not
	// advertised until every public and private listener is ready.
	components := []Component{d.spec.Persistence, d.spec.Gateway, d.spec.HTTP3Ingress, d.spec.PrivateIngress, d.spec.RedirectIngress, d.spec.Control, d.spec.Node}
	if d.spec.Certificates != nil {
		components = append(components, d.spec.Certificates)
	}
	if d.spec.Preview != nil {
		// Publish and ACK the complete admission snapshot before opening the
		// carrier listener. Hosts are permitted to dial only after that ACK, and
		// the listener's PeerBinding therefore sees the admitted identity on the
		// first connection.
		components = append(components, d.spec.Preview)
	}
	if d.spec.Runtime != nil {
		components = append(components, d.spec.Runtime)
	}
	if d.spec.Carrier != nil {
		components = append(components, d.spec.Carrier)
	}
	if d.spec.PublicTCP != nil {
		components = append(components, d.spec.PublicTCP)
	}
	components = append(components, d.spec.Routes)
	components = append(components, d.spec.Usage)
	for _, component := range components {
		if err := component.Start(ctx); err != nil {
			var cleanup []error
			for i := len(d.started) - 1; i >= 0; i-- {
				if stopErr := d.started[i].Shutdown(context.Background()); stopErr != nil {
					cleanup = append(cleanup, stopErr)
				}
			}
			d.started = nil
			d.closed = true
			return errors.Join(componentStartError(component, err), errors.Join(cleanup...))
		}
		d.started = append(d.started, component)
	}
	return nil
}

func (d *DataPlane) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	// Public ingress and connector forwarding stop before the final accounting
	// flush. Control and persistence remain available for cleanup.
	order := []Component{d.spec.Usage}
	if d.spec.PublicTCP != nil {
		order = append(order, d.spec.PublicTCP)
	}
	order = append(order, d.spec.Routes)
	if d.spec.Runtime != nil {
		order = append(order, d.spec.Runtime)
	}
	if d.spec.Preview != nil {
		// Reconcile and detach preview routes while authenticated carrier peers
		// are still alive. Carrier shutdown follows this component.
		order = append(order, d.spec.Preview)
	}
	if d.spec.Certificates != nil {
		// Stop pulling/acknowledging certificate actions before carrier
		// shutdown. The receiver drops private key material on close.
		order = append(order, d.spec.Certificates)
	}
	if d.spec.Carrier != nil {
		// Stop intake and close active carrier streams before route, gateway,
		// and process teardown. This preserves the connector drain boundary.
		order = append(order, d.spec.Carrier)
	}
	order = append(order, d.spec.Node, d.spec.Control, d.spec.RedirectIngress, d.spec.PrivateIngress, d.spec.HTTP3Ingress, d.spec.Gateway, d.spec.Persistence)
	var failures []error
	for _, component := range order {
		if err := component.Shutdown(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	d.started = nil
	return errors.Join(failures...)
}
