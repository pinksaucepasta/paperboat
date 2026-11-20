package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/preview"
	"github.com/spf13/cobra"
)

type previewDependencies struct {
	client     func(*cobra.Command) (*api.Client, error)
	machineID  func() (string, error)
	carrier    previewCarrierFactory
	owner      func() (*preview.LocalOwnerSessionClient, error)
	isTerminal func(*cobra.Command) bool
	console    func(*cobra.Command, preview.Lease, preview.LeaseTarget, string, []string, *api.Client, <-chan error, func(context.Context, time.Duration) (time.Time, error)) (previewConsoleResult, error)
}

type previewDependenciesKey struct{}

func defaultPreviewDependencies() previewDependencies {
	return previewDependencies{
		client:     productionPreviewClient,
		machineID:  configuredMachineID,
		carrier:    newProductionPreviewCarrier,
		owner:      newProductionPreviewOwnerSessionClient,
		isTerminal: previewInteractiveTerminal,
		console:    runPreviewConsole,
	}
}

func previewDependenciesOf(command *cobra.Command) previewDependencies {
	if ctx := command.Context(); ctx != nil {
		if deps, ok := ctx.Value(previewDependenciesKey{}).(previewDependencies); ok {
			return deps
		}
	}
	return defaultPreviewDependencies()
}
