//go:build darwin || linux || windows

package runtime

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// A delegated carrier reads the existing machine's signed authority. It never
// registers a network identity or publishes endpoints. Relay connections use
// isolated generation routes under the same existing signed machine identity.
type terminalCarrierNetwork struct{ tailnet.NetworkAPI }

func (terminalCarrierNetwork) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, ErrProductionInvalid
}
func (n terminalCarrierNetwork) PeerNetworkConfiguration(ctx context.Context, r api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	r.Endpoints, r.MembershipGrants = nil, false
	result, err := n.NetworkAPI.PeerNetworkConfiguration(ctx, r)
	result.EndpointReports, result.MembershipGrants = false, false
	return result, err
}
