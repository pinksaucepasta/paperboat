package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	hostauth "github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

type streamCredentialAuthorizer struct {
	frame  protocol.Frame
	closed bool
	value  any
}

func (a *streamCredentialAuthorizer) Authorize(_ context.Context, frame protocol.Frame) (Authorization, error) {
	a.frame = frame
	return Authorization{ClientID: "cli_1", UserID: "account_1", MachineID: "machine_1", Value: a.value}, nil
}

func TestCredentialStreamAuthorizerValidatesNativePrivateBindingBeforeDispatch(t *testing.T) {
	now := time.Now().UTC()
	expiresAt := now.Add(time.Minute).Truncate(time.Second)
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 2, RouteID: "route_1", RouteGeneration: 3, TargetGeneration: 4, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: expiresAt}
	target, _ := json.Marshal(binding)
	claims := hostauth.Claims{CredentialClass: "native_private", MachineID: "machine_1", ResourceKind: "tunnel", ResourceID: "tun_1", ExpectedGeneration: 2, RouteID: "route_1", RouteGeneration: 3, TargetGeneration: 4, Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: expiresAt.Unix()}
	for name, testCase := range map[string]struct {
		mutate    func(*hostauth.Claims)
		wantError bool
	}{
		"valid":               {func(*hostauth.Claims) {}, false},
		"stale generation":    {func(c *hostauth.Claims) { c.RouteGeneration-- }, true},
		"target substitution": {func(c *hostauth.Claims) { c.TargetAddress = "127.0.0.1:22" }, true},
	} {
		t.Run(name, func(t *testing.T) {
			changed := claims
			testCase.mutate(&changed)
			authorize := CredentialStreamAuthorizer(func(string) (Authorizer, error) { return &streamCredentialAuthorizer{value: changed}, nil })
			header, err := streamauth.NewNativePrivate("operation_1", "private_tcp", "stream_1", "credential", now.Add(time.Minute), 1024, target)
			if err != nil {
				t.Fatal(err)
			}
			_, err = authorize(context.Background(), header)
			if (err != nil) != testCase.wantError {
				t.Fatalf("error=%v wantError=%t", err, testCase.wantError)
			}
		})
	}
}
func (a *streamCredentialAuthorizer) CloseAuthorization() { a.closed = true }

func TestCredentialStreamAuthorizerUsesCanonicalApplicationPolicy(t *testing.T) {
	for consumer, capability := range map[string]string{"terminal": "terminal.v1", "exec": "exec.v1", "ssh": "ssh.v1", "file_transfer": "file-transfer.v1", "private_preview": "preview.launch.v1", "codex": "codex.connect.v1"} {
		t.Run(consumer, func(t *testing.T) {
			var created *streamCredentialAuthorizer
			authorize := CredentialStreamAuthorizer(func(token string) (Authorizer, error) {
				if token != "credential" {
					t.Fatalf("token=%q", token)
				}
				created = &streamCredentialAuthorizer{}
				return created, nil
			})
			header, err := streamauth.New("operation_1", consumer, "stream_1", "credential", time.Now().Add(time.Minute), 1024)
			if err != nil {
				t.Fatal(err)
			}
			authorization, err := authorize(context.Background(), header)
			if err != nil {
				t.Fatal(err)
			}
			if authorization.ClientID != "cli_1" || authorization.UserID != "account_1" || authorization.MachineID != "machine_1" || created.frame.Capability != capability || created.frame.OperationID != header.OperationID || created.frame.RequestID != header.StreamID || !created.closed {
				t.Fatalf("authorization=%+v frame=%+v closed=%t", authorization, created.frame, created.closed)
			}
		})
	}
}
