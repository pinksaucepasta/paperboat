//go:build linux || darwin || windows

package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func runCarrier(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("carrier", flag.ContinueOnError)
	flags.SetOutput(stderr)
	instanceFlag := flags.String("instance", "", "enrolled account instance")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (runtime.GOOS == "windows") != (*instanceFlag != "") {
		return hostinstall.ErrInvalidRequest
	}
	instance, err := carrierInstance(*instanceFlag)
	if err != nil {
		return err
	}
	config, err := hostinstall.ReadCarrierConfig(ctx, instance)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil || filepath.Clean(executable) != config.Executable {
		return errors.Join(hostinstall.ErrInvalidRequest, err)
	}
	return runNativeCarrier(ctx, config)
}

func runCarrierProcess(ctx context.Context, config hostinstall.CarrierConfig, ready, watchdog func() error, interval time.Duration) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := hostinstall.LockCarrier(config)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	capability, err := hostinstall.ReadCarrierCapability(ctx, config)
	if err != nil {
		return err
	}
	defer clear(capability)
	hostdToken, err := carrierHostdToken(config)
	if err != nil {
		return err
	}
	defer clear(hostdToken)
	control, err := hostdproto.NewClient(config.HostdEndpoint, hostdToken, 5*time.Second)
	if err != nil {
		return err
	}
	dispatch, err := server.NewNativeDispatchClient(server.NativeDispatchClientConfig{Address: config.WorkerAddress, Token: base64.RawURLEncoding.EncodeToString(capability), Active: control.Active, DialTimeout: 5 * time.Second})
	if err != nil {
		return err
	}
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: 3 * time.Second, DialTimeout: 10 * time.Second})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	carrier, err := hostruntime.NewProductionCarrier(runCtx, buildinfo.Version, func(key string) string { return config.Environment[key] }, dispatch, ssh)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer closeCancel()
		resultErr = errors.Join(resultErr, carrier.Shutdown(closeCtx))
	}()
	if err := carrier.Start(runCtx); err != nil {
		return err
	}
	privateControl := &carriercontrol.Server{Endpoint: config.ControlEndpoint, Token: capability, SHA256: config.Source.SHA256, SSH: ssh, Ready: make(chan error, 1)}
	privateControl.RefreshTarget = func(ctx context.Context) error {
		target, err := dispatch.SSHTarget(ctx)
		if err != nil {
			return err
		}
		_, err = ssh.ReconcileSourceTarget(ctx, target.Generation, target.Port, target.SourceSHA256)
		return err
	}
	privateControl.WorkerFence = func(ctx context.Context, worker string, epoch uint64) (func(), <-chan struct{}, error) {
		bridge, err := workloadbridge.NewClient(config.HostdEndpoint+".workloads", hostdToken, worker, epoch)
		if err != nil {
			return nil, nil, err
		}
		release, lost, err := bridge.AcquireAdmission(ctx)
		if err != nil {
			bridge.Close()
			return nil, nil, err
		}
		return func() { release(); bridge.Close() }, lost, nil
	}
	done := make(chan error, 1)
	finished := false
	go func() { done <- privateControl.Run(runCtx) }()
	defer func() {
		cancel()
		if !finished {
			resultErr = errors.Join(resultErr, <-done)
		}
	}()
	select {
	case err := <-privateControl.Ready:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ready(); err != nil {
		return err
	}
	var ticks <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			finished = true
			if err == nil {
				return carriercontrol.ErrUnavailable
			}
			return err
		case <-ticks:
			if err := watchdog(); err != nil {
				return err
			}
		}
	}
}
