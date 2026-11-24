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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

type Coordinator struct {
	cfg               CoordinatorConfig
	rpcServer         *daemonrpc.Server
	rpcListener       net.Listener
	rpcBackend        *daemonrpc.LiveDaemonBackend
	errors            chan error
	servicesNudge     chan struct{}
	accessChanged     chan struct{}
	nameClient        guardedNameClient
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	running           bool
	routes            map[string]*guardedRoute
	draining          []*guardedRoute // routes a reset stopped accepting on; their forwards still run
	webRoutes         map[string]guardedWebRoute
	connections       chan struct{}
	wg                sync.WaitGroup
	snapshotMu        sync.Mutex
	queueMu           sync.Mutex
	queued            *reconcileRequest // the newest snapshot waiting for the running reconcile (guarded by queueMu)
	reconciling       bool              // guarded by queueMu
	queueCtx          context.Context   // ctx and cancel again, readable while a reconcile holds mu (guarded by queueMu)
	queueCancel       context.CancelFunc
	publishedPending  atomic.Pointer[[2]string] // step and detail as last read, for readers while a setup holds mu
	setupPartial      bool                      // a reconcile ended before it finished (guarded by mu)
	reconcileErr      error
	inventoryMu       sync.Mutex
	inventoryRevision uint64
	inventoryMachines []api.UserMachine
	// claimMachines is every machine of the last complete inventory read, online or not (guarded by mu);
	// the guard releases claims only against it. Nil until the first complete read.
	claimMachines []string
	// portNames is each listed machine's server port names and front desk, and machineAliases the name of every
	// inventory machine, from the last ApplyMachines (guarded by mu): browser routes read both.
	portNames       map[string]api.MachineServicesMachine
	machineAliases  map[string]bool
	cleanups        []*generationCleanup
	reportMu        sync.Mutex
	reconcileFailed bool
	// browserTrustReported is set once a browser certificate failure has been recorded.
	browserTrustReported bool
	// browserPending is set while the last reconciliation left browser URLs off because the browser
	// certificate could not be issued or trusted (guarded by mu); it is retried like a failed reconcile.
	browserPending bool
	// overLimit says what of the last reconciliation is above the machine guard's limits and not served on this
	// computer, or "" (guarded by mu). It is not retried: only a smaller account changes it.
	overLimit string
	// namesRefused is set while the guard refused some machine names of the last reconciliation (guarded by mu).
	namesRefused bool
	// browserPortsPending is set while a per-port browser listener of the last reconciliation could not be
	// bound or started (guarded by mu).
	browserPortsPending bool
	// browserGatewayBusy is set while another program owns a browser gateway port (:80 or :443), so browser URLs
	// are off; browserGatewayOwner names that program when the OS says (both guarded by mu). It is retried like
	// a failed reconcile, so the URLs return by themselves once the port is free.
	browserGatewayBusy  bool
	browserGatewayOwner string
	// browserHTTPBusy is set while another program owns only the gateway's :80: https:// browser URLs work, the
	// http:// links do not (guarded by mu; browserGatewayOwner names the program). Retried like browserGatewayBusy.
	browserHTTPBusy bool
	// retry re-applies the latest snapshot after a failed or incomplete reconciliation (guarded by retryMu).
	retryMu       sync.Mutex
	retryTimer    *time.Timer
	retryAttempt  int
	retryPeers    []*daemonrpc.PeerUpdate
	retryRevision uint64
	// lastPeers/lastRevision are the newest snapshot reconciled; the guard watch re-applies it after the guard
	// restarts (guarded by snapshotMu; haveSnapshot is false until the first reconcile).
	lastPeers    []*daemonrpc.PeerUpdate
	lastRevision uint64
	haveSnapshot bool
	domain       atomic.Pointer[string] // the local access domain, as the machine guard reports it
}

// connectTimeout bounds connecting to the protected machine-name service and stopping the coordinator.
const connectTimeout = 5 * time.Second

// reconcileTimeout is the default bound for applying one peer snapshot.
const reconcileTimeout = 15 * time.Second

