package server

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
)

func TestRevalidateNativePrivateRejectsCredentialTargetSubstitution(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 2, RouteID: "route_1", RouteGeneration: 3, TargetGeneration: 4, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpiresAt: now.Add(time.Minute)}
	raw, _ := json.Marshal(binding)
	claims := auth.Claims{CredentialClass: "native_private", MachineID: "machine_1", ResourceKind: "tunnel", ResourceID: "tun_1", RouteID: "route_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpectedGeneration: 2, RouteGeneration: 3, TargetGeneration: 4, ExpiresAt: now.Add(2 * time.Minute).Unix()}
	if _, err := RevalidateNativePrivate(Authorization{Value: claims}, string(raw), "machine_1", now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*auth.Claims){
		"resource":   func(v *auth.Claims) { v.ResourceID = "tun_other" },
		"route":      func(v *auth.Claims) { v.RouteID = "route_other" },
		"generation": func(v *auth.Claims) { v.TargetGeneration++ },
		"protocol":   func(v *auth.Claims) { v.Protocol = "http" },
		"target":     func(v *auth.Claims) { v.TargetAddress = "127.0.0.1:5432" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := claims
			mutate(&changed)
			if !errors.Is(func() error {
				_, err := RevalidateNativePrivate(Authorization{Value: changed}, string(raw), "machine_1", now)
				return err
			}(), ErrNativePrivateBinding) {
				t.Fatal("substitution accepted")
			}
		})
	}
}
