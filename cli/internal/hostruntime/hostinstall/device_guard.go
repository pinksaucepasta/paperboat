//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/deviceguard"
)

// The privileged installer owns this machine-wide prerequisite. Installing it
// before changing user services lets a guard failure leave those services intact.
func installDeviceGuard(ctx context.Context, request Request) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.Source.Verify(request.Executable); err != nil {
		return fmt.Errorf("verify Paperboat device guard executable: %w", err)
	}
	if err := prepareDeviceGuardFoundation(); err != nil {
		return fmt.Errorf("prepare Paperboat device guard directory: %w", err)
	}
	if err := deviceguard.Install(ctx, request.Executable); err != nil {
		return fmt.Errorf("install Paperboat device guard: %w", err)
	}
	return nil
}
