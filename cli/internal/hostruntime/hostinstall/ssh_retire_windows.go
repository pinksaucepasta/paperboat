//go:build windows

package hostinstall

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

// pruneWindowsSSHGenerations runs under the native installation lock after
// recovery and verification of the committed installation source. It preserves
// the current source even before the worker publishes its SSH target. Accepted
// connections across every worker epoch are owned and counted by the carrier.
func pruneWindowsSSHGenerations(ctx context.Context, carrier CarrierConfig, current installsource.Source) error {
	if current.Validate() != nil || current.Platform != "windows" || carrier.Validate() != nil {
		return ErrInvalidRequest
	}
	capability, err := ReadCarrierCapability(ctx, carrier)
	if err != nil {
		return err
	}
	defer clear(capability)
	control, err := carriercontrol.NewClient(carrier.ControlEndpoint, capability, carrier.Source.SHA256)
	if err != nil {
		return err
	}
	generations, err := windowsopenssh.ListGenerations(ctx, carrier.OwnerSID)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		if generation.Source.SHA256 == current.SHA256 {
			continue
		}
		if err := control.CheckSourceRetirement(ctx, generation.Source.SHA256); err != nil {
			if errors.Is(err, carriercontrol.ErrBusy) || errors.Is(err, carriercontrol.ErrFenced) {
				continue
			}
			return err
		}
		if err := windowsopenssh.RetireGeneration(ctx, generation); err != nil {
			return err
		}
	}
	return nil
}
