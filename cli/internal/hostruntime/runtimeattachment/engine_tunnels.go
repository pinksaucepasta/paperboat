package runtimeattachment

import (
	"context"
	"encoding/json"
	"errors"
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

// EngineTunnels serves this machine's tunnels that ride on its runtime carriers (PLAN 3a step 4):
// each active route is registered on every carrier hub and forwarded to its origin, as the
// tunnel's own connector did. Served tells the tunnel manager, which reconciles every few seconds,
// which tunnels no longer run a connector.
type EngineTunnels struct {
	Service   *Service
	Forwarder tunnelmanager.OriginStreamForwarder
	// Events, when set, is the machine's tunnels topic; a tunnels change refetches at once.
	Events *machineevents.Subscription

	mu      sync.Mutex
	tunnels map[string]engineTunnel
	routes  map[engineRouteKey]*engineRoute
	cancel  context.CancelFunc
	done    chan struct{}
}

type engineTunnel struct {
	generation uint64
	hash       string
	routes     []hoststate.TunnelConfigRoute
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
	tunnel := engineTunnel{generation: item.ConfigGeneration, hash: item.ConfigContentHash}
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
}

func (e *EngineTunnels) serve(ctx context.Context, hub *preview.DataCarrierPreviewHub, tunnelID string, tunnel engineTunnel, route hoststate.TunnelConfigRoute) (*engineRoute, error) {
	if e.routes == nil {
		e.routes = map[engineRouteKey]*engineRoute{}
	}
	registration, err := hub.Register(route.ID)
	if err != nil {
		return nil, err
	}
	streams, err := e.Forwarder.StartFrom(ctx, registration, hub.Identity(), []hoststate.TunnelConfigRoute{route})
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
	e.tunnels = nil
}
