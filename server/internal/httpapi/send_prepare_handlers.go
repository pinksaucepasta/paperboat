package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/access"
	"github.com/pinksaucepasta/paperboat/server/internal/accountprefs"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// sendPrepareRequest is the single-call `pb send` setup. Field for field it
// carries the inputs of GET /v1/me/preferences (transfer_mode, empty for the account preference),
// POST /v1/team-inbox/requests (the inbox fields) and
// POST /v1/machines/{id}/file-transfer-descriptor (session_id,
// declared_bytes). Unknown fields are refused like the neighbours.
type sendPrepareRequest struct {
	DestinationID   string           `json:"destination_machine_id"`
	SourceMachineID string           `json:"source_machine_id"`
	SessionID       string           `json:"session_id"`
	RequestID       string           `json:"request_id"`
	OperationID     string           `json:"operation_id"`
	BatchID         string           `json:"batch_id"`
	Files           []teaminbox.File `json:"files"`
	ExpiresAt       time.Time        `json:"expires_at"`
	TransferMode    string           `json:"transfer_mode"`
	DeclaredBytes   int64            `json:"declared_bytes"`
}

// sendPrepareResponse folds the resolve, inbox and descriptor answers. The
// descriptor is null for pending and stored requests.
type sendPrepareResponse struct {
	Request          teaminbox.Request                    `json:"request"`
	FileTransferMode string                               `json:"file_transfer_mode"`
	Descriptor       *usermachines.FileTransferDescriptor `json:"descriptor"`
}

// sendPrepare resolves destination_ref, defaults the transfer mode, creates
// the Team Inbox request and, for an approved or no-approval request, mints
// the file-transfer descriptor inline. One HTTP auth, one round trip; the
// database work is the same sequence the four separate calls run.
func sendPrepare(machines *usermachines.Service, inbox *teaminbox.Service, prefs *accountprefs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		limitEnvironmentJSON(w, r, teaminbox.MaximumManifestBytes)
		var in sendPrepareRequest
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		destinationID := strings.TrimSpace(in.DestinationID)
		if destinationID == "" {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "destination_machine_id is required.")
			return
		}
		mode := strings.TrimSpace(in.TransferMode)
		if mode == "" {
			preferences, err := prefs.Get(r.Context(), p.User.ID)
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "preferences_unavailable", "Your preferences could not be read. Retry.", err)
				return
			}
			mode = preferences.FileTransferMode
		}
		created, err := inbox.Create(r.Context(), teaminbox.RequestInput{
			RequestID: in.RequestID, OperationID: in.OperationID, SenderAccount: p.User.ID,
			SourceMachineID: in.SourceMachineID, DestinationMachineID: destinationID,
			BatchID: in.BatchID, Files: in.Files, ExpiresAt: in.ExpiresAt,
		})
		if err != nil {
			writePrepareInboxError(w, r, err)
			return
		}
		out := sendPrepareResponse{Request: created, FileTransferMode: mode}
		if created.Stored || created.Status == "pending" {
			// Stored sends keep their existing upload branch; pending sends poll
			// the existing request route, then mint the existing descriptor
			// route once approved. Neither carries a descriptor.
			status := http.StatusCreated
			if created.Status == "pending" {
				status = http.StatusAccepted
			}
			writeJSON(w, status, SuccessResponse{Data: out})
			return
		}
		requestID, manifestDigest := created.RequestID, created.ManifestDigest
		if created.Status == "not_required" {
			// Own machines skip request binding exactly like the descriptor
			// route called without request_id and manifest_digest.
			requestID, manifestDigest = "", ""
		}
		descriptor, err := machines.PrepareFileTransferDescriptor(r.Context(), p.User.ID, in.SourceMachineID, destinationID, p.Client.SessionID, in.SessionID, requestID, manifestDigest, usermachines.TransferIntent{Mode: mode, DeclaredBytes: in.DeclaredBytes})
		if err != nil {
			writePrepareDescriptorError(w, r, err)
			return
		}
		out.Descriptor = &descriptor
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: out})
	}
}

func writePrepareInboxError(w http.ResponseWriter, r *http.Request, err error) {
	if writeSDKLimitError(w, r, err) {
		return
	}
	if errors.Is(err, teaminbox.ErrOffline) {
		writeError(w, r, http.StatusConflict, "machine_offline", "Destination machine is offline.")
		return
	}
	teamInboxError(w, r, err)
}

// writePrepareDescriptorError is the file-transfer-descriptor route's mapping,
// unchanged: the inline issue authorizes and fails exactly like the separate
// descriptor call.
func writePrepareDescriptorError(w http.ResponseWriter, r *http.Request, err error) {
	if writeSDKLimitError(w, r, err) {
		return
	}
	if errors.Is(err, auth.ErrOutsideTokenScope) {
		writeError(w, r, http.StatusForbidden, "outside_token_scope", "The API token does not allow this session, workspace or file transfer.")
		return
	}
	if errors.Is(err, auth.ErrOutsideReach) {
		writeError(w, r, http.StatusForbidden, "outside_recipe_reach", "This machine's recipe does not reach that machine for file transfer. Add it to [[reach]] and run pb recipe update.")
		return
	}
	var tooLarge *usermachines.TransferTooLargeError
	if errors.As(err, &tooLarge) {
		writeErrorDetails(w, r, http.StatusConflict, "transfer_too_large", fmt.Sprintf("This transfer is %d bytes. Automatic sends up to %d bytes. Use --unlimited to send directly or through your own relay.", tooLarge.TotalBytes, tooLarge.MaxBytes), map[string]any{"total_bytes": tooLarge.TotalBytes, "max_bytes": tooLarge.MaxBytes})
		return
	}
	if errors.Is(err, usermachines.ErrInvalidTransferIntent) {
		writeError(w, r, http.StatusBadRequest, "invalid_transfer_intent", "transfer_mode must be automatic or unlimited, and declared_bytes must be the prepared batch's total (the accepted inbox request's total for team sends).")
		return
	}
	if errors.Is(err, usermachines.ErrAmbiguousTeamAccess) {
		writeError(w, r, http.StatusConflict, "ambiguous_team_access", "More than one team grants access to this resource. Use a resource with one team grant until team selection is available.")
		return
	}
	if errors.Is(err, usermachines.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Source or destination machine was not found.")
		return
	}
	if errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
		writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
		return
	}
	if errors.Is(err, usermachines.ErrMachineCapabilityUnavailable) {
		writeError(w, r, http.StatusConflict, "machine_capability_unavailable", "Destination machine cannot receive files.")
		return
	}
	if errors.Is(err, usermachines.ErrMachineOffline) {
		writeError(w, r, http.StatusConflict, "machine_offline", "Destination machine is offline.")
		return
	}
	if errors.Is(err, teaminbox.ErrPending) || errors.Is(err, teaminbox.ErrDeclined) || errors.Is(err, teaminbox.ErrExpired) || errors.Is(err, teaminbox.ErrRevoked) || errors.Is(err, teaminbox.ErrConflict) {
		teamInboxError(w, r, err)
		return
	}
	if errors.Is(err, access.ErrTerminalSessionNotFound) {
		writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
		return
	}
	if errors.Is(err, access.ErrTunnelUnavailable) || errors.Is(err, access.ErrTerminalRuntimeUnavailable) {
		writeError(w, r, http.StatusConflict, "machine_offline", "Session host is offline.")
		return
	}
	if errors.Is(err, usermachines.ErrProvisioningUnavailable) {
		writeError(w, r, http.StatusConflict, "machine_offline", "Destination machine is offline.")
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, "file_transfer_unavailable", "File transfer credentials are unavailable.", err)
}
