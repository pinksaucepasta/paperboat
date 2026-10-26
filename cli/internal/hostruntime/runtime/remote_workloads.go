//go:build darwin || linux || windows

package runtime

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// NewProductionFeatureHost constructs the actual replaceable feature runtime.
// Durable terminals have independent owners; hostd owns explicit executions.
func NewProductionFeatureHost(ctx context.Context, version string, environ func(string) string, sessions session.Service, executions execprocess.Service, dispatch *server.NativeDispatchConfig, reconcileSSH func(context.Context, bool) error, workerEpoch uint64) (*Host, error) {
	if sessions == nil || executions == nil {
		return nil, ErrHostInvalid
	}
	if dispatch == nil {
		workerEpoch = 0
	} else if workerEpoch == 0 {
		return nil, ErrHostInvalid
	}
	return newProductionHost(ctx, version, environ, HostDependencies{Sessions: sessions, Executions: executions, ReuseAgentToken: true, NativeDispatch: dispatch, ReconcileCarrierSSH: reconcileSSH, NativeWorkerEpoch: workerEpoch})
}

type launchSessions struct {
	session.Service
	source envinject.EnvironmentSource
}

func (s launchSessions) Create(ctx context.Context, r session.CreateRequest) (session.Snapshot, error) {
	values, err := managedEnvironmentForLaunch(s.source)(ctx)
	if err != nil {
		return session.Snapshot{}, err
	}
	return s.Service.Create(session.WithLaunchEnvironment(ctx, values), r)
}

type launchExecutions struct {
	execprocess.Service
	source envinject.EnvironmentSource
}

func (s launchExecutions) Start(ctx context.Context, r execprocess.Request) (execprocess.ExecutionService, bool, error) {
	values, err := managedEnvironmentForLaunch(s.source)(ctx)
	if err != nil {
		return nil, false, err
	}
	return s.Service.Start(execprocess.WithLaunchEnvironment(ctx, values), r)
}

func (s launchSessions) Restart(id string) (session.Snapshot, error) {
	return s.RestartContext(context.Background(), id)
}
func (s launchSessions) RestartAtGeneration(id string, g uint64) (session.Snapshot, error) {
	return s.RestartAtGenerationContext(context.Background(), id, g)
}
func (s launchSessions) RestartContext(ctx context.Context, id string) (session.Snapshot, error) {
	v, err := managedEnvironmentForLaunch(s.source)(ctx)
	if err != nil {
		return session.Snapshot{}, err
	}
	return s.Service.RestartContext(session.WithLaunchEnvironment(ctx, v), id)
}
func (s launchSessions) RestartAtGenerationContext(ctx context.Context, id string, g uint64) (session.Snapshot, error) {
	v, err := managedEnvironmentForLaunch(s.source)(ctx)
	if err != nil {
		return session.Snapshot{}, err
	}
	return s.Service.RestartAtGenerationContext(session.WithLaunchEnvironment(ctx, v), id, g)
}
