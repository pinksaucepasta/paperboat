//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

func TestNativeNetworkPrivateAuthorizerUsesVerifiedAccessSession(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	factory, err := NewStaticAuthorizer(StaticAuthConfig{Issuer: "https://control.test", EnvironmentID: "env_test", MachineID: "machine_test", HelperID: "hlp_test", Keys: map[string]ed25519.PublicKey{"key-1": public}, Clock: staticClock{now}})
	if err != nil {
		t.Fatal(err)
	}
	authorize := nativeNetworkAuthorizer(factory)
	for _, consumer := range []string{"private_http", "private_tcp"} {
		t.Run(consumer, func(t *testing.T) {
			protocol, scheme, address := "tcp", "tcp", "127.0.0.1:22"
			if consumer == "private_http" {
				protocol, scheme, address = "http", "http", "127.0.0.1:3000"
			}
			binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tunnel_test", ResourceGeneration: 2, RouteID: "route_test", RouteGeneration: 3, TargetGeneration: 4, OwnerEndpointID: "machine_test", Protocol: protocol, TargetScheme: scheme, TargetAddress: address, ExpiresAt: now.Add(time.Minute)}
			raw, err := json.Marshal(binding)
			if err != nil {
				t.Fatal(err)
			}
			claims := auth.Claims{Issuer: "https://control.test", Audience: "paperboat-machine", Subject: "user_test", JTI: "jti_test", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix(), Scope: []string{"private:native"}, CredentialClass: "native_private", EnvironmentID: "env_test", AccountID: "account_test", MachineID: "machine_test", UserID: "user_test", CLIClientSessionID: "cli_test", AssignmentID: "access_test", OperationID: "operation_test", ResourceKind: binding.ResourceKind, ResourceID: binding.ResourceID, ExpectedGeneration: int64(binding.ResourceGeneration), RouteID: binding.RouteID, RouteGeneration: int64(binding.RouteGeneration), TargetGeneration: int64(binding.TargetGeneration), Protocol: protocol, TargetScheme: scheme, TargetAddress: address}
			header, err := streamauth.NewNativePrivate("operation_test", consumer, "stream_test", signStaticCredential(t, private, "key-1", claims), now.Add(time.Minute), 1024, raw)
			if err != nil {
				t.Fatal(err)
			}
			header.Target = string(raw)
			got, err := authorize(context.Background(), header)
			if err != nil || got != "access_test" {
				t.Fatalf("resource=%q err=%v", got, err)
			}
			binding.TargetAddress = "127.0.0.1:9999"
			raw, _ = json.Marshal(binding)
			header.Target = string(raw)
			if got, err = authorize(context.Background(), header); err == nil || got != "" {
				t.Fatalf("substituted target accepted resource=%q err=%v", got, err)
			}
		})
	}
}
