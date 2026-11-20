package main

import (
	"context"

	"github.com/spf13/cobra"
)

// accessDependencies are the runtimes pb access opens: authenticated machine dialing and the local tunnel client.
type accessDependencies struct {
	machineRuntime func(*cobra.Command, string) (accessMachineRuntime, string, error)
	tunnelRuntime  func() (accessTunnelRuntime, error)
}

func defaultAccessDependencies() accessDependencies {
	return accessDependencies{machineRuntime: newProductionAccessMachineRuntime, tunnelRuntime: newProductionAccessTunnelRuntime}
}

func accessDependenciesOf(ctx context.Context) accessDependencies {
	return dependenciesIn(ctx, defaultAccessDependencies)
}
