package main

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	servepkg "github.com/pinksaucepasta/paperboat/internal/serve"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"reflect"
)

type commandRejectionReason uint8

const (
	commandRejectDefaultSessionRename commandRejectionReason = iota + 1
	commandRejectDefaultSessionDelete
	commandRejectOpenSessionDelete
	commandRejectMissingServer
	commandRejectNoninteractiveHome
	commandRejectCustomUpdate
	commandRejectUpdateBusy
	commandRejectUpdateVerification
	commandRejectLegacyCarrierRunning
	commandRejectActivationPending
	commandRejectInboxDeclined
	commandRejectPreviewOrigin
	commandRejectPreviewExpired
	commandRejectPreviewTimeout
	commandRejectPreviewDomainTimeout
	commandRejectConfigRepositoryEmpty
	commandRejectConfigRepositoryBranch
	commandRejectConfigRepositoryInvalid
	commandRejectTeamReserved
	commandRejectTeamRequestInvalid
	commandRejectTeamSeatRequired
	commandRejectResetRetire
	commandRejectDaemonAccount
	commandRejectSendToSameMachine
)

// commandRejection describes a command-owned refusal. Its
// finite reason controls presentation; an optional original cause still
// participates in command classification and can outrank the refusal.
type commandRejection struct {
	reason commandRejectionReason
	cause  error
}

func (e commandRejection) valid() bool {
	return e.reason >= commandRejectDefaultSessionRename && e.reason <= commandRejectSendToSameMachine
}
func (e commandRejection) Error() string {
	switch e.reason {
	case commandRejectPreviewOrigin:
		return "Cannot connect to the preview origin. Start your app and check its listening address and port, then retry. No preview was published."
	case commandRejectPreviewExpired:
		return "The preview's time limit ended before it was ready. Retry with a longer --ttl; publishing can take up to a minute."
	case commandRejectPreviewTimeout:
		return "The preview did not become ready in time and was stopped. Retry; run pb doctor if this continues."
	case commandRejectPreviewDomainTimeout:
		return "The customer domain did not become ready. Verify the displayed DNS records and certificate status, then retry."
	case commandRejectConfigRepositoryEmpty:
		return "The config repository has no commits. Push an initial commit to its sync branch, then retry; config sync does not create the first commit."
	case commandRejectConfigRepositoryBranch:
		return "The config repository does not have the sync branch. Create that branch (or pick the repository's existing branch in the machine configuration), then retry."
	case commandRejectConfigRepositoryInvalid:
		return "The config repository could not be read. Check that it exists and that its saved credentials still work; a local repository must be a bare Git repository at a path without symbolic links. Then retry."
	case commandRejectTeamReserved:
		return "That team name is already reserved. Choose another name, or run the original `pb team create` command again to reuse its payment."
	case commandRejectTeamRequestInvalid:
		return "The server rejected the team request. Use an available team name, at least one seat and a month or year interval."
	case commandRejectTeamSeatRequired:
		return "No free seat: every purchased seat is used or held by a pending invitation. Cancel a pending invitation (`pb team cancel-invite`) or add a seat in the console, then try again."
	case commandRejectDaemonAccount:
		var mismatch daemoncmd.ErrDaemonAccountMismatch
		if errors.As(e.cause, &mismatch) {
			return mismatch.Error()
		}
	case commandRejectResetRetire:
		return "The old machine record could not be removed, so nothing was reset. Check your connection and run `pb reset` again."
	case commandRejectSendToSameMachine:
		return "That is this computer. Choose another computer with --to; run pb machine list to see them."
	case commandRejectInboxDeclined:
		return "The recipient declined this transfer. Contact them before sending again."
	case commandRejectDefaultSessionRename:
		return "The default session cannot be renamed. Select another session."
	case commandRejectDefaultSessionDelete:
		return "The default session cannot be deleted. Select another closed session."
	case commandRejectOpenSessionDelete:
		return "close the terminal session before deleting its record."
	case commandRejectMissingServer:
		return "Paperboat server is not configured; set server_url or use --server."
	case commandRejectCustomUpdate:
		return "The installed Paperboat is a custom build, which cannot use official updates. To switch, run `pb update --force` (it may interrupt running work) or install an official bundle with `pb update --from <file.pb-update>`."
	case commandRejectLegacyCarrierRunning:
		return hostinstall.ErrLegacyCarrierRunning.Error()
	case commandRejectUpdateVerification:
		return "The official update could not be verified; the installed version is unchanged. Download a fresh official update bundle and retry. Force cannot bypass verification."
	case commandRejectActivationPending:
		return "An approved signed update owns installation recovery. Wait for its update service to finish, then retry; run pb doctor if this continues."
	case commandRejectUpdateBusy:
		return "Paperboat is busy; the installed version is unchanged. Wait for active work or another update to finish, then retry. Use --force only if you want to interrupt active sessions."
	case commandRejectNoninteractiveHome:
		return "pb requires a command or environment when used without an interactive terminal"
	}
	return "Paperboat could not finish the command. Check the current state before retrying."
}
func (e commandRejection) Unwrap() error { return e.cause }

// Is lets callers branch on the preview sentinel a preview refusal stands for.
func (e commandRejection) Is(target error) bool {
	switch e.reason {
	case commandRejectPreviewOrigin:
		return target == ErrPreviewOriginUnavailable
	case commandRejectPreviewExpired:
		return target == preview.ErrLeaseExpired
	case commandRejectPreviewTimeout, commandRejectPreviewDomainTimeout:
		return target == servepkg.ErrPreviewReadinessTimeout
	}
	return false
}

// API response status is supporting metadata, not permission to ignore an
// independently preserved decoder/read failure. This proof is limited to the
// private body-free status cause emitted by the API transport boundary.
func commandAPIStatusMetadataOnly(err error, status int) (pure bool) {
	defer func() {
		if recover() != nil {
			pure = false
		}
	}()
	remaining := 32
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil || remaining == 0 {
			return false
		}
		remaining--
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
			if value.IsNil() {
				return false
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > remaining {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if unary, ok := current.(interface{ Unwrap() error }); ok {
			return visit(unary.Unwrap())
		}
		metadata, ok := current.(interface{ DiagnosticStatus() int })
		return ok && status >= 400 && status <= 599 && metadata.DiagnosticStatus() == status
	}
	return visit(err)
}

func signedActivationPending(err error) bool {
	if err == hostinstall.ErrSignedUpdatePending {
		return true
	}
	remote, ok := err.(*elevation.RemoteError)
	return ok && remote != nil && remote.Code == "activation_pending"
}

// rejectDaemonAccount presents the local service's account refusal as a command-owned rejection.
func rejectDaemonAccount(err error) error {
	var mismatch daemoncmd.ErrDaemonAccountMismatch
	if errors.As(err, &mismatch) {
		return commandRejection{reason: commandRejectDaemonAccount, cause: err}
	}
	return err
}
