//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"os"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

type workerFeature interface {
	Health(context.Context) error
	Shutdown(context.Context) error
}
type workerFeatureFactory func(context.Context, string, []byte, string, uint64, string) (workerFeature, error)
type activeFeature struct {
	host   *hostruntime.Host
	bridge *workloadbridge.Client
}

func (f *activeFeature) Health(ctx context.Context) error     { return f.bridge.Health(ctx) }
func (f *activeFeature) DrainLocal(ctx context.Context) error { return f.host.DrainLocal(ctx) }
func (f *activeFeature) StopAccepting()                       { f.host.StopAccepting() }
func (f *activeFeature) ResumeAccepting(ctx context.Context) error {
	return f.host.ResumeAccepting(ctx)
}
func (f *activeFeature) Shutdown(ctx context.Context) error {
	defer f.bridge.Close()
	return f.host.Shutdown(ctx)
}
func newWorkerFeature(ctx context.Context, endpoint string, token []byte, worker string, epoch uint64, version string) (workerFeature, error) {
	bridge, err := workloadbridge.NewClient(endpoint+".workloads", token, worker, epoch, "")
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			bridge.Close()
		}
	}()
	if err = bridge.Health(ctx); err != nil {
		return nil, err
	}
	sessions, err := bridge.Sessions()
	if err != nil {
		return nil, err
	}
	executions, err := bridge.Executions()
	if err != nil {
		return nil, err
	}
	dispatch, err := featureNativeDispatch(ctx, bridge)
	if err != nil {
		return nil, err
	}
	var reconcileSSH func(context.Context, bool) error
	if dispatch != nil {
		instance, err := featureCarrierInstance()
		if err != nil {
			return nil, err
		}
		ssh := &carrierSSH{
			read: func(ctx context.Context) (hostinstall.CarrierConfig, error) {
				return hostinstall.ReadCarrierConfig(ctx, instance)
			},
			paired: func(config hostinstall.CarrierConfig) bool { return carrierPairedWithHostd(config, endpoint) },
			apply: func(ctx context.Context, config hostinstall.CarrierConfig, enabled bool) error {
				return applyFeatureCarrierSSH(ctx, config, worker, epoch, enabled)
			},
		}
		reconcileSSH = ssh.reconcile
	}
	host, err := hostruntime.NewProductionFeatureHost(ctx, version, os.Getenv, sessions, executions, dispatch, reconcileSSH, epoch)
	if err != nil {
		return nil, err
	}
	if err = host.BindOwnerUpdateGate(token); err != nil {
		return nil, runtimeFailure("runtime feature startup failed", err, host.Shutdown(ctx))
	}
	if err = host.Start(ctx); err != nil {
		return nil, runtimeFailure("runtime feature startup failed", err, host.Shutdown(ctx))
	}
	if host.State() != hostruntime.Running {
		return nil, runtimeFailure("runtime feature did not become ready", hostruntime.ErrHostInvalid, host.Shutdown(ctx))
	}
	success = true
	return &activeFeature{host: host, bridge: bridge}, nil
}

// carrierSSHPairWait bounds how long a worker waits for the carrier of its hostd generation to serve.
var carrierSSHPairWait = 2 * time.Minute

// carrierSSH applies the worker's managed-SSH state to the carrier serving its hostd generation. In a Windows update
// the new hostd's worker starts while the previous generation's carrier still serves the network, and that carrier
// fences every worker but its own hostd's: that is the update in progress, not a failure. The wanted state is then
// kept and applied in the background once the active carrier pairs with this hostd, polling with backoff (50 ms
// doubling to 1 s) for at most carrierSSHPairWait; nothing is reported as an error meanwhile. It never waits in
// reconcile itself: the first reconcile runs inside worker startup, which a handover bounds to seconds.
type carrierSSH struct {
	read   func(context.Context) (hostinstall.CarrierConfig, error)
	paired func(hostinstall.CarrierConfig) bool
	apply  func(context.Context, hostinstall.CarrierConfig, bool) error

	mu      sync.Mutex
	wanted  *bool // a state still to apply once the carrier pairs
	waiting bool
}