func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	if cfg.SocketAddress == "" {
		cfg.SocketAddress = daemonrpc.DefaultSocketAddress()
	}
	if cfg.NameClient != nil && cfg.ConnectNameClient != nil {
		return nil, errors.New("configure either an injected name client or a reconnect factory")
	}
	hasNames := cfg.NameClient != nil || cfg.ConnectNameClient != nil
	if hasNames != (cfg.DialMachine != nil) {
		return nil, errors.New("guarded names require an authenticated machine dialer")
	}
	if cfg.ReconcileTimeout <= 0 {
		cfg.ReconcileTimeout = reconcileTimeout
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = reconcileRetryBase
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = reconcileRetryMax
	}
	coordinator := &Coordinator{
		cfg:           cfg,
		nameClient:    cfg.NameClient,
		rpcBackend:    daemonrpc.NewLiveDaemonBackend(buildinfo.Version),
		routes:        make(map[string]*guardedRoute),
		webRoutes:     make(map[string]guardedWebRoute),
		connections:   make(chan struct{}, 256),
		errors:        make(chan error, 1),
		servicesNudge: make(chan struct{}, 1),
		accessChanged: make(chan struct{}, 1),
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
	c.queueMu.Lock()
	c.queueCtx, c.queueCancel = c.ctx, c.cancel
	c.queueMu.Unlock()
	c.mu.Unlock()
	if c.cfg.ConnectNameClient != nil {
		connectCtx, cancel := context.WithTimeout(parent, connectTimeout)
		client, err := c.cfg.ConnectNameClient(connectCtx)
		cancel()
		if err != nil {
			// The protected machine-name service being down must not stop the daemon: a daemon that
			// exits at start is restarted in a loop and can no longer be updated or inspected.
			// It runs degraded without local machine names, and every reconciliation retries the
			// connection until the service returns.
			c.observe(parent, "guard_connect", coordinatorOperationFailure("guard_connect", err))
		} else {
			c.mu.Lock()
			c.nameClient = client
			c.mu.Unlock()
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
	c.wg.Add(2)
	runCtx, watchEvery := c.ctx, guardWatchInterval
	go func() {
		defer c.wg.Done()
		c.watchGuard(runCtx, watchEvery)
	}()
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

// ApplyServices projects a fresh announced-service list onto the machines of the last inventory.
func (c *Coordinator) ApplyServices(ctx context.Context, services []api.MachineServicesMachine) error {
	if c == nil {
		return errors.New("local access coordinator is unavailable")
	}
	c.inventoryMu.Lock()
	machines := slices.Clone(c.inventoryMachines)
	c.inventoryMu.Unlock()
	if machines == nil {
		return nil
	}
	return c.ApplyMachines(ctx, machines, services)
}

// ApplyMachines projects authenticated, unexpired service discovery when the
// optional streaming topology endpoint is not configured.
func (c *Coordinator) ApplyMachines(ctx context.Context, machines []api.UserMachine, services []api.MachineServicesMachine) error {
	if c == nil {
		return errors.New("local access coordinator is unavailable")
	}
	request, err := c.queueMachines(ctx, machines, services)
	if request == nil {
		return err
	}
	return request.wait(ctx)
}

// queueMachines never waits for a running reconcile, so a withdrawal reaches it at once.
func (c *Coordinator) queueMachines(ctx context.Context, machines []api.UserMachine, services []api.MachineServicesMachine) (*reconcileRequest, error) {
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	c.queueMu.Lock()
	parent := c.queueCtx
	c.queueMu.Unlock()
	if parent == nil {
		return nil, errors.New("local access coordinator is not running")
	}
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.inventoryMachines = slices.Clone(machines)
	claimMachines := make([]string, 0, len(machines))
	for _, m := range machines {
		if m.ID != "" {
			claimMachines = append(claimMachines, m.ID)
		}
	}
	byID := make(map[string]api.MachineServicesMachine, len(services))
	// A listed service stays forwarded until the next list (topic, nudge or idle refresh) or until
	// its machine leaves the inventory; the server's short lease is the host's to renew.
	for _, service := range services {
		if service.ExpiresAt.After(serverclock.Now()) {
			byID[service.MachineID] = service
		}
	}
	machineAliases := make(map[string]bool, len(machines))
	for _, m := range machines {
		machineAliases[strings.ToLower(m.Alias)] = true
	}
	peers := make([]*daemonrpc.PeerUpdate, 0, len(machines))
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
		peers = append(peers, &daemonrpc.PeerUpdate{PeerId: m.ID, Alias: m.Alias,
			AssignedIp: m.AssignedIP, Approved: m.NetworkApproved, Online: m.Online, ExportedPorts: ports})
	}
	c.inventoryRevision++
	revision := c.inventoryRevision
	return c.queueSnapshot(peers, revision, &machineNames{claimMachines, byID, machineAliases}), nil
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

func (c *Coordinator) applySnapshot(peers []*daemonrpc.PeerUpdate, revision uint64) {
	c.mu.Lock()
	parent := c.ctx
	c.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	_ = c.reconcileSnapshot(parent, peers, revision)
}

// machineNames is the inventory part of a snapshot, applied with it.
type machineNames struct {
	claimMachines  []string
	portNames      map[string]api.MachineServicesMachine
	machineAliases map[string]bool
}

// reconcileRequest is one queued snapshot and everyone waiting for it.
type reconcileRequest struct {
	peers    []*daemonrpc.PeerUpdate
	revision uint64
	names    *machineNames
	done     chan struct{}
	err      error
	left     atomic.Bool // a caller stopped waiting: the inventory is told when the setup ends
}

// reconcileSnapshot queues peers and waits for its result until ctx ends. One reconcile runs at a time; snapshots that
// arrive meanwhile collapse into the newest, and no caller's deadline cuts the running one (a guard call cut halfway
// drops the guard connection and every listener on it).
func (c *Coordinator) reconcileSnapshot(ctx context.Context, peers []*daemonrpc.PeerUpdate, revision uint64) error {
	return c.queueSnapshot(peers, revision, nil).wait(ctx)
}

func (c *Coordinator) queueSnapshot(peers []*daemonrpc.PeerUpdate, revision uint64, names *machineNames) *reconcileRequest {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if c.queued == nil {
		c.queued = &reconcileRequest{done: make(chan struct{})}
	}
	c.queued.peers, c.queued.revision = peers, revision
	if names != nil {
		c.queued.names = names
	}
	request := c.queued
	if !c.reconciling {
		c.reconciling = true
		go c.runReconciles()
	}
	return request
}

func (r *reconcileRequest) wait(ctx context.Context) error {
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		r.left.Store(true)
		select {
		case <-r.done:
			return r.err
		default:
		}
		return coordinatorOperationFailure("local_access_setup", ctx.Err())
	}
}

func (c *Coordinator) runReconciles() {
	for {
		c.queueMu.Lock()
		request := c.queued
		c.queued = nil
		if request == nil {
			c.reconciling = false
			c.queueMu.Unlock()
			return
		}
		c.queueMu.Unlock()
		c.mu.Lock()
		ctx := c.ctx
		if names := request.names; names != nil {
			c.claimMachines, c.portNames, c.machineAliases = names.claimMachines, names.portNames, names.machineAliases
		}
		c.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		c.snapshotMu.Lock()
		started := time.Now()
		diagnosticlog.TryDebug("local access setup started", "revision", request.revision, "machines", len(request.peers))
		request.err = c.reconcileSnapshotLocked(ctx, request.peers, request.revision)
		var operation coordinatorOperationError
		step := ""
		if errors.As(request.err, &operation) {
			step = operation.LocalAccessStep()
		}
		diagnosticlog.TryDebug("local access setup ended", "revision", request.revision, "duration_ms", time.Since(started).Milliseconds(), "step", step, "error", request.err)
		c.snapshotMu.Unlock()
		close(request.done)
		if request.left.Load() {
			select {
			case c.accessChanged <- struct{}{}:
			default:
			}
		}
	}
}

// superseded reports whether a snapshot different from the running one waits.
func (c *Coordinator) superseded() bool {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	return c.queued != nil && !slices.EqualFunc(c.lastPeers, c.queued.peers, samePeer)
}

func (c *Coordinator) reconcileSnapshotLocked(ctx context.Context, peers []*daemonrpc.PeerUpdate, revision uint64) error {
	unchanged := c.haveSnapshot && slices.EqualFunc(c.lastPeers, peers, samePeer)
	c.lastPeers, c.lastRevision, c.haveSnapshot = peers, revision, true
	if unchanged && c.deferToPendingRetry(peers, revision) {
		return c.reconcileErr
	}
	before := c.PendingLocalAccessStep()
	err := c.applySnapshotLocked(ctx, peers, revision)
	c.reconcileErr = err
	if c.PendingLocalAccessStep() != before { // pb doctor reads the inventory snapshot: refresh it now
		select {
		case c.accessChanged <- struct{}{}:
		default:
		}
	}
	if err == nil {
		c.reportMu.Lock()
		c.reconcileFailed = false
		c.reportMu.Unlock()
	}
	c.mu.Lock()
	incomplete := err != nil || c.browserPending || c.namesRefused || c.browserPortsPending || c.browserGatewayBusy || c.browserHTTPBusy
	c.mu.Unlock()
	if incomplete {
		c.scheduleReconcileRetry(peers, revision)
	} else {
		c.resetReconcileRetry()
	}
	return err
}

// guardWatchInterval is how often a successful reconciliation checks that the guard connection still answers.
// A restarted guard drops the connection and every lease on it; nothing else tells the daemon, because a
// reconciliation that succeeded schedules no retry (a Linux daemon stayed unregistered until it was restarted).
var guardWatchInterval = 10 * time.Second

func (c *Coordinator) watchGuard(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkGuard(ctx)
		}
	}
}

