package server

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

var ErrStreamAuthorization = errors.New("peer stream authorization failed")

// CredentialStreamAuthorizer binds a native stream to its application grant.
func CredentialStreamAuthorizer(factory AuthorizerFactory) func(context.Context, streamauth.Header) (Authorization, error) {
	return func(ctx context.Context, header streamauth.Header) (Authorization, error) {
		if factory == nil {
			return Authorization{}, ErrStreamAuthorization
		}
		authorizer, err := factory(header.Credential)
		if err != nil || authorizer == nil {
			return Authorization{}, errors.Join(ErrStreamAuthorization, err)
		}
		if closer, ok := authorizer.(AuthorizationCloser); ok {
			defer closer.CloseAuthorization()
		}
		capability := map[string]string{"config_compare": "config.compare.v1", "terminal": "terminal.v1", "exec": "exec.v1", "ssh": "ssh.v1", "file_transfer": "file-transfer.v1", "private_preview": "preview.launch.v1", "private_http": "private.access.v1", "private_tcp": "private.access.v1", "codex": "codex.connect.v1"}[header.Consumer]
		if capability == "" {
			return Authorization{}, ErrStreamAuthorization
		}
		authorization, err := authorizer.Authorize(ctx, protocol.Frame{Type: "request", RequestID: header.StreamID, Version: protocol.ProtocolVersion, OperationID: header.OperationID, Capability: capability})
		if err != nil {
			return Authorization{}, errors.Join(ErrStreamAuthorization, err)
		}
		if header.Consumer == "config_compare" {
			claims, ok := authorization.Value.(auth.Claims)
			if !ok || header.UsageSessionID == "" || header.UsageSessionID != claims.JTI {
				return Authorization{}, ErrStreamAuthorization
			}
		}
		if header.Consumer == "private_http" || header.Consumer == "private_tcp" {
			if _, err := RevalidateNativePrivate(authorization, header.Target, authorization.MachineID, time.Now().UTC()); err != nil {
				return Authorization{}, errors.Join(ErrStreamAuthorization, err)
			}
		}
		return authorization, nil
	}
}
