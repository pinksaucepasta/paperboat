//go:build darwin || linux || windows

package runtime

import (
	"context"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

// ownerExecutions builds a session owner's command manager. hostd adds its
// local endpoints to each start (hostdExecutions), since only it knows them.
func ownerExecutions(ctx context.Context, workspace, shell string, durable *store.Store) (*execprocess.Manager, error) {
	environment, err := process.BaseEnvironment(shell)
	if err != nil {
		return nil, err
	}
	resources := runtimeconfig.DefaultResources
	return execprocess.NewPersistent(ctx, execprocess.Config{WorkspaceRoot: workspace, BaseEnvironment: environment, MaximumActive: resources.MaxConcurrentOps, MaximumOperations: resources.MaxConcurrentOps * 32, ReplayBytes: int(runtimeconfig.DefaultLimits.PendingOutputBytes), CancelGrace: 2 * time.Second, Store: durable})
}

// hostdExecutions forwards commands to the session owners and adds the
// launch environment hostd gave commands when it ran them itself.
type hostdExecutions struct {
	*execprocess.RemoteManager
	environment []string
}

func (h hostdExecutions) Start(ctx context.Context, request execprocess.Request) (execprocess.ExecutionService, bool, error) {
	launch := append(append([]string(nil), h.environment...), execprocess.LaunchEnvironment(ctx)...)
	return h.RemoteManager.Start(execprocess.WithLaunchEnvironment(ctx, launch), request)
}
