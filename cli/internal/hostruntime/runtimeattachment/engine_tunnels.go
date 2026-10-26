package runtimeattachment

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/originhealth"
	"io"
	"net/http"
	"sync"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

// maximumEngineTunnels bounds one answer of the engine tunnel list.
const maximumEngineTunnels = 1024

// engineTunnelPoll is how often the list is refetched without a tunnels event.
const engineTunnelPoll = machineevents.DefaultFallbackInterval

// TunnelEndpointsServed reports whether every edge of the latest attachment serves this agent's
// tunnels as endpoint routes, and a channel closed when that changes.
func (s *Service) TunnelEndpointsServed() (bool, <-chan struct{}) {
	s.hubsMu.Lock()
	defer s.hubsMu.Unlock()
	return s.tunnelsServed, s.tunnelsChanged
}

// FetchEngineTunnels returns the active configuration of each of this machine's tunnels that the
// server serves on the endpoint engine (PLAN 3a step 4).
func (s *Service) FetchEngineTunnels(ctx context.Context) (out []runtimewire.EngineTunnel, returnErr error) {
	op, err := randomID("operation")
	if err != nil {
		return nil, controlFailure(err)
	}
	res, err := s.post(ctx, runtimewire.HostTunnelsPath, op, runtimewire.TunnelsRequest{OperationID: op})
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil && returnErr == nil {
			out, returnErr = nil, controlFailure(closeErr)
		}
	}()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return nil, controlFailure(errorreport.HTTPStatusFailure(res))
	}
	var envelope struct {
		Data struct {
			Tunnels []runtimewire.EngineTunnel `json:"tunnels"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&envelope); err != nil {
		return nil, controlFailure(errors.Join(preview.ErrMachineAttachmentSessionInvalid, err))
	}
	if len(envelope.Data.Tunnels) > maximumEngineTunnels {
		return nil, controlFailure(preview.ErrMachineAttachmentSessionInvalid)
	}
	return envelope.Data.Tunnels, nil
}

// ReportOriginHealth tells the server whether one engine tunnel's route origins are reachable.
func (s *Service) ReportOriginHealth(ctx context.Context, report tunnelmanager.OriginHealthReport) (returnErr error) {
	op, err := randomID("operation")
	if err != nil {
		return controlFailure(err)
	}
	request := runtimewire.OriginHealthRequest{}
	for _, route := range report.Routes {
		request.Routes = append(request.Routes, runtimewire.OriginHealthRoute{TunnelID: report.TunnelID, RouteID: route.RouteID, State: route.State, Code: route.Code})
	}
	res, err := s.post(ctx, runtimewire.HostTunnelOriginHealthPath, op, request)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK {
		return controlFailure(errorreport.HTTPStatusFailure(res))
	}
	return nil
}

// EngineTunnels serves this machine's tunnels that ride on its runtime carriers (PLAN 3a step 4):
// each active route is registered on every carrier hub and forwarded to its origin, as the
// tunnel's own connector did. Served tells the tunnel manager, which reconciles every few seconds,
// which tunnels no longer run a connector.
type EngineTunnels struct {
	Service   *Service
	Forwarder tunnelmanager.OriginStreamForwarder
	// Prober checks each route's origin with the same TLS settings as Forwarder.
	Prober tunnelmanager.OriginProber
	// Events, when set, is the machine's tunnels topic; a tunnels change refetches at once.
	Events *machineevents.Subscription

	mu             sync.Mutex
	browserIngress tunnelmanager.IngressAuthorityFunc
	tunnels        map[string]engineTunnel
	routes         map[engineRouteKey]*engineRoute
	// origins probes each engine tunnel's route origins once (not per hub) and reports changes.
	origins map[string]engineOrigins
	// originInterval overrides the origin probe interval (tests); zero is the default.
	originInterval time.Duration
	cancel         context.CancelFunc
	done           chan struct{}
}

type engineTunnel struct {
	generation uint64
	hash       string
	restricted bool
	routes     []hoststate.TunnelConfigRoute
	// capture is the inspector identity of each route (tunnel and route generations), so a
	// captured request can be replayed.
	capture map[string]tunnelmanager.CaptureIdentity
}

type engineOrigins struct {
	generation uint64
	hash       string
	stop       func()
}

type engineRouteKey struct {
	hub   *preview.DataCarrierPreviewHub
	route string
}

type engineRoute struct {
	tunnel       string
	generation   uint64
	hash         string
	registration *preview.DataCarrierPreviewRegistration
	streams      *tunnelmanager.RunningOriginStreams
}

// Served reports whether tunnelID is served on the engine now.
func (e *EngineTunnels) Served(tunnelID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.tunnels[tunnelID]
	return ok
}

func (e *EngineTunnels) Start(ctx context.Context) error {
	if e == nil || e.Service == nil || e.Forwarder.Transport == nil || ctx == nil {
		return errors.New("engine tunnels configuration invalid")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done != nil {
		return errors.New("engine tunnels already started")
	}
	// Without an origin transport every route would be refused at serve time and no tunnel served
	// (dev-099c1c94, 2026-10-26: step 5 dropped the wiring); fail at startup instead.
	if e.Forwarder.Transport == nil || e.Prober == nil {
		return errors.New("engine tunnels need an origin transport and prober")
	}
	browserIngress, err := tunnelmanager.NewBrowserIngressAuthority(e.Service.config.ControlURL, e.Service.auth, e.Service.config.Transport)
	if err != nil {
		return err
	}
	e.browserIngress = browserIngress
	runCtx, cancel := context.WithCancel(ctx)
	e.cancel, e.done = cancel, make(chan struct{})
	go func() { defer close(e.done); e.run(runCtx) }()
	return nil
}

func (e *EngineTunnels) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *EngineTunnels) run(ctx context.Context) {
	defer e.stopAll()
	ticker := time.NewTicker(engineTunnelPoll)
	defer ticker.Stop()
	var events <-chan struct{}
	if e.Events != nil {
		events = e.Events.Changed()
	}
	refetch := true
	for {
		served, servedChanged := e.Service.TunnelEndpointsServed()
		hubs, hubsChanged := e.Service.Hubs()
		if refetch {
			e.refresh(ctx, served)
		}
		e.reconcile(ctx, hubs)
		select {
		case <-ctx.Done():
			return
		case <-servedChanged:
			refetch = true
		case <-events:
			refetch = true
		case <-ticker.C:
			refetch = true
		case <-hubsChanged:
			refetch = false
		}
	}
}

// refresh replaces the engine tunnel set; a failed fetch keeps the last good set.
func (e *EngineTunnels) refresh(ctx context.Context, served bool) {
	next := map[string]engineTunnel{}
	if served {
		fetchCtx, cancel := context.WithTimeout(ctx, attachmentCheckTimeout)
		list, err := e.Service.FetchEngineTunnels(fetchCtx)
		cancel()
		if err != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "engine_tunnels", "fetch", "service_failed", err)
			return
		}
		for _, item := range list {
			tunnel, err := decodeEngineTunnel(item)
			if err != nil {
				errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "engine_tunnels", "decode", "service_failed", err)
				continue
			}
			next[item.TunnelID] = tunnel
		}
	}
	e.mu.Lock()
	e.tunnels = next
	e.mu.Unlock()
}

// decodeEngineTunnel verifies the snapshot against the server's content hash and keeps its active
// routes.
func decodeEngineTunnel(item runtimewire.EngineTunnel) (engineTunnel, error) {
	snapshot, err := hoststate.NewConfigSnapshot(item.TunnelID, item.ConfigGeneration, item.Snapshot)
	if err != nil {
		return engineTunnel{}, err
	}
	if snapshot.ContentHash != item.ConfigContentHash {
		return engineTunnel{}, errors.New("engine tunnel configuration hash differs")
	}
	parsed, err := hoststate.ParseTunnelConfigSnapshot(snapshot.Payload, item.TunnelID, item.ConfigGeneration)
	if err != nil {
		return engineTunnel{}, err
	}
	tunnel := engineTunnel{generation: item.ConfigGeneration, hash: item.ConfigContentHash, restricted: parsed.AccessMode != "public", capture: map[string]tunnelmanager.CaptureIdentity{}}
	// Replay re-checks a fresh server decision each time; this expiry only bounds the local binding.
	// shortcut: a route restarts on config changes only, so a tunnel generation bump alone (rename)
	// leaves its captures unreplayable until the next config change; key restarts on it if that matters.
	expires := time.Now().AddDate(1, 0, 0)
	if parsed.ExpiresAt != nil {
		expires = *parsed.ExpiresAt
	}
	for routeID, generation := range item.RouteGenerations {
		tunnel.capture[routeID] = tunnelmanager.CaptureIdentity{ResourceID: routeID, ResourceGeneration: item.TunnelGeneration, RouteGeneration: generation, TargetGeneration: generation, ExpiresAt: expires}
	}
	if parsed.DesiredState != "active" {
		return tunnel, nil
	}
	for _, route := range parsed.Routes {
		if route.DesiredState == "active" {
			tunnel.routes = append(tunnel.routes, route)
		}
	}
	return tunnel, nil
}

// reconcile serves every active route of every engine tunnel on every live hub, and stops what is
// no longer wanted. A route whose tunnel configuration changed is restarted.
func (e *EngineTunnels) reconcile(ctx context.Context, hubs []*preview.DataCarrierPreviewHub) {
	e.mu.Lock()
	defer e.mu.Unlock()
	wanted := map[engineRouteKey]struct{}{}
	for _, hub := range hubs {
		for tunnelID, tunnel := range e.tunnels {
			for _, route := range tunnel.routes {
				key := engineRouteKey{hub: hub, route: route.ID}
				wanted[key] = struct{}{}
				if current := e.routes[key]; current != nil {
					if current.generation == tunnel.generation && current.hash == tunnel.hash {
						continue
					}
					stopEngineRoute(current)
					delete(e.routes, key)
				}
				served, err := e.serve(ctx, hub, tunnelID, tunnel, route)
				if err != nil {
					errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "engine_tunnels", "route", "service_failed", err)
					continue
				}
				e.routes[key] = served
			}
		}
	}
	for key, route := range e.routes {
		if _, ok := wanted[key]; !ok {
			stopEngineRoute(route)
			delete(e.routes, key)
		}
	}
	e.reconcileOrigins(ctx)
}

func (e *EngineTunnels) serve(ctx context.Context, hub *preview.DataCarrierPreviewHub, tunnelID string, tunnel engineTunnel, route hoststate.TunnelConfigRoute) (*engineRoute, error) {
	if e.routes == nil {
		e.routes = map[engineRouteKey]*engineRoute{}
	}
	registration, err := hub.Register(route.ID)
	if err != nil {
		return nil, err
	}
	// The edge confirms an engine route with a connector_ready stream before it serves it; a
	// viewer's stream on a team or private route carries the edge's browser decision, re-checked
	// with the server before the origin is reached (PLAN 3a 4.4).
	forwarder := e.Forwarder
	forwarder.EngineRoutes = true
	forwarder.IngressAuthority = e.browserIngress
	forwarder.Restricted = tunnel.restricted
	if identity, ok := tunnel.capture[route.ID]; ok {
		forwarder.CaptureIdentity = &identity
	}
	streams, err := forwarder.StartFrom(ctx, registration, hub.Identity(), []hoststate.TunnelConfigRoute{route})
	if err != nil {
		_ = registration.Close()
		return nil, err
	}
	return &engineRoute{tunnel: tunnelID, generation: tunnel.generation, hash: tunnel.hash, registration: registration, streams: streams}, nil
}

func stopEngineRoute(route *engineRoute) {
	_ = route.registration.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = route.streams.Close(shutdown)
}

func (e *EngineTunnels) stopAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, route := range e.routes {
		stopEngineRoute(route)
		delete(e.routes, key)
	}
	for tunnelID, current := range e.origins {
		current.stop()
		delete(e.origins, tunnelID)
	}
	e.tunnels = nil
}

// reconcileOrigins keeps one origin prober per served engine tunnel, restarted when its
// configuration changes, so `pb tunnel status` can say the app behind a route stopped.
func (e *EngineTunnels) reconcileOrigins(ctx context.Context) {
	if e.origins == nil {
		e.origins = map[string]engineOrigins{}
	}
	for tunnelID, tunnel := range e.tunnels {
		if current, ok := e.origins[tunnelID]; ok {
			if current.generation == tunnel.generation && current.hash == tunnel.hash {
				continue
			}
			current.stop()
		}
		stop := tunnelmanager.StartOriginHealth(ctx, tunnelID, tunnel.routes, e.Prober, e.Service, cmp.Or(e.originInterval, originhealth.DefaultProbeInterval))
		e.origins[tunnelID] = engineOrigins{generation: tunnel.generation, hash: tunnel.hash, stop: stop}
	}
	for tunnelID, current := range e.origins {
		if _, ok := e.tunnels[tunnelID]; !ok {
			current.stop()
			delete(e.origins, tunnelID)
		}
	}
}
