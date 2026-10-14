//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"os"
	"time"
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

func (f *activeFeature) Health(ctx context.Context) error { return f.bridge.Health(ctx) }
func (f *activeFeature) Shutdown(ctx context.Context) error {
	defer f.bridge.Close()
	return f.host.Shutdown(ctx)
}
func newWorkerFeature(ctx context.Context, endpoint string, token []byte, worker string, epoch uint64, version string) (workerFeature, error) {
	bridge, err := workloadbridge.NewClient(endpoint+".workloads", token, worker, epoch)
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
	dispatch, err := featureNativeDispatch(ctx, bridge, worker, epoch)
	if err != nil {
		return nil, err
	}
	var reconcileSSH func(context.Context, bool) error
	if dispatch != nil {
		reconcileSSH = func(ctx context.Context, enabled bool) error {
			return reconcileFeatureCarrierSSH(ctx, worker, epoch, enabled)
		}
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

func reconcileFeatureCarrierSSH(ctx context.Context, worker string, epoch uint64, enabled bool) error {
	instance, err := featureCarrierInstance()
	if err != nil {
		return err
	}
	config, err := hostinstall.ReadCarrierConfig(ctx, instance)
	if err != nil {
		return err
	}
	capability, err := hostinstall.ReadCarrierCapability(ctx, config)
	if err != nil {
		return err
	}
	defer clear(capability)
	client, err := carriercontrol.NewClient(config.ControlEndpoint, capability, config.Source.SHA256)
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

func featureNativeDispatch(ctx context.Context, bridge *workloadbridge.Client, worker string, epoch uint64) (*server.NativeDispatchConfig, error) {
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
	return &server.NativeDispatchConfig{Token: base64.RawURLEncoding.EncodeToString(capability), HandshakeTimeout: 15 * time.Second, Fence: func(ctx context.Context, requestedWorker string, requestedEpoch uint64) (func(), <-chan struct{}, error) {
		if requestedWorker != worker || requestedEpoch != epoch {
			return nil, nil, hostdproto.ErrFenced
		}
		return bridge.AcquireAdmission(ctx)
	}}, nil
}
