package server

import (
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
)

var ErrNativePrivateBinding = errors.New("native private authorization binding is invalid")

// RevalidateNativePrivate requires the signed operation credential and the
// canonical stream target to describe the same current resource. Callers must
// additionally compare the returned generations with current desired state
// immediately before dialing the origin.
func RevalidateNativePrivate(authorization Authorization, encodedTarget string, ownerEndpointID string, now time.Time) (nativeprivate.Binding, error) {
	claims, ok := authorization.Value.(auth.Claims)
	binding, err := nativeprivate.Decode([]byte(encodedTarget), now)
	if !ok || err != nil || claims.CredentialClass != "native_private" || binding.OwnerEndpointID != ownerEndpointID || claims.MachineID != ownerEndpointID ||
		claims.ResourceKind != binding.ResourceKind || claims.ResourceID != binding.ResourceID || claims.RouteID != binding.RouteID || claims.Protocol != binding.Protocol ||
		claims.TargetScheme != binding.TargetScheme || claims.TargetAddress != binding.TargetAddress || claims.ExpectedGeneration != int64(binding.ResourceGeneration) ||
		claims.RouteGeneration != int64(binding.RouteGeneration) || claims.TargetGeneration != int64(binding.TargetGeneration) || time.Unix(claims.ExpiresAt, 0).Before(binding.ExpiresAt) {
		return nativeprivate.Binding{}, ErrNativePrivateBinding
	}
	return binding, nil
}
