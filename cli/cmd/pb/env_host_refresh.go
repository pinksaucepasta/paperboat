package main

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/spf13/cobra"
)

type envHostRefreshFailure struct {
	cause              error
	sourceChanged      bool
	completed          int
	publicationPending bool
	operationCompleted bool
}

func (e *envHostRefreshFailure) Unwrap() error { return e.cause }
func (e *envHostRefreshFailure) Error() string {
	prefix := "Selected ENV recipients could not be refreshed. "
	if e.operationCompleted {
		prefix = "The ENV vault operation completed, but selected recipients still need refresh. "
	}
	if e.sourceChanged {
		prefix = "The encrypted ENV source change was saved, but selected recipients still need refresh. "
	}
	if e.publicationPending && !e.sourceChanged {
		prefix = "The ENV recipient publication remains pending. "
	}
	return prefix + e.recovery()
}
func (e *envHostRefreshFailure) recovery() string {
	if onlyEnvironmentHostRotationRequired(e.cause) {
		return "Complete the required personal or team ENV key rotation with `pb env rotate` or `pb env team rotate <team>`, then run `pb env vault resume`. Recipient access remains fenced until refreshed."
	}
	if rejected, ok := environmentAPIErrorForRecovery(e.cause); ok {
		if rejected.Code == "team_entitlement_required" {
			return "Activate Team Billing for the selected Team source, then run `pb env vault resume`."
		}
		if rejected.Status == 401 {
			return "Sign in again to the same account with `pb auth login`, then run `pb env vault resume`."
		}
		if rejected.Status == 403 {
			return "Restore the required recipient and source permissions for this account, then run `pb env vault resume`. No broader selection was authorized."
		}
	}
	switch {
	case onlyEnvironmentFailureLeaves(e.cause, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultLocked) }), onlyEnvironmentFailureLeaves(e.cause, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultChanged) }):
		return "Run `pb env vault unlock` to refresh previously authorized selections."
	case onlyEnvironmentFailureLeaves(e.cause, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultTeamGrantRequired) }):
		return "Run `pb env grants sync`, then retry `pb env vault resume`."
	default:
		return "Run `pb env vault resume` to retry any staged publication exactly and refresh previously authorized selections."
	}
}
func refreshSelectedENVHosts(command *cobra.Command, manager environmentmanager.PasswordVault, sourceChanged bool) error {
	completed, err := manager.ReconcileHosts(command.Context())
	if err != nil {
		pending := environmentFailureHasMarker(err, func(cause error) bool {
			_, ok := cause.(*environmentmanager.HostPublicationPending)
			return ok
		})
		return &envHostRefreshFailure{cause: err, sourceChanged: sourceChanged, completed: completed, publicationPending: pending, operationCompleted: true}
	}
	return nil
}

// envCommandFailure exposes only local, command-owned recovery text. The cause
// remains available for typed recovery and diagnostics, never public formatting.
type envCommandFailure struct {
	cause   error
	message string
}

func (e *envCommandFailure) Error() string { return e.message }
func (e *envCommandFailure) Unwrap() error { return e.cause }
func envCommandError(cause error, message string) error {
	return &envCommandFailure{cause: cause, message: message}
}
