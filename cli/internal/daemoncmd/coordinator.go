package daemoncmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/controlsync"
	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type Coordinator struct {
	cfg                CoordinatorConfig
	rpcServer          *daemonrpc.Server
	rpcListener        net.Listener
	rpcBackend         *daemonrpc.LiveDaemonBackend
	syncClient         *controlsync.Client
	errors             chan error
	nameClient         guardedNameClient
	ctx                context.Context
	cancel             context.CancelFunc
	mu                 sync.Mutex
	running            bool
	routes             map[string]*guardedRoute
	webRoutes          map[string]guardedWebRoute
	connections        chan struct{}
	wg                 sync.WaitGroup
	snapshotMu         sync.Mutex
	reconcileErr       error
	inventoryMu        sync.Mutex
	inventoryRevision  uint64
	inventoryTimer     *time.Timer
	stopInventoryTimer func() bool
	cleanups           []*generationCleanup
	reportMu           sync.Mutex
	reconcileFailed    bool
	domain             atomic.Pointer[string] // the local access domain, as the machine guard reports it
}

func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	if cfg.SocketAddress == "" {
		cfg.SocketAddress = daemonrpc.DefaultSocketAddress()
	}
	if cfg.SyncAddress != "" && (cfg.MachineID == "" || cfg.Token == nil) {
		return nil, errors.New("control sync requires machine identity and fresh authenticated credentials")
	}
	if cfg.NameClient != nil && cfg.ConnectNameClient != nil {
		return nil, errors.New("configure either an injected name client or a reconnect factory")
	}
	hasNames := cfg.NameClient != nil || cfg.ConnectNameClient != nil
	if hasNames != (cfg.DialMachine != nil) {
		return nil, errors.New("guarded names require an authenticated machine dialer")
	}
	if cfg.ReconcileTimeout <= 0 {
		cfg.ReconcileTimeout = controlsync.SnapshotTimeout
	}
	coordinator := &Coordinator{
		cfg:         cfg,
		nameClient:  cfg.NameClient,
		rpcBackend:  daemonrpc.NewLiveDaemonBackend(nil, buildinfo.Version),
		routes:      make(map[string]*guardedRoute),
		webRoutes:   make(map[string]guardedWebRoute),
		connections: make(chan struct{}, 256),
		errors:      make(chan error, 1),
	}
	coordinator.rpcBackend.SetApprovalHandler(cfg.ApprovePeer)
	coordinator.rpcBackend.SetAccount(cfg.AccountID, cfg.AccountLabel)
	return coordinator, nil
}

func (c *Coordinator) Start(parent context.Context) (err error) {
	defer func() {
		if err != nil {
			if _, safe := err.(coordinatorError); !safe {
				err = coordinatorFailure("local access coordinator could not start", err)
			}
		}
	}()
	if supportref.FromContext(parent) == "" {
		parent = supportref.WithContext(parent, supportref.New())
	}
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	cleanupErr := c.completedCleanupLocked()
	if len(c.cleanups) > 0 {
		c.mu.Unlock()
		return coordinatorFailure("previous local access cleanup is still running; wait for cleanup, then retry", errCoordinatorCleanupRunning, cleanupErr, parent.Err(), context.Cause(parent))
	}
	if parent.Err() != nil {
		c.mu.Unlock()
		return coordinatorFailure("local access coordinator could not start", parent.Err(), context.Cause(parent), cleanupErr)
	}
	// Completed delayed cleanup no longer blocks a fresh lifecycle, but its
	// original failure remains observable rather than disappearing on restart.
	defer func() { c.observe(parent, "lifecycle", cleanupErr) }()
	c.ctx, c.cancel = context.WithCancel(parent)
	c.mu.Unlock()
	if c.cfg.ConnectNameClient != nil {
		connectCtx, cancel := context.WithTimeout(parent, controlsync.ConnectTimeout)
		client, err := c.cfg.ConnectNameClient(connectCtx)
		cancel()
		if err != nil {
			return coordinatorFailure("protected machine-name service connection failed", coordinatorOperationFailure("guard_connect", err), c.stopStarted())
		}
		c.mu.Lock()
		c.nameClient = client
		c.mu.Unlock()
	}
	if c.cfg.SyncAddress != "" {
		client := controlsync.NewClient(controlsync.ClientConfig{ServerAddr: c.cfg.SyncAddress, MachineID: c.cfg.MachineID, Token: c.cfg.Token, TLSConfig: c.cfg.TLSConfig, Insecure: c.cfg.Insecure, OnPeerUpdate: c.applySnapshot, OnError: func(err error) {
			select {
			case c.errors <- err:
			default:
			}
		}})
		c.rpcBackend.SetSyncClient(client)
		c.syncClient = client
		if err := client.Start(c.ctx); err != nil {
			return coordinatorFailure("authenticated control sync could not start", err, c.stopStarted())
		}
	}
	listener, err := daemonrpc.Listen(c.cfg.SocketAddress)
	if err != nil {
		return coordinatorFailure("daemon socket could not open", err, c.stopStarted())
	}
	server := daemonrpc.NewServer(c.rpcBackend)
	c.mu.Lock()
	c.rpcListener, c.rpcServer, c.running = listener, server, true
	c.mu.Unlock()
	c.wg.Add(1)
	runCtx := c.ctx
	go func() {
		defer c.wg.Done()
		if err := server.Serve(listener); err != nil {
			if runCtx.Err() != nil && coordinatorExpected(err, false) {
				return
			}
			c.observe(runCtx, "listener_accept", err)
			select {
			case c.errors <- coordinatorFailure("daemon RPC listener failed", err):
			default:
			}
		}
	}()
	return nil
}