// checkGuard re-applies the latest snapshot when the connected guard no longer answers; the reconcile drops
// the dead client, and its own retry backoff connects to the restarted guard.
func (c *Coordinator) checkGuard(ctx context.Context) {
	c.mu.Lock()
	client := c.nameClient
	c.mu.Unlock()
	if client == nil {
		return // no connection to lose: a failed reconcile is already retrying
	}
	statusCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	_, err := client.Status(statusCtx)
	cancel()
	if err == nil || ctx.Err() != nil {
		return
	}
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	if !c.haveSnapshot {
		return
	}
	_ = c.reconcileSnapshotLocked(ctx, c.lastPeers, c.lastRevision)
}

// Local access setup that fails (machine names, browser aliases, the browser certificate) is retried with
// capped exponential backoff and jitter until it succeeds, instead of waiting for the next topology change.
var (
	reconcileRetryBase = time.Second
	reconcileRetryMax  = time.Minute
)

func (c *Coordinator) scheduleReconcileRetry(peers []*daemonrpc.PeerUpdate, revision uint64) {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	c.retryPeers, c.retryRevision = peers, revision // the newest snapshot is the one retried
	if c.retryTimer != nil {
		return
	}
	c.retryAttempt++
	delay := backoff.Retry(c.retryAttempt, c.cfg.RetryBase, c.cfg.RetryMax)
	c.retryTimer = time.AfterFunc(delay, func() {
		c.retryMu.Lock()
		c.retryTimer = nil
		peers, revision := c.retryPeers, c.retryRevision
		c.retryMu.Unlock()
		c.mu.Lock()
		running := c.running
		c.mu.Unlock()
		if running {
			c.applySnapshot(peers, revision)
		}
	})
}

