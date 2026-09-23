package localdaemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/identitybootstrap"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/trustedkeys"
)

var ErrPeerApprovalSignerUnavailable = errors.New("peer enrollment signer is unavailable")

// PeerApprovalSignerUnavailableError is a non-fatal local capability result:
// this authenticated profile can verify the account root, but it does not
// possess a signing key required to sign pending endpoint certificates.
type PeerApprovalSignerUnavailableError struct {
	PendingRequests int
}

func (e *PeerApprovalSignerUnavailableError) Error() string {
	return fmt.Sprintf("%v for %d pending request(s)", ErrPeerApprovalSignerUnavailable, e.PendingRequests)
}

func (e *PeerApprovalSignerUnavailableError) Unwrap() error {
	return ErrPeerApprovalSignerUnavailable
}

// ApproveOwnedPeerEnrollments completes the authenticated one-shot enrollment
// handshake. The pending request is eligible only when its endpoint ID is
// already present in the account-scoped machine list. The exact server-issued
// safety code is then passed to the existing root-key signer, so the daemon
// cannot approve an endpoint outside this account or fabricate trust data.
func ApproveOwnedPeerEnrollments(ctx context.Context, store config.ProfileStore, profile config.Profile, client *api.Client, machines []api.UserMachine) error {
	if client == nil || profile.Account.ID == "" || profile.CLIClientSessionID == "" || profile.Issuer == "" {
		return errors.New("automatic peer enrollment approval is not configured")
	}
	owned := make(map[string]struct{}, len(machines))
	for _, machine := range machines {
		if machine.ID != "" && machine.State != "revoked" && machine.State != "deleted" {
			owned[machine.ID] = struct{}{}
		}
	}
	pending, err := client.PendingE2EEEndpoints(ctx)
	if err != nil {
		return inventorySourceFailure("peer_pending", err)
	}
	eligible := make([]api.PendingEndpointIdentity, 0, len(pending))
	for _, request := range pending {
		if request.Role == "cli" {
			eligible = append(eligible, request)
			continue
		}
		if request.Role != "" && request.Role != "machine" {
			return errors.New("automatic peer enrollment returned an invalid endpoint role")
		}
		if _, ok := owned[request.EndpointID]; !ok {
			continue
		}
		eligible = append(eligible, request)
	}
	if len(eligible) == 0 {
		return nil
	}
	seed, err := store.PeerApprovalSigningKey(profile.Issuer, profile.Account.ID, profile.CLIClientSessionID)
	if errors.Is(err, config.ErrSecretNotFound) {
		if err := validateVerifierOnlyRoot(ctx, store, profile, client); err != nil {
			return inventorySourceFailure("peer_root", err)
		}
		return &PeerApprovalSignerUnavailableError{PendingRequests: len(eligible)}
	}
	if err != nil {
		return inventorySourceFailure("peer_signer", err)
	}
	clear(seed)
	for _, request := range eligible {
		approval := identitybootstrap.ApprovalRequest{
			Store: store, Client: client, Issuer: profile.Issuer, AccountID: profile.Account.ID,
			CLIClientSessionID: profile.CLIClientSessionID, RequestID: request.RequestID, SafetyCode: request.SafetyCode,
		}
		if request.Role == "cli" {
			_, err = identitybootstrap.ApproveCLI(ctx, approval)
		} else {
			_, err = identitybootstrap.ApproveMachine(ctx, approval)
		}
		if err != nil {
			return inventorySourceFailure("peer_approve", err)
		}
	}
	return nil
}

// ApproveOwnedMachineEnrollment approves exactly one pending machine endpoint.
// Ownership and the pending request are refreshed inside the mutation so a
// stale daemon snapshot cannot authorize a different or revoked machine.
func ApproveOwnedMachineEnrollment(ctx context.Context, store config.ProfileStore, profile config.Profile, client *api.Client, machineID string) error {
	if client == nil || profile.Account.ID == "" || profile.CLIClientSessionID == "" || profile.Issuer == "" || machineID == "" {
		return errors.New("machine enrollment approval is not configured")
	}
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return inventorySourceFailure("peer_machines", err)
	}
	owned := false
	for _, machine := range machines {
		if machine.ID == machineID && machine.State != "revoked" && machine.State != "deleted" {
			owned = true
			break
		}
	}
	if !owned {
		return errors.New("machine is not an active machine owned by this account")
	}
	pending, err := client.PendingE2EEEndpoints(ctx)
	if err != nil {
		return inventorySourceFailure("peer_pending", err)
	}
	var selected *api.PendingEndpointIdentity
	for index := range pending {
		request := &pending[index]
		if request.EndpointID != machineID || (request.Role != "" && request.Role != "machine") {
			continue
		}
		if selected != nil {
			return errors.New("multiple pending enrollment requests exist for this machine")
		}
		selected = request
	}
	if selected == nil {
		return errors.New("no pending machine enrollment exists for this machine")
	}
	seed, err := store.PeerApprovalSigningKey(profile.Issuer, profile.Account.ID, profile.CLIClientSessionID)
	if errors.Is(err, config.ErrSecretNotFound) {
		return &PeerApprovalSignerUnavailableError{PendingRequests: 1}
	} else if err != nil {
		return inventorySourceFailure("peer_signer", err)
	}
	clear(seed)
	_, err = identitybootstrap.ApproveMachine(ctx, identitybootstrap.ApprovalRequest{
		Store: store, Client: client, Issuer: profile.Issuer, AccountID: profile.Account.ID,
		CLIClientSessionID: profile.CLIClientSessionID, RequestID: selected.RequestID, SafetyCode: selected.SafetyCode,
	})
	if err != nil {
		return inventorySourceFailure("peer_approve", err)
	}
	return nil
}

func validateVerifierOnlyRoot(ctx context.Context, store config.ProfileStore, profile config.Profile, client *api.Client) error {
	local, err := store.LoadPeerAccountRootPublic(profile.Issuer, profile.Account.ID)
	if err != nil {
		return err
	}
	defer clear(local)
	remote, err := client.E2EERoot(ctx)
	if err != nil {
		return err
	}
	trusted, err := trustedkeys.Root(remote)
	if err != nil {
		return errors.New("automatic peer enrollment returned an invalid account root")
	}
	defer trustedkeys.Clear(trusted)
	if _, ok := trustedkeys.ByPublic(trusted, local); !ok {
		return errors.New("automatic peer enrollment verifier root does not match the account root")
	}
	return nil
}
