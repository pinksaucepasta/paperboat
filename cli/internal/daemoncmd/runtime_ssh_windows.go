//go:build windows

package daemoncmd

import (
	"context"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
)

func runWindowsSSHGeneration(ctx context.Context, instance, generation string) error {
	return windowsopenssh.RunGenerationServiceForInstance(ctx, instance, generation)
}