// deferToPendingRetry hands an unchanged snapshot to the retry already waiting out its backoff, so repeated
// snapshots and guard checks do not re-run a failing reconcile faster than the backoff (a Mac's guard and name
// services ran hot in such a loop). A changed snapshot runs at once: it may withdraw access or fix the failure.
func (c *Coordinator) deferToPendingRetry(peers []*daemonrpc.PeerUpdate, revision uint64) bool {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	if c.retryTimer == nil {
		return false
	}
	c.retryPeers, c.retryRevision = peers, revision
	return true
}

func samePeer(a, b *daemonrpc.PeerUpdate) bool {
	return (a == nil) == (b == nil) && (a == nil || a.PeerId == b.PeerId && a.Alias == b.Alias &&
		a.AssignedIp == b.AssignedIp && a.Online == b.Online && a.Approved == b.Approved &&
		slices.Equal(a.ExportedPorts, b.ExportedPorts))
}

// PendingLocalAccessStep names the local access step still being retried after the last reconciliation
// reported success with browser URLs off (the browser certificate), or "" when nothing is pending.
func (c *Coordinator) PendingLocalAccessStep() string { return c.pendingLocalAccess()[0] }

// pendingLocalAccess never waits for a running setup, which holds mu: it then answers what the last one left.
func (c *Coordinator) pendingLocalAccess() [2]string {
	if !c.mu.TryLock() {
		if last := c.publishedPending.Load(); last != nil {
			return *last
		}
		return [2]string{}
	}
	defer c.mu.Unlock()
	pending := [2]string{c.pendingStepLocked(), c.pendingDetailLocked()}
	c.publishedPending.Store(&pending)
	return pending
}

func (c *Coordinator) pendingStepLocked() string {
	if c.namesRefused {
		return "machine_names"
	}
	if c.browserGatewayBusy {
		return "browser_gateway_port"
	}
	if c.browserHTTPBusy {
		return "browser_http_port"
	}
	if c.browserPending {
		return "browser_certificate"
	}
	if c.browserPortsPending {
		return "browser_port_listener"
	}
	if c.overLimit != "" {
		return "machine_limit"
	}
	return ""
}

// PendingLocalAccessDetail is what the OS says about the pending step (the program that owns the gateway port),
// or "" when it cannot tell.
func (c *Coordinator) PendingLocalAccessDetail() string { return c.pendingLocalAccess()[1] }

