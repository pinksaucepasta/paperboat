package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/spf13/cobra"
)

type updateDependencies struct {
	socket          func() (string, error)
	inputIsTerminal func(*cobra.Command) bool
	daemonSnapshot  func(context.Context) (localapi.Snapshot, error)
	inspect         func(context.Context, string) (installsource.Source, error)
	install         func(context.Context, string, installsource.Source, string, bool, string, string) (string, error)
	openConnections func(context.Context) (liveconnections.Counts, error)
	exportBundle    func(ctx context.Context, directory, output string) error // the signed official release as a .pb-update bundle
}

type updateDependenciesKey struct{}

func defaultUpdateDependencies() updateDependencies {
	return updateDependencies{
		socket:          updatedControlSocket,
		inputIsTerminal: updateInputIsTerminal,
		daemonSnapshot:  readLocalDaemonSnapshot,
		inspect:         installsource.InspectCandidate,
		install:         hostruntimecmd.InstallUpdateSource,
		openConnections: openConnectionsForUpdate,
		exportBundle:    exportOfficialBundle,
	}
}

func updateDependenciesOf(ctx context.Context) updateDependencies {
	if ctx != nil {
		if deps, ok := ctx.Value(updateDependenciesKey{}).(updateDependencies); ok {
			return deps
		}
	}
	return defaultUpdateDependencies()
}
