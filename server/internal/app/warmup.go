package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/peersessions"
)

const (
	// warmConnections is how many pool connections the warm-up opens and primes.
	warmConnections = 8
	// warmTimeout bounds the warm-up: a slow database must not keep a replacement from ever reporting ready
	// (the rollout waits 60 s for it).
	warmTimeout = 20 * time.Second
	// warmPolicyLimit caps the machines whose peer policy document the warm-up builds (the rest are built on demand,
	// behind the in-flight bound of the configuration routes).
	warmPolicyLimit = 2000
	// warmPolicyTimeout bounds that step: it waits for the policy LISTEN session, which a broken direct pool never
	// attaches, and readiness must not wait the whole warm-up for it.
	warmPolicyTimeout = 10 * time.Second
)

// warmUp builds the warm-up: open the pool's connections, then answer the heavy cutover read (the edges' ingress
// desired-state projection) internally on each of them. Every edge and daemon hits a replacement server in the
// same instant, and its first answers took 1.7-2.0 s while the edges' deadline was 2 s. Failures are logged and
// never block readiness.
func warmUp(store *db.DB, edges *controlplane.EdgeService, peers *peersessions.NetworkService, logger *slog.Logger) func(context.Context) {
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
		if peers != nil {
			// The cache is empty after a restart and every machine reconnects into it: build their documents first.
			peerCtx, cancelPeers := context.WithTimeout(ctx, warmPolicyTimeout)
			warmed, err := peers.WarmPolicies(peerCtx, warmPolicyLimit, warmConnections)
			cancelPeers()
			if err != nil {
				logger.Warn("warm-up: peer policy documents", "warmed", warmed, "error", err)
			} else {
				logger.Info("warm-up: peer policy documents", "warmed", warmed)
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
