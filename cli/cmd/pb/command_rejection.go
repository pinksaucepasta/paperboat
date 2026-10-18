package main

import (
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
)

// commandRejection describes a command-owned refusal. Its
// finite reason controls presentation; an optional original cause still
// participates in command classification and can outrank the refusal.
type commandRejection struct {
	reason commandRejectionReason
	cause  error
}

func (e commandRejection) valid() bool {
	return e.reason >= commandRejectDefaultSessionRename && e.reason <= commandRejectPreviewDomainTimeout
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
		return "Custom builds cannot use official updates. Install a fresh official build to switch to official updates."
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