// ApplyMachines projects authenticated, unexpired service discovery when the
// optional streaming topology endpoint is not configured.
func (c *Coordinator) ApplyMachines(ctx context.Context, machines []api.UserMachine, services []api.MachineServicesMachine) error {
	if c == nil {
		return errors.New("local access coordinator is unavailable")
	}
	if c.cfg.SyncAddress != "" {
		return c.ReconcileStatus(ctx)
	}
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	c.mu.Lock()
	parent := c.ctx
	c.mu.Unlock()
	if parent == nil {
		return errors.New("local access coordinator is not running")
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.inventoryTimer != nil {
		c.inventoryTimer.Stop()
	}
	if c.stopInventoryTimer != nil {
		c.stopInventoryTimer()
	}
	byID := make(map[string]api.MachineServicesMachine, len(services))
	var expiry time.Time
	for _, service := range services {
		if service.ExpiresAt.After(time.Now()) {
			byID[service.MachineID] = service
			if expiry.IsZero() || service.ExpiresAt.Before(expiry) {
				expiry = service.ExpiresAt
			}
		}
	}
	peers := make([]*pbSync.PeerUpdate, 0, len(machines))
	for _, m := range machines {
		if m.AssignedIP == "" || !m.NetworkApproved || !m.Online || m.State != "online" {
			continue
		}
		service, found := byID[m.ID]
		var ports []int32
		if found && service.InstallationGeneration == int64(m.InstallationGeneration) {
			for _, port := range service.Services {
				if port.Port > 0 && port.Port <= 65535 {
					ports = append(ports, int32(port.Port))
				}
			}
		}
		peers = append(peers, &pbSync.PeerUpdate{PeerId: m.ID, Alias: m.Alias,
			AssignedIp: m.AssignedIP, Approved: m.NetworkApproved, Online: m.Online, ExportedPorts: ports})
	}
	c.inventoryRevision++
	revision := c.inventoryRevision
	if err := c.reconcileSnapshot(ctx, peers, revision); err != nil {
		return err
	}
	if expiry.IsZero() {
		return nil
	}
	c.inventoryTimer = time.AfterFunc(time.Until(expiry), func() {
		c.inventoryMu.Lock()
		defer c.inventoryMu.Unlock()
		if c.inventoryRevision == revision && parent.Err() == nil {
			c.applySnapshot(nil, revision)
		}
	})
	timer := c.inventoryTimer
	c.stopInventoryTimer = context.AfterFunc(parent, func() { timer.Stop() })
	return nil
}

func (c *Coordinator) reportReconcileError(err error) {
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	c.reportReconcileFailure(ctx, "reconciliation", err)
}
func (c *Coordinator) reportReconcileFailure(ctx context.Context, stage string, err error) {
	if err == nil {
		return
	}
	c.reportMu.Lock()
	if c.reconcileFailed {
		c.reportMu.Unlock()
		return
	}
	c.reconcileFailed = true
	c.reportMu.Unlock()
	c.observe(ctx, stage, err)
	if c.cfg.ReportReconcileError != nil {
		c.cfg.ReportReconcileError(coordinatorFailure("local machine access reconciliation failed", err))
	}
}

func (c *Coordinator) observe(ctx context.Context, stage string, err error) {
	if err == nil || errorreport.HTTPAttemptObserved(err) {
		return
	}
	if errorreport.ProjectFault(ctx, "paperboat-daemon", "local_gateway", stage, "local_access_failed", err).Outcome == "canceled" {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "local_gateway", stage, "local_access_failed", err)
}

// Closed resources are quiet only at their lifecycle teardown boundary. A
// mixed failure or unresolved tree must retain its independent operational cause.
func coordinatorExpected(err error, eof bool) (expected bool) {
	defer func() {
		if recover() != nil {
			expected = false
		}
	}()
	remaining := 32
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil || remaining == 0 {
			return false
		}
		remaining--
		v := reflect.ValueOf(current)
		switch v.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
			if v.IsNil() {
				return false
			}
		}
		if current == net.ErrClosed || current == os.ErrClosed || current == context.Canceled || (eof && current == io.EOF) {
			return true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > remaining {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap())
		}
		return false
	}
	return visit(err)
}
func lifecycleCleanup(err error) error {
	if err != nil && coordinatorExpected(err, false) {
		return nil
	}
	return err
}

