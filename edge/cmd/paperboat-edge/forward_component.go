package main

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/config"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

// forwardSources adapts the control client to the mesh's peer and holder sources for this node process.
type forwardSources struct {
	client        *control.HTTPClient
	nodeID, epoch string
}

func (s forwardSources) Peers(ctx context.Context) (nodemesh.PeersResponse, error) {
	return s.client.Peers(ctx, s.nodeID, s.epoch)
}

func (s forwardSources) Holders(ctx context.Context, page string, limit int) (nodemesh.HoldersResponse, error) {
	return s.client.Holders(ctx, s.nodeID, s.epoch, page, limit)
}

// forwardHeld is what this node serves: preview and runtime hostnames (the preview registries behind the composite
// matcher), and durable HTTPS and TLS-passthrough tunnel hostnames with a ready replica attached.
type forwardHeld struct {
	matcher     *edgehttp.CompositeRouteMatcher
	durable     *edgehttp.DataCarrierRouteRegistry
	assignments *route.Registry
	authority   edgehttp.PublicTCPAuthority
}

// Held also lists the raw TCP ports and TLS-passthrough names of this node's public tunnels (its authority snapshot's
// valid decisions with their carrier attached).
func (h forwardHeld) Held() []string {
	held := append(h.matcher.Held(), h.durable.HeldHosts(h.assignments)...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if decisions, err := h.authority.Snapshot(ctx); err == nil {
		held = append(held, h.durable.HeldFromDecisions(decisions)...)
	}
	return held
}

// forwardStats puts the mesh's counters into every heartbeat observation.
type forwardStats struct {
	control.NodeSink
	stats atomic.Pointer[func() nodemesh.Stats]
}

func (s *forwardStats) Heartbeat(ctx context.Context, observation control.NodeObservation) (control.NodeAnswer, error) {
	if stats := s.stats.Load(); stats != nil {
		current := (*stats)()
		observation.Forward = &current
	}
	return s.NodeSink.Heartbeat(ctx, observation)
}

// forwardComponent runs the mesh as a data-plane component: it starts once the server has recorded this process as
// ready (the peer directory is refused before that), and stops with the data plane, after the drain has finished,
// so the links and the visitors forwarded through them live through a drain.
type forwardComponent struct {
	mesh  *forward.Mesh
	ready <-chan struct{}
	mu    sync.Mutex
	stop  context.CancelFunc
	done  chan struct{}
}

type forwardSpec struct {
	deployment  config.Deployment
	nodeID      string
	epoch       string
	certificate tls.Certificate
	client      *control.HTTPClient
	ready       <-chan struct{}
	held        forwardHeld
	public      *edgehttp.PublicTCPListeners
	metrics     *sharedmetrics.Metrics
	reporter    *reporting.Reporter
	stats       *forwardStats
}

// newForwardComponent builds the mesh and hooks it into the public listener; it returns nil when this node does not
// forward (forward_enabled off, or a self-hosted edge: managed nodes are never mixed with self-hosted ones).
func newForwardComponent(spec forwardSpec) (*forwardComponent, error) {
	d := spec.deployment
	if !d.ForwardOn() || d.SelfHosted {
		return nil, nil
	}
	sources := forwardSources{client: spec.client, nodeID: spec.nodeID, epoch: spec.epoch}
	mesh, err := forward.New(forward.Config{
		NodeID: spec.nodeID, Certificate: spec.certificate,
		Peers: sources, Holders: sources, Held: spec.held, Injector: spec.public, TCPInjector: spec.public,
		DialTimeout: d.ForwardDial(), OpenTimeout: d.ForwardOpen(), PeerInterval: d.ForwardPeers(), MaxPeerStale: d.ForwardPeerStale(), SeedInterval: d.ForwardSeed(), MaxStreamsPerPeer: d.ForwardMaxStreams(),
		OnFailure: func(stage string, err error) { spec.reporter.ObserveFailure(context.Background(), stage, err) },
		OnOutcome: func(outcome string) {
			_ = spec.metrics.IncCounter(edgetelemetry.MetricForward, sharedmetrics.Labels{"outcome": outcome})
		},
	})
	if err != nil {
		return nil, err
	}
	spec.public.SetForwarding(edgehttp.ForwardHooks{Forwarder: mesh, Mesh: mesh})
	stats := mesh.Stats
	spec.stats.stats.Store(&stats)
	return &forwardComponent{mesh: mesh, ready: spec.ready}, nil
}

func (c *forwardComponent) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done != nil {
		return errors.New("forward component already started")
	}
	run, stop := context.WithCancel(ctx)
	c.stop, c.done = stop, make(chan struct{})
	go func() {
		defer close(c.done)
		select {
		case <-c.ready:
		case <-run.Done():
			return
		}
		_ = c.mesh.Start(run)
		<-run.Done()
	}()
	return nil
}

func (c *forwardComponent) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	stop, done := c.stop, c.done
	c.mu.Unlock()
	if stop == nil {
		return nil
	}
	stop()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return c.mesh.Stop(ctx)
}
