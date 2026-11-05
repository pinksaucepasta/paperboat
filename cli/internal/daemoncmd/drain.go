package daemoncmd

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A user's daemon generation that gets SIGQUIT (pb install started the next one) drains instead of
// exiting: it hands over every singleton at once and keeps only its native runtime, the forwarded
// connections it already accepted and the local API peer streams (exec, terminal, SSH) it carries, until the last one closes, runtimewire.DrainLimit passes, or its
// network identity is revoked. Prior art: Teleport graceful restart (lib/service/signals.go: the old
// process stops listening and finishes its connections) and nginx's QUIT for an old binary.

// drainCoordinator is the local access coordinator seen by a draining generation.
type drainCoordinator interface {
	// Drain releases the RPC socket, the guard connection and every route listener, keeping the
	// connections already accepted.
	Drain() error
	// DrainingStreams names each held route: its lease address and port, the machine and port, and
	// how many streams it carries.
	DrainingStreams() []string
}

// drainIdentity is this generation's network identity (tunnel.PeerTerminalTunnel).
type drainIdentity interface {
	BeginDrain(context.Context) error
	Withdraw(context.Context) error
}

type drainer struct {
	coordinator drainCoordinator
	identity    drainIdentity
	// stopLive ends the live part of the daemon: local API, daemon lock, inventory and events.
	stopLive func()
	// revoked receives the reason once the server refuses this generation's identity.
	revoked chan string
	// publish reports the streams still held and the drain deadline (pb status, pb doctor).
	publish func(streams int, until time.Time)
	limit   time.Duration
	poll    time.Duration
	begun   atomic.Bool
}

// newDrainer exists before the tunnel and coordinator it drains, so the tunnel can report a
// revocation to it; their fields are set before begin.
func newDrainer() *drainer {
	return &drainer{revoked: make(chan string, 1), limit: runtimewire.DrainLimit, poll: time.Second}
}

// identityRevoked is the tunnel's OnIdentityRevoked callback.
func (d *drainer) identityRevoked(reason string) {
	select {
	case d.revoked <- reason:
	default:
	}
}

// begin hands over the singletons: the coordinator's sockets, guard and listeners first, then the
// local API and the daemon lock the next generation waits for. It runs once.
func (d *drainer) begin(ctx context.Context) error {
	if !d.begun.CompareAndSwap(false, true) {
		return nil
	}
	err := d.coordinator.Drain()
	d.stopLive()
	return errors.Join(err, d.identity.BeginDrain(ctx))
}

// wait holds until the last held connection (forward or local API peer stream such as an exec) closes, the drain cap, a revocation or ctx's end, and
// names which one it was.
func (d *drainer) wait(ctx context.Context) string {
	until := time.Now().Add(d.limit)
	limit := time.NewTimer(d.limit)
	defer limit.Stop()
	poll := time.NewTicker(d.poll)
	defer poll.Stop()
	published := -1
	for {
		streams := liveconnections.Held()
		if streams == 0 {
			return "drained"
		}
		if streams != published {
			d.publish(streams, until)
			published = streams
		}
		select {
		case <-poll.C:
		case <-limit.C:
			return "drain_limit"
		case reason := <-d.revoked:
			// The streams end when the coordinator stops; say which ones the revocation cut.
			for _, stream := range d.coordinator.DrainingStreams() {
				diagnosticlog.TryWarn("a held forwarded connection ended: this daemon generation's identity was revoked", "reason", reason, "stream", stream)
			}
			return reason
		case <-ctx.Done():
			return "stopped"
		}
	}
}

// finish withdraws this generation's network identity, so machines drop it at once.
func (d *drainer) finish(reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	diagnosticlog.TryInfo("daemon generation finished draining", "reason", reason)
	return d.identity.Withdraw(ctx)
}
