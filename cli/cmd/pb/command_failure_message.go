package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/identitybootstrap"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

func commandFailureMessage(err error, failure commandFailure) string {
	if err == nil {
		return ""
	}
	if preference, ok := soleCommandOwner(err).(preferenceLoadFailure); ok {
		return preference.Error()
	}
	// A login that signed in but could not finish names its own recovery step; a transport cause
	// underneath must not replace it with a generic service message.
	var login browserLoginFailure
	if errors.As(err, &login) && login.message != "" && failure.kind != commandCanceled && failure.kind != commandInteractiveCanceled {
		return login.message
	}
	if remote, ok := soleCommandOwner(err).(interface{ SupportReference() string }); ok && failure.kind == commandOperational && supportref.Valid(remote.SupportReference()) {
		return "The remote machine could not confirm terminal creation. Check `pb session list` before retrying; run `pb doctor` if this continues. Support reference: " + remote.SupportReference() + "."
	}
	if remote, ok := failure.owner.(interface{ windowsUninstallFailureOwnership() (string, bool) }); ok {
		if _, reported := remote.windowsUninstallFailureOwnership(); reported {
			return "The Windows uninstall helper could not confirm completion. Check the current installation before retrying uninstall; run pb doctor if this continues."
		}
		return failure.owner.Error()
	}
	const generic = "Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues."
	if failure.kind == commandRejected {
		if _, accountCredentials := soleCommandOwner(err).(*sessionauth.CredentialFailure); accountCredentials && onlyEnvironmentFailureLeaves(err, func(leaf error) bool {
			return leaf == config.ErrSecretNotFound || leaf == config.ErrNoCredentials
		}) {
			return "Paperboat sign-in credentials are unavailable. " + dashboardEnrollmentGuidance + " Then retry."
		}
		if rejected, ok := soleCommandOwner(err).(commandRejection); ok && rejected.valid() {
			return rejected.Error()
		}
	}
	switch owned := soleCommandOwner(err).(type) {
	case accessTunnelCauseError:
		switch owned.classification {
		case ErrAccessTunnelAuthentication:
			return "Paperboat authentication is required. Sign in to Paperboat, then retry."
		case ErrAccessTunnelForbidden:
			return "Private access was denied. Check that this account can access the tunnel, then retry."
		case ErrAccessTunnelRuntimeRPCUnavailable:
			return "The installed Paperboat service does not support private TCP access. Update Paperboat, then retry."
		case ErrAccessTunnelUnavailable:
			return "Paperboat private access is unavailable. Check the local Paperboat service and target machine, then retry; run `pb doctor` if this continues."
		}
	case *envCommandFailure:
		return owned.Error()
	case *envHostRefreshFailure:
		return owned.Error()
	case *configComparisonFailure:
		return owned.Error()
	case updateCompletionFailure:
		return owned.Error()
	case *hostruntimecmd.CommandFailure:
		if message := owned.Error(); message != "" {
			return "Paperboat could not finish: " + message + ". Check the current state before retrying; run `pb doctor` if this continues."
		}
	case setupFailureError:
		return owned.Error()
	case commandPreparationFailure:
		return owned.Error()
	}
	if cleanup, ok := soleCommandOwner(err).(uninstallCleanupError); ok {
		return cleanup.Error()
	}
	if failure.kind == commandUnexpected {
		if _, ok := failure.owner.(managedssh.NativeLaunchError); ok {
			return "The native SSH or transfer tool could not start. Check that the required tool is installed and executable, then retry; run `pb ssh doctor` to check SSH setup."
		}

		if _, ok := failure.owner.(*os.PathError); ok {
			return "Paperboat could not access local files. Check file permissions and the local Paperboat service, then retry; run `pb doctor` if this continues."
		}
		if failure.owner == syscall.EACCES || failure.owner == syscall.EPERM {
			return "Paperboat could not access a local resource. Check permissions and the local Paperboat service, then retry; run `pb doctor` if this continues."
		}
		return generic
	}
	owner := failure.owner
	if soleCommandOwner(err) == nil && failure.kind == commandRejected {
		return generic
	}
	if message := typedCommandOwnerMessage(owner, failure); message != "" {
		return message
	}
	if owner, ok := failure.owner.(jsonFailureExitCodeError); ok && owner.message != "" {
		return owner.message
	}
	switch failure.kind {
	case commandCanceled, commandInteractiveCanceled:
		return "Operation canceled."
	case commandUsage:
		return "The command arguments are invalid. Run `pb COMMAND --help` and retry."
	case commandDeadline:
		return "The operation timed out. Check the current state before retrying; run `pb doctor` if this continues."
	case commandNativeExit:
		return "The native command did not finish successfully."
	}
	switch failure.owner {
	case localwait.ErrMachineNotFound:
		return "The selected machine was not found. Run `pb status` and select an available machine."
	case resolver.ErrMachineNotFound:
		return "No machine has that name or ID. Run `pb machine list` to see your machines."
	case resolver.ErrSharedSessionAuthority:
		return "The server offered this shared session with different access than the terminal role you were granted, so Paperboat refused to join. Update Paperboat, ask the owner to share it again, and retry; if this continues, run `pb doctor`."
	case resolver.ErrMachineAmbiguous:
		return "More than one machine has that name. Run `pb machine list` and retry with the machine ID."
	case localwait.ErrMachineAmbiguous:
		return "The machine selection is ambiguous. Run `pb status` and retry with the machine ID."
	case localwait.ErrInvalid:
		return "The machine wait request is invalid. Run `pb wait --help` and retry."
	case tailnet.ErrAuthority, tailnet.ErrAdmission:
		return "The secure connection could not be established. Retry; if this continues, run `pb doctor`."
	case tunnel.ErrTransportLost:
		return "The terminal connection was lost and could not be restored. Retry `pb`; if this continues, run `pb doctor`."
	case api.ErrUnauthenticated:
		return "Your Paperboat session is no longer valid. " + dashboardEnrollmentGuidance + " Then retry."
	case config.ErrNoCredentials:
		return "Not signed in to Paperboat. " + dashboardEnrollmentGuidance + " Then retry."
	case config.ErrSecretNotFound:
		return "This CLI is signed in but not paired for private transport. Run the enrollment command from the Paperboat dashboard."
	case identitybootstrap.ErrPairingRequired:
		return "This CLI needs approval from a paired machine before private transport can be enabled. Run the enrollment command from the Paperboat dashboard."
	case identitybootstrap.ErrEnrollmentExpired:
		return "Private transport approval expired. Run the enrollment command from the Paperboat dashboard."
	}
	if message, ok := hostruntimecmd.FailureMessage(err); ok && failure.kind != commandOperational {
		return "Paperboat could not finish: " + message + ". Check the current state before retrying; run `pb doctor` if this continues."
	}
	if failure.kind == commandOperational {
		return "Paperboat is unreachable. Check your network connection and retry; if the service is recovering, retry in a moment."
	}
	return generic
}

