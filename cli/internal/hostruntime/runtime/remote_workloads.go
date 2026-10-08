//go:build darwin || linux || windows

package runtime

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// NewProductionFeatureHost constructs the actual replaceable feature runtime.
// Durable terminals and explicit executions remain owned by the native hostd.
func NewProductionFeatureHost(ctx context.Context, version string, environ func(string) string, sessions session.Service, executions execprocess.Service) (*Host, error) {
	if sessions == nil || executions == nil {
		return nil, ErrHostInvalid
	}
	return newProductionHost(ctx, version, environ, nil, HostDependencies{Sessions: sessions, Executions: executions, ReuseAgentToken: true})
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