// Called with c.mu held; cleanup never needs that lock, and remains owned when
// reconciliation returns at its deadline before a listener release completes.
func (c *Coordinator) launchCleanupLocked(work func() error) *generationCleanup {
	result := &generationCleanup{done: make(chan struct{})}
	c.cleanups = append(c.cleanups, result)
	go func() { result.err = work(); close(result.done) }()
	return result
}
func (c *Coordinator) completedCleanupLocked() error {
	var causes []error
	pending := c.cleanups[:0]
	for _, result := range c.cleanups {
		select {
		case <-result.done:
			causes = append(causes, result.err)
		default:
			pending = append(pending, result)
		}
	}
	c.cleanups = pending
	return coordinatorFailure("local access generation cleanup failed", causes...)
}

func (c *Coordinator) applySnapshot(peers []*pbSync.PeerUpdate, revision uint64) {
	c.mu.Lock()
	parent := c.ctx
	c.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	_ = c.reconcileSnapshot(parent, peers, revision)
}

func (c *Coordinator) reconcileSnapshot(ctx context.Context, peers []*pbSync.PeerUpdate, revision uint64) error {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	err := c.applySnapshotLocked(ctx, peers, revision)
	c.reconcileErr = err
	if err == nil {
		c.reportMu.Lock()
		c.reconcileFailed = false
		c.reportMu.Unlock()
	}
	return err
}

// ReconcileStatus reports the latest streaming topology reconciliation. The
// client's Start applies its first snapshot synchronously before readiness.
func (c *Coordinator) ReconcileStatus(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.reconcileErr
}