func (c *Coordinator) pendingDetailLocked() string {
	if c.browserGatewayBusy || c.browserHTTPBusy {
		return c.browserGatewayOwner
	}
	if !c.namesRefused && !c.browserPending && !c.browserPortsPending {
		return c.overLimit
	}
	return ""
}

func (c *Coordinator) resetReconcileRetry() {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	if c.retryTimer != nil {
		c.retryTimer.Stop()
		c.retryTimer = nil
	}
	c.retryAttempt, c.retryPeers = 0, nil
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

func (c *Coordinator) applySnapshotLocked(parent context.Context, peers []*daemonrpc.PeerUpdate, revision uint64) error {
	localPeers, skipped := mapPeerSnapshot(peers, machineloopback.DefaultCIDR)
	skipped = coordinatorFailure("machine snapshot entries left out of local access", skipped)
	c.mu.Lock()
	namesEnabled := c.nameClient != nil || c.cfg.ConnectNameClient != nil
	c.mu.Unlock()
	if namesEnabled {
		if parent == nil {
			return errors.New("local access coordinator is not running")
		}
		err := c.replaceGuardedRoutes(parent, localPeers)
		if err != nil {
			err = errors.Join(err, skipped)
			c.reportReconcileError(fmt.Errorf("local machine access withdrawn; retry after repairing the machine guard or browser trust: %w", err))
			c.rpcBackend.ApplyPeerUpdates(nil, revision, nil)
			return err
		}
		// RPC status must describe acquired native listeners, while browser URLs
		// can still offer an independently authorized route to an occupied port.
		// The address is the one the guard bound them on: its per-uid claim differs from the server's preferred
		// address when another OS user holds that one (a test VM, 2026-11-05: pb resolve said .3, the port was on .18).
		c.mu.Lock()
		active := make(map[string]map[int]bool)
		bound := make(map[string]string)
		for _, route := range c.routes {
			if active[route.machineID] == nil {
				active[route.machineID] = make(map[int]bool)
			}
			active[route.machineID][route.port] = true
			bound[route.machineID] = route.address.String()
		}
		for _, peer := range localPeers {
			if peer == nil {
				continue
			}
			if address := bound[peer.PeerId]; address != "" {
				peer.AssignedIp = address
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
	c.reportReconcileError(skipped)
	return skipped
}

func (c *Coordinator) Stop() error {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), connectTimeout)
	defer shutdownCancel()
	c.resetReconcileRetry()
	c.queueMu.Lock()
	if c.queueCancel != nil {
		c.queueCancel() // ends a running reconcile, which holds mu
	}
	c.queueCtx, c.queueCancel = nil, nil
	c.queueMu.Unlock()
	c.mu.Lock()
	server, listener, cancel := c.rpcServer, c.rpcListener, c.cancel
	c.rpcServer, c.rpcListener, c.ctx, c.cancel, c.running = nil, nil, nil, nil, false
	routes, webRoutes, nameClient := c.routes, c.webRoutes, c.nameClient
	draining := c.draining
	c.nameClient, c.draining = nil, nil
	c.routes = make(map[string]*guardedRoute)
	c.webRoutes = make(map[string]guardedWebRoute)
	if cancel != nil {
		cancel()
	}
	if server != nil || listener != nil || nameClient != nil || len(routes) > 0 || len(webRoutes) > 0 || len(draining) > 0 {
		c.launchCleanupLocked(func() error {
			var causes []error
			if nameClient != nil {
				causes = append(causes, lifecycleCleanup(nameClient.Close()))
			}
			for _, route := range routes {
				causes = append(causes, route.close())
			}
			for _, route := range draining {
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

// LocalAccessChanged wakes the machine inventory when the pending local access step changes (a retry bound the
// browser gateway again), so pb doctor stops naming a step that has cleared.
func (c *Coordinator) LocalAccessChanged() <-chan struct{} { return c.accessChanged }

// ServicesNudge wakes the machine-services refresh early (a browser request for a just-started service).
func (c *Coordinator) ServicesNudge() <-chan struct{} { return c.servicesNudge }

func (c *Coordinator) nudgeServices() {
	select {
	case c.servicesNudge <- struct{}{}:
	default:
	}
}

// awaitBrowserRoute asks for a services refresh and waits, within ctx, until hostname has a route on
// the running gateway.
func (c *Coordinator) awaitBrowserRoute(ctx context.Context, state *browserGatewayState, hostname string) bool {
	c.nudgeServices()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		routes, _, _ := state.get()
		if _, ok := browserRouteForHostDomain(routes, hostname, c.localDomain()); ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
