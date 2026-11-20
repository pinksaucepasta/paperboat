package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/prompt"
	"github.com/spf13/cobra"
)

type tunnelDependencies struct {
	client          func(*cobra.Command) (*api.Client, error)
	resolve         func(context.Context, *api.Client, string) (string, error)
	beginWorkflow   func(context.Context, tunnelCreateWorkflowRequest) (tunnelCreateWorkflow, error)
	newKey          func() (string, error)
	promptName      func(prompt.TextOptions) (string, error)
	localReport     func(context.Context) (localDoctorReport, error)
	hostDiagnostics func(context.Context) (tunnelHostDiagnostics, error)
	dnsReady        func(context.Context) bool
	probeHost       func(context.Context, string) error
	repairHost      func(context.Context) error
	policyClient    func(*cobra.Command) (lazyPolicyClient, error)
	policyMachine   func(*cobra.Command, string) (string, error)
	inspector       func(*cobra.Command) (*inspectorClients, error)
	issueInspector  func(context.Context, *api.Client, string, string, string, string) (api.InspectorCredential, error)
}

type tunnelDependenciesKey struct{}

func defaultTunnelDependencies() tunnelDependencies {
	return tunnelDependencies{
		client:          productionTunnelClientForCommand,
		resolve:         resolveTunnelSelector,
		beginWorkflow:   beginProductionTunnelCreateWorkflow,
		newKey:          newTunnelIdempotencyKey,
		promptName:      prompt.Text,
		localReport:     collectTunnelDoctorLocalReport,
		hostDiagnostics: collectTunnelHostDiagnostics,
		dnsReady:        tunnelDoctorDNSCheck,
		probeHost:       probeTunnelHostRuntime,
		repairHost:      hostruntimecmd.RepairManagedHost,
		policyClient:    productionLazyPolicyClient,
		policyMachine:   lazyPolicyMachineID,
		inspector:       productionInspectorClients,
		issueInspector:  issueInspectorCredential,
	}
}

func tunnelDependenciesOf(ctx context.Context) tunnelDependencies {
	if ctx != nil {
		if deps, ok := ctx.Value(tunnelDependenciesKey{}).(tunnelDependencies); ok {
			return deps
		}
	}
	return defaultTunnelDependencies()
}
