//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/rand"
	"net"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

// TerminalCarrierEndpointConfig uses the independent PTY owner's service,
// rather than hostd's worker bridge. The new transport can therefore continue
// an existing shell while hostd and the feature worker are being replaced.
type TerminalCarrierEndpointConfig struct {
	Runtime            runtimeconfig.Config
	Sessions           session.Service
	Workspace          string
	Shell              string
	Authorizer         server.AuthorizerFactory
	Journal            *operation.Journal
	RecordTerminalJoin server.TerminalJoinRecorder
}

func NewTerminalCarrierEndpoint(c TerminalCarrierEndpointConfig) (*server.Server, *nativesession.Service, error) {
	if c.Runtime.Validate() != nil || c.Sessions == nil || c.Authorizer == nil {
		return nil, nil, ErrHostInvalid
	}
	sessions := continuingSessions{Service: c.Sessions}
	launcher, err := process.NewShellLauncher(c.Shell, nil, sessions)
	if err != nil {
		return nil, nil, err
	}
	status := health.New(c.Runtime.Version, []string{"terminal.v1", "health.v1"}, nil)
	status.Set("terminal.v1", health.Ready, "", 0)
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{
		Sessions: sessions, Health: status, SessionLauncher: launcher,
		WorkspaceRoot: c.Workspace, Random: rand.Reader, RecordTerminalJoin: c.RecordTerminalJoin,
	})
	if err != nil {
		return nil, nil, err
	}
	available, err := protocol.AvailableCapabilities(dispatcher)
	if err != nil {
		return nil, nil, err
	}
	maximum := c.Runtime.Resources.MaxConcurrentOps
	if maximum == 0 {
		maximum = runtimeconfig.DefaultResources.MaxConcurrentOps
	}
	endpoint, err := server.New(server.Config{
		Negotiator: protocol.Negotiator{Available: available}, Journal: c.Journal, Handler: dispatcher,
		MaxConcurrent: maximum, HeartbeatInterval: c.Runtime.Limits.HeartbeatInterval,
		MutationDeadline: c.Runtime.Limits.MutationDeadline,
	})
	if err != nil {
		return nil, nil, err
	}
	limiter, err := server.NewConnectionLimiter(maximum)
	if err != nil {
		return nil, nil, err
	}
	associations, err := server.NewNativeAssociationManager(server.NativeAssociationConfig{Server: endpoint, Authorizer: c.Authorizer, Limiter: limiter})
	if err != nil {
		return nil, nil, err
	}
	authorize := server.CredentialStreamAuthorizer(c.Authorizer)
	serve := func(_ context.Context, _ streamauth.Header, conn net.Conn) error { return associations.Serve(conn) }
	apps, err := nativesession.New(nativesession.Config{
		Authorize: func(ctx context.Context, header streamauth.Header) (string, error) {
			value, err := authorize(ctx, header)
			return value.ResourceID, err
		}, ServeStream: serve, ServeAuthorizedTransfer: serve,
	})
	if err != nil {
		_ = endpoint.Shutdown(context.Background())
		return nil, nil, err
	}
	return endpoint, apps, nil
}