func (c *Coordinator) applySnapshotLocked(parent context.Context, peers []*pbSync.PeerUpdate, revision uint64) error {
	localPeers, err := mapPeerSnapshot(peers, machineloopback.DefaultCIDR)
	if err != nil {
		c.mu.Lock()
		guarded := c.nameClient != nil || c.cfg.ConnectNameClient != nil
		c.mu.Unlock()
		if guarded && parent != nil {
			ctx, cancel := context.WithTimeout(parent, c.cfg.ReconcileTimeout)
			withdrawalErr := c.replaceGuardedRoutes(ctx, nil)
			cancel()
			err = coordinatorFailure("machine snapshot rejected", err, withdrawalErr)
		}
		c.reportReconcileError(coordinatorFailure("machine snapshot rejected", err))
		c.rpcBackend.ApplyPeerUpdates(nil, revision, nil)
		return err
	}
	c.mu.Lock()
	namesEnabled := c.nameClient != nil || c.cfg.ConnectNameClient != nil
	c.mu.Unlock()
	if namesEnabled {
		if parent == nil {
			return errors.New("local access coordinator is not running")
		}
		ctx, cancel := context.WithTimeout(parent, c.cfg.ReconcileTimeout)
		err := c.replaceGuardedRoutes(ctx, localPeers)
		cancel()
		if err != nil {
			c.reportReconcileError(fmt.Errorf("local machine access withdrawn; retry after repairing the machine guard or browser trust: %w", err))
			c.rpcBackend.ApplyPeerUpdates(nil, revision, nil)
			return err
		}
		// RPC status must describe acquired native listeners, while browser URLs
		// can still offer an independently authorized route to an occupied port.
		c.mu.Lock()
		active := make(map[string]map[int]bool)
		for _, route := range c.routes {
			if active[route.machineID] == nil {
				active[route.machineID] = make(map[int]bool)
			}
			active[route.machineID][route.port] = true
		}
		for _, peer := range localPeers {
			if peer == nil {
				continue
			}
			forwarded := make([]int32, 0, len(peer.ExportedPorts))
			for _, port := range peer.ExportedPorts {
				if active[peer.PeerId][int(port)] {
					forwarded = append(forwarded, port)
				}
			}
			peer.ExportedPorts = forwarded
		}
		c.mu.Unlock()
	}
	c.rpcBackend.ApplyPeerUpdates(localPeers, revision, c.BrowserURLs())
	return nil
}

func (c *Coordinator) Stop() error {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), controlsync.ConnectTimeout)
	defer shutdownCancel()
	c.mu.Lock()
	server, listener, cancel := c.rpcServer, c.rpcListener, c.cancel
	c.rpcServer, c.rpcListener, c.ctx, c.cancel, c.running = nil, nil, nil, nil, false
	routes, webRoutes, nameClient, syncClient := c.routes, c.webRoutes, c.nameClient, c.syncClient
	c.nameClient, c.syncClient = nil, nil
	c.routes = make(map[string]*guardedRoute)
	c.webRoutes = make(map[string]guardedWebRoute)
	if cancel != nil {
		cancel()
	}
	if server != nil || listener != nil || nameClient != nil || syncClient != nil || len(routes) > 0 || len(webRoutes) > 0 {
		c.launchCleanupLocked(func() error {
			var causes []error
			if syncClient != nil {
				causes = append(causes, lifecycleCleanup(syncClient.Close()))
				c.rpcBackend.SetSyncClient(nil)
			}
			if nameClient != nil {
				causes = append(causes, lifecycleCleanup(nameClient.Close()))
			}
			for _, route := range routes {
				causes = append(causes, route.close())
			}
			for _, route := range webRoutes {
				causes = append(causes, lifecycleCleanup(route.proxy.Stop(shutdownCtx)))
			}
			if server != nil {
				server.Stop()
			}
			if listener != nil {
				causes = append(causes, lifecycleCleanup(listener.Close()))
			}
			c.wg.Wait()
			return coordinatorFailure("local access coordinator cleanup failed", causes...)
		})
	}
	pending := slices.Clone(c.cleanups)
	c.mu.Unlock()
	var waitErr error
	var causes []error
	completed := make(map[*generationCleanup]bool, len(pending))
	for _, cleanup := range pending {
		select {
		case <-cleanup.done:
			causes = append(causes, cleanup.err)
			completed[cleanup] = true
		case <-shutdownCtx.Done():
			waitErr = shutdownCtx.Err()
		}
	}
	c.mu.Lock()
	c.cleanups = slices.DeleteFunc(c.cleanups, func(result *generationCleanup) bool { return completed[result] })
	c.mu.Unlock()
	cleanupErr := coordinatorFailure("local access generation cleanup failed", causes...)

	// Unfinished cleanup remains tracked for a later Stop rather than being
	// forgotten or blocking beyond the existing control connection deadline.
	return coordinatorFailure("local access coordinator cleanup failed", cleanupErr, waitErr)
}

// Errors reports a terminal control-sync or daemon RPC listener failure.
func (c *Coordinator) Errors() <-chan error { return c.errors }
