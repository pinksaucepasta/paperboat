//go:build darwin || linux || windows

package hostruntimecmd

import (
	"context"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"os"
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
	host, err := hostruntime.NewProductionFeatureHost(ctx, version, os.Getenv, sessions, executions)
	if err != nil {
		return nil, err
	}
	if err = host.BindOwnerUpdateGate(token); err != nil {
		_ = host.Shutdown(ctx)
		return nil, err
	}
	if err = host.Start(ctx); err != nil {
		_ = host.Shutdown(ctx)
		return nil, err
	}
	if host.State() != hostruntime.Running {
		_ = host.Shutdown(ctx)
		return nil, hostruntime.ErrHostInvalid
	}
	success = true
	return &activeFeature{host: host, bridge: bridge}, nil
}
