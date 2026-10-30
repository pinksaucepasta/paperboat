//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// A source upgrade retains account custody and accepted SSH bridges. When SSH
// can retire, its carrier joins the same native cutover as hostd, rather than
// causing a second maintenance window after the installation commits.
type unixCarrierUpgrade struct {
	candidate  CarrierConfig
	capability []byte
	previous   *carriercontrol.Client
	native     *service.NativeTransactionalComponent
}

func stageUnixCarrierUpgrade(ctx context.Context, request Request, paths installPaths) (_ *unixCarrierUpgrade, resultErr error) {
	retained, err := ReadCarrierConfig(ctx, paths.instance)
	if err != nil {
		return nil, err
	}
	if retained.Source == request.Source {
		return nil, nil
	}
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return nil, err
	}
	client, err := carriercontrol.NewClient(retained.ControlEndpoint, capability, retained.Source.SHA256)
	if err != nil {
		clear(capability)
		return nil, err
	}
	if err := client.RetireSSH(ctx); err != nil {
		clear(capability)
		if errors.Is(err, carriercontrol.ErrBusy) {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			clear(capability)
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, client.ResumeSSH(recovery))
		}
	}()
	candidate, err := prepareCarrierConfig(ctx, request)
	if err != nil {
		return nil, err
	}
	candidate.Environment = retained.Environment
	stable := candidate
	stable.Source, stable.Executable = retained.Source, retained.Executable
	want, _ := json.Marshal(stable)
	have, _ := json.Marshal(retained)
	if !bytes.Equal(want, have) {
		return nil, ErrInvalidRequest
	}
	next, err := carrierInstaller(candidate)
	if err != nil {
		return nil, err
	}
	native, err := next.NativeComponent(func(ctx context.Context) error { return waitCarrierReady(ctx, candidate) })
	if err != nil {
		return nil, err
	}
	// Both native managers leave an already running job on its current image.
	if err := native.Install(ctx); err != nil {
		return nil, err
	}
	return &unixCarrierUpgrade{candidate: candidate, capability: capability, previous: client, native: native}, nil
}

func (u *unixCarrierUpgrade) stop(ctx context.Context) error {
	if u == nil {
		return nil
	}
	if err := u.native.Stop(ctx); err != nil {
		return err
	}
	return publishCarrierFiles(u.candidate, carrierConfigPath(u.candidate.Instance), u.capability)
}

func (u *unixCarrierUpgrade) finish(ctx context.Context, failed bool) error {
	if u == nil {
		return nil
	}
	defer clear(u.capability)
	if !failed {
		return nil
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return u.previous.ResumeSSH(recovery)
}

func (u *unixCarrierUpgrade) component() *service.NativeTransactionalComponent {
	if u == nil {
		return nil
	}
	return u.native
}
