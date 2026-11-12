package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

const (
	// warmConnections is how many pool connections the warm-up opens and primes.
	warmConnections = 8
	// warmTimeout bounds the warm-up: a slow database must not keep a replacement from ever reporting ready
	// (the rollout waits 60 s for it).
	warmTimeout = 20 * time.Second
)

// warmUp builds the warm-up: open the pool's connections, then answer the heavy cutover read (the edges' ingress
// desired-state projection) internally on each of them. Every edge and daemon hits a replacement server in the
// same instant, and its first answers took 1.7-2.0 s while the edges' deadline was 2 s. Failures are logged and
// never block readiness.
func warmUp(store *db.DB, edges *controlplane.EdgeService, logger *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, warmTimeout)
		defer cancel()
		if err := store.WarmPool(ctx, warmConnections); err != nil {
			logger.Warn("warm-up: opening connections failed", "error", err)
		}
		if edges != nil {
			if err := edges.Warm(ctx, warmConnections); err != nil {
				logger.Warn("warm-up: edge desired-state projection failed", "error", err)
			}
		}
	}
}

// runWarmUp runs the warm-up once and then lets the server report ready.
func (a *App) runWarmUp(ctx context.Context) {
	if a.warmed == nil {
		return
	}
	started := time.Now()
	if a.warm != nil {
		a.warm(ctx)
	}
	a.warmed.Store(true)
	a.logger.Info("server warm", "duration_ms", time.Since(started).Milliseconds())
}