func (c *carrierSSH) reconcile(ctx context.Context, enabled bool) error {
	config, err := c.read(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paired(config) {
		c.wanted = nil
		return c.apply(ctx, config, enabled)
	}
	c.wanted = &enabled
	if !c.waiting {
		c.waiting = true
		go c.applyWhenPaired(context.WithoutCancel(ctx))
	}
	return nil
}

func (c *carrierSSH) applyWhenPaired(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, carrierSSHPairWait)
	defer cancel()
	for delay := 50 * time.Millisecond; ; delay = min(2*delay, time.Second) {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.waiting = false
			c.mu.Unlock()
			diagnosticlog.TryWarn("managed SSH state not applied: the carrier of this hostd generation did not start serving")
			return
		case <-time.After(delay):
		}
		config, err := c.read(ctx)
		if err != nil || !c.paired(config) {
			continue
		}
		c.mu.Lock()
		wanted := c.wanted
		c.wanted, c.waiting = nil, false
		var applyErr error
		if wanted != nil {
			applyErr = c.apply(ctx, config, *wanted)
		}
		c.mu.Unlock()
		if applyErr != nil {
			diagnosticlog.TryWarn("managed SSH state could not be applied to the carrier", "error", applyErr)
		}
		return
	}
}

func applyFeatureCarrierSSH(ctx context.Context, config hostinstall.CarrierConfig, worker string, epoch uint64, enabled bool) error {
	capability, err := hostinstall.ReadCarrierCapability(ctx, config)
	if err != nil {
		return err
	}
	defer clear(capability)
	client, err := carriercontrol.NewClient(config.ControlEndpoint, capability, config.Source.SHA256, config.OwnerSID)
	if err != nil {
		return err
	}
	bound, err := client.BindWorker(worker, epoch)
	if err != nil {
		return err
	}
	if enabled {
		return bound.EnableSSH(ctx)
	}
	return bound.DisableSSH(ctx)
}

func featureNativeDispatch(ctx context.Context, bridge *workloadbridge.Client) (*server.NativeDispatchConfig, error) {
	instance, err := featureCarrierInstance()
	if err != nil {
		return nil, err
	}
	configPath, err := hostinstall.CarrierConfigPath(instance)
	if err != nil {
		return nil, err
	}
	_, err = os.Lstat(configPath)
	if errors.Is(err, os.ErrNotExist) {
		// The working native owner remains until initial carrier cutover. An
		// existing malformed declaration must never reactivate that owner.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	config, err := hostinstall.ReadCarrierConfig(ctx, instance)
	if err != nil {
		return nil, err
	}
	capability, err := hostinstall.ReadCarrierCapability(ctx, config)
	if err != nil {
		return nil, err
	}
	defer clear(capability)
	// The bridge is bound to this worker's epoch at its own hostd, so a worker
	// that hostd fenced refuses whatever the carrier last saw as active.
	return &server.NativeDispatchConfig{Token: base64.RawURLEncoding.EncodeToString(capability), HandshakeTimeout: 15 * time.Second, Fence: bridge.AcquireAdmission}, nil
}

// workerHealthRetry is the pause before the one retry of a failed feature health check. A single failed call (a
// reset connection to the session owner) ended the worker and hostd with it, cutting every carrier for the 5 s
// systemd restart; an owner that is really gone fails the retry too and ends the worker within a second.
var workerHealthRetry = 250 * time.Millisecond

func workerFeatureHealthy(ctx context.Context, feature workerFeature) error {
	err := feature.Health(ctx)
	if err == nil || ctx.Err() != nil || errors.Is(err, workloadbridge.ErrFenced) { // fenced: end at once
		return err
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(workerHealthRetry):
	}
	return feature.Health(ctx)
}
