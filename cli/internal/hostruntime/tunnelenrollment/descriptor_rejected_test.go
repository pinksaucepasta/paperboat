package tunnelenrollment

import (
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

// A descriptor that matches the request but carries another config generation must name that
// check (so a stuck tunnel can say why) and still be an ErrConflict for existing callers.
func TestValidateCarrierDescriptorNamesTheFailedCheck(t *testing.T) {
	now := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	endpoint := "endpoint_123e4567-e89b-42d3-a456-426614174000"
	request := ActivationRequest{AccountID: "user_1", TunnelID: "tunnel_1", HostID: "host_1", ConnectorID: "connector_1", StableEndpointID: endpoint}
	identity := ControlIdentity{AccountID: "user_1", ProcessGeneration: 3, CredentialGeneration: 2}
	welcome := connectorprotocol.Welcome{SessionID: "session_1"}
	apply := tunnelmanager.ApplyRequest{Snapshot: hoststate.ConfigSnapshot{Generation: 7, ContentHash: "sha256:abc"}}
	descriptor := carrierBootstrapDescriptor{
		Schema: carrierBootstrapSchema, Kind: "carrier_bootstrap_descriptor",
		AccountID: "user_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", HostID: "host_1",
		StableEndpointID: endpoint, SessionID: "session_1", ProcessGeneration: 3, CredentialGeneration: 2,
		ConfigGeneration: 8, ConfigContentHash: "sha256:abc",
		IssuedAt: now, ExpiresAt: now.Add(10 * time.Second),
	}
	err := validateCarrierDescriptor(descriptor, now, request, identity, welcome, apply)
	var rejected *DescriptorRejectedError
	if !errors.As(err, &rejected) || rejected.Check != "config_generation" {
		t.Fatalf("validateCarrierDescriptor() = %v, want rejected config_generation", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("rejection %v must remain ErrConflict", err)
	}
}
