//go:build windows

package runtime

import (
	"context"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/server"
)

type hostAuthorizer struct{}

func (hostAuthorizer) Authorize(context.Context, protocol.Frame) (server.Authorization, error) {
	return server.Authorization{JournalBinding: "test:user:client", EnvironmentID: "env_test", UserID: "user_test", ClientID: "client_test"}, nil
}
