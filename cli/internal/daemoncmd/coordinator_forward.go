package daemoncmd

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"slices"
	"sync"
	"syscall"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

func (c *Coordinator) serveRoute(route *guardedRoute) {
	defer c.wg.Done()
	for {
		local, err := route.listener.Accept()
		if err != nil {
			// While the daemon closes a route, the guard shuts the shared socket down when the
			// lease is released, so a pending accept ends with EINVAL instead of "closed".
			if route.ctx.Err() == nil || !(coordinatorExpected(err, false) || errors.Is(err, syscall.EINVAL)) {
				c.retireFailedRoute(route, err)
			}
			return
		}
		select {
		case c.connections <- struct{}{}:
		default:
			c.observe(route.ctx, "delivery", lifecycleCleanup(local.Close()))
			continue
		}
		if !route.startForward() {
			c.observe(route.ctx, "delivery", lifecycleCleanup(local.Close()))
			<-c.connections
			return
		}
		go func() {
			defer route.wg.Done()
			defer func() { <-c.connections }()
			c.forward(route.ctx, local, route.machineID, route.port)
		}()
	}
}

// Withdraw only the failed owning listener. A late failure from an old
// generation cannot erase a replacement or another machine's native readiness.
func (c *Coordinator) retireFailedRoute(route *guardedRoute, cause error) {
	failure := coordinatorFailure("local machine listener failed", cause, route.close())
	c.snapshotMu.Lock()
	c.mu.Lock()
	removed := false
	for key, current := range c.routes {
		if current == route {
			delete(c.routes, key)
			removed = true
		}
	}
	c.mu.Unlock()
	if removed {
		if c.rpcBackend != nil {
			status, err := c.rpcBackend.GetStatus(route.ctx)
			if err != nil {
				failure = coordinatorFailure("local machine listener failed", failure, err)
			} else {
				updates := make([]*pbSync.PeerUpdate, 0, len(status.Peers))
				urls := make(map[string]map[int32]string, len(status.Peers))
				for _, peer := range status.Peers {
					ports := slices.Clone(peer.ForwardedPorts)
					if peer.PeerId == route.machineID {
						ports = slices.DeleteFunc(ports, func(port int32) bool { return int(port) == route.port })
					}
					updates = append(updates, &pbSync.PeerUpdate{PeerId: peer.PeerId, Alias: peer.Alias, AssignedIp: peer.AssignedIp, Approved: peer.Approved, Online: peer.Online, ExportedPorts: ports})
					urls[peer.PeerId] = maps.Clone(peer.BrowserUrls)
				}
				c.rpcBackend.ApplyPeerUpdates(updates, status.Generation, urls)
			}
		}
		c.reconcileErr = failure
	}
	c.snapshotMu.Unlock()
	if removed {
		c.reportReconcileFailure(route.ctx, "listener_accept", failure)
	} else if !coordinatorExpected(failure, false) {
		c.observe(route.ctx, "listener_accept", failure)
	}
}

func (c *Coordinator) forward(ctx context.Context, local net.Conn, machineID string, port int) {
	var causes []error
	var remote net.Conn
	stage := "target_connect"
	var localOnce, remoteOnce sync.Once
	var localErr, remoteErr error
	closeLocal := func() { localOnce.Do(func() { localErr = lifecycleCleanup(local.Close()) }) }
	closeRemote := func() {
		if remote != nil {
			remoteOnce.Do(func() { remoteErr = lifecycleCleanup(remote.Close()) })
		}
	}
	defer func() {
		closeLocal()
		closeRemote()
		causes = append(causes, localErr, remoteErr)
		err := coordinatorFailure("local machine stream failed", causes...)
		if stage == "delivery" || ctx.Err() != nil {
			if coordinatorExpected(err, true) {
				return
			}
		}
		c.observe(ctx, stage, err)
	}()
	var err error
	remote, err = c.cfg.DialMachine(ctx, machineID, port)
	if err != nil {
		causes = append(causes, err)
		return
	}
	defer liveconnections.TrackForward()()
	stage = "delivery"
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(remote, local)
		if half, ok := remote.(interface{ CloseWrite() error }); ok {
			err = errors.Join(err, half.CloseWrite())
		}
		done <- err
	}()
	go func() {
		_, err := io.Copy(local, remote)
		if half, ok := local.(interface{ CloseWrite() error }); ok {
			err = errors.Join(err, half.CloseWrite())
		}
		done <- err
	}()
	canceled := ctx.Done()
	for remaining := 2; remaining > 0; {
		select {
		case copyErr := <-done:
			remaining--
			if copyErr != nil {
				causes = append(causes, copyErr)
				closeLocal()
				closeRemote()
			}
		case <-canceled:
			closeLocal()
			closeRemote()
			canceled = nil
		}
	}
}

func (c *Coordinator) stopStarted() error { return c.Stop() }