func typedCommandOwnerMessage(owner error, failure commandFailure) string {
	const generic = "Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues."
	switch owner := owner.(type) {
	case usageError:
		return owner.Error()
	case unsupportedJSONOutputError:
		return "This command does not support JSON output. Run it without --json."
	case *api.APIError:
		if failure.apiCauseInvalid {
			return generic
		}
		if owner.Status == http.StatusUpgradeRequired && owner.Code == "update_required" {
			return owner.Error()
		}
		if owner.Status == http.StatusUnauthorized {
			return "Your Paperboat session is no longer valid. " + dashboardEnrollmentGuidance + " Then retry."
		}
		message := friendlyAPIError(owner)
		if owner.Code == "team_subscription_required" && owner.Status != http.StatusForbidden {
			message = ""
		}
		if message == "" {
			message = apiErrorFallback(owner)
		}
		message = sentence(message)
		if supportref.Valid(owner.SupportReference) {
			message += " Support reference: " + owner.SupportReference + "."
		}
		return message
	case *configComparisonFailure:
		return owner.Error()
	case updateCompletionFailure:
		return owner.Error()
	case *envHostRefreshFailure:
		return owner.Error()
	case *envCommandFailure:
		return owner.Error()
	case *localapi.RemoteError:
		if owner.Code == "environment_unavailable" {
			return "Managed environment variables could not be delivered on the target. Check ENV delivery and retry."
		}
		return "The local Paperboat service could not complete the request. Check its status with `pb status`, then retry; run `pb doctor` if this continues."
	case *TunnelCreateExistingError:
		return "A tunnel with this name already exists; nothing was changed. Inspect it with `pb tunnel status <tunnel>`."
	case *TunnelCreateChangedError:
		if tunnelID := safeTunnelCreateTarget(owner.TunnelID); tunnelID != "" {
			if recovery := safeTunnelCreateRecoveryCommand(tunnelID, owner.RecoveryCommand); recovery != "" {
				return fmt.Sprintf("Tunnel %s was created, but a later step failed; it was preserved. Recover with `%s`.", tunnelID, recovery)
			}
			return fmt.Sprintf("Tunnel %s was created, but a later step failed; it was preserved. Inspect it with `pb tunnel status %s` before retrying.", tunnelID, tunnelID)
		}
		return "The tunnel was created, but a later step failed; the tunnel was preserved. Inspect it with `pb tunnel status` using the tunnel ID shown in the command output before retrying."
	case *TunnelOperationOutcomeError:
		return "The tunnel operation failed. Inspect its current state with `pb tunnel status <tunnel>` before retrying."
	case *TunnelOperationWaitTimeoutError:
		return "The wait for the tunnel operation timed out. The operation remains active; inspect it with `pb tunnel status <tunnel>`."
	}
	return ""
}
