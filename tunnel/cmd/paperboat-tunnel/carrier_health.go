package main

import (
	"context"
	"sync/atomic"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp"
	edgeruntime "github.com/pinksaucepasta/paperboat/tunnel/internal/runtime"
)

type trackedCarrierComponent struct {
	edgeruntime.Component
	running atomic.Bool
}

func (c *trackedCarrierComponent) Start(ctx context.Context) error {
	if err := c.Component.Start(ctx); err != nil {
		return err
	}
	c.running.Store(true)
	return nil
}
func (c *trackedCarrierComponent) Shutdown(ctx context.Context) error {
	c.running.Store(false)
	return c.Component.Shutdown(ctx)
}

func (c *trackedCarrierComponent) Done() <-chan error {
	if source, ok := c.Component.(interface{ Done() <-chan error }); ok {
		return source.Done()
	}
	return nil
}

// previewReadiness answers a host's route state from the endpoint registry first, then the fallback.
type previewReadiness struct {
	Canonical interface {
		RouteState(string) (string, string, string, bool)
		Lookup(string) (edgehttp.DataCarrierPreviewRoute, bool)
	}
	Fallback interface {
		RouteState(string) (string, string, string, bool)
	}
}

func (r previewReadiness) RouteState(host string) (string, string, string, bool) {
	if r.Canonical != nil {
		if _, ok := r.Canonical.Lookup(host); ok {
			return r.Canonical.RouteState(host)
		}
	}
	if r.Fallback != nil {
		return r.Fallback.RouteState(host)
	}
	return "", "", "", false
}
