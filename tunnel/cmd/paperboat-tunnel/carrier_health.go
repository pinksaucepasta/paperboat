package main

import (
	"context"
	"sync/atomic"

	edgeruntime "github.com/pinksaucepasta/paperboat-tunnel/internal/runtime"
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
