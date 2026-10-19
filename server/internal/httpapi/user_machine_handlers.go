package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/access"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

func userMachinePairings(service *usermachines.Service) http.HandlerFunc {
	type request struct {
		Verifier                string          `json:"verifier"`
		EnrollmentToken         string          `json:"enrollment_token"`
		Alias                   string          `json:"alias"`
		Description             string          `json:"description"`
		Platform                string          `json:"platform"`
		Architecture            string          `json:"architecture"`
		WorkspaceRoot           string          `json:"workspace_root"`
		RuntimeVersions         json.RawMessage `json:"runtime_versions"`
		PublicIdentityKey       string          `json:"public_identity_key"`
		AcceptBetaPlatform      bool            `json:"accept_beta_platform,omitempty"`
		SSHUser                 string          `json:"ssh_user,omitempty"`
		SSHPort                 uint16          `json:"ssh_port,omitempty"`
		CanReuseRuntimeIdentity bool            `json:"can_reuse_runtime_identity,omitempty"`
		KeepAwake               bool            `json:"keep_awake,omitempty"`
		ReplacesMachineID       string          `json:"replaces_machine_id,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		pairing, err := service.CreatePairing(r.Context(), usermachines.PairingInput{Verifier: body.Verifier, EnrollmentToken: body.EnrollmentToken, Alias: body.Alias, Description: body.Description, Platform: body.Platform, Architecture: body.Architecture, WorkspaceRoot: body.WorkspaceRoot, RuntimeVersions: body.RuntimeVersions, PublicIdentityKey: body.PublicIdentityKey, AcceptBetaPlatform: body.AcceptBetaPlatform, SSHUser: body.SSHUser, SSHPort: body.SSHPort, CanReuseRuntimeIdentity: body.CanReuseRuntimeIdentity, KeepAwake: body.KeepAwake, ReplacesMachineID: body.ReplacesMachineID})
		if err != nil {
			if writePairingRejection(w, r, err) {
				return
			}
			switch {
			case errors.Is(err, billing.ErrWorkspaceQuota):
				writeError(w, r, http.StatusConflict, "machine_quota_exhausted", "The configured personal machine quota is exhausted.")
			case errors.Is(err, usermachines.ErrBandwidthDenied):
				writeError(w, r, http.StatusForbidden, "machine_plan_inactive", "The machine plan is inactive or its billing period has ended. Update billing and create a new install command.")
			case errors.Is(err, usermachines.ErrSeatUnavailable):
				writeError(w, r, http.StatusConflict, "machine_seat_unavailable", "No machine seat is available. Free a seat or update the plan, then create a new install command.")
			case errors.Is(err, usermachines.ErrMachineIdentityConflict):
				writeError(w, r, http.StatusConflict, "machine_identity_conflict", "This machine identity belongs to another account.")
			case errors.Is(err, usermachines.ErrProvisioningUnavailable):
				writeError(w, r, http.StatusServiceUnavailable, "machine_provisioning_unavailable", "Machine setup is temporarily unavailable. Create a new install command after service recovers.", err)
			case errors.Is(err, usermachines.ErrInvalidPairing), errors.Is(err, usermachines.ErrInvalidMachineName), errors.Is(err, usermachines.ErrEnrollmentState):
				writeError(w, r, http.StatusBadRequest, "invalid_user_machine_pairing", "Pairing details are invalid or unsupported.")
			default:
				writeError(w, r, http.StatusInternalServerError, "user_machine_pairing_failed", "Unable to register this machine. Create a new install command and retry.", err)
			}
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: pairing})
	}
}

func machineSetup(service *usermachines.Service) http.HandlerFunc {
	type request struct {
		Alias              string          `json:"alias"`
		Description        string          `json:"description"`
		Platform           string          `json:"platform"`
		Architecture       string          `json:"architecture"`
		WorkspaceRoot      string          `json:"workspace_root"`
		PublicIdentityKey  string          `json:"public_identity_key"`
		RuntimeVersions    json.RawMessage `json:"runtime_versions"`
		AcceptBetaPlatform bool            `json:"accept_beta_platform,omitempty"`
		KeepAwake          bool            `json:"keep_awake,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body request
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		machine, err := service.Setup(r.Context(), principal.User.ID, usermachines.SetupInput{
			Alias: body.Alias, Description: body.Description, Platform: body.Platform, Architecture: body.Architecture,
			WorkspaceRoot: body.WorkspaceRoot, PublicIdentityKey: body.PublicIdentityKey,
			RuntimeVersions: body.RuntimeVersions, AcceptBetaPlatform: body.AcceptBetaPlatform,
			KeepAwake: body.KeepAwake,
		})
		switch {
		case errors.Is(err, billing.ErrWorkspaceQuota):
			writeError(w, r, http.StatusConflict, "machine_quota_exhausted", "The configured personal machine quota is exhausted. Remove a personal machine or change the configured quota before retrying.")
		case errors.Is(err, usermachines.ErrInvalidSetup):
			writeError(w, r, http.StatusBadRequest, "invalid_machine_setup", "Machine setup details are invalid or unsupported.")
		case errors.Is(err, usermachines.ErrMachineIdentityConflict):
			writeError(w, r, http.StatusConflict, "machine_identity_conflict", "This machine identity is already registered to another account.")
		case errors.Is(err, usermachines.ErrMachineNameConflict):
			writeError(w, r, http.StatusConflict, "machine_name_conflict", "A machine with this alias already exists in this ownership scope. Choose another alias.")
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "machine_setup_failed", "Unable to set up this machine.", err)
		default:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: machine})
		}
	}
}

func authenticatedMachineInstallation(service *usermachines.Service) http.HandlerFunc {
	type request struct {
		Verifier                string `json:"verifier"`
		PublicIdentityKey       string `json:"public_identity_key"`
		InstallationGeneration  int64  `json:"installation_generation"`
		SSHUser                 string `json:"ssh_user,omitempty"`
		SSHPort                 uint16 `json:"ssh_port,omitempty"`
		CanReuseRuntimeIdentity bool   `json:"can_reuse_runtime_identity,omitempty"`
		KeepAwake               bool   `json:"keep_awake,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body request
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		result, err := service.PrepareAuthenticatedMachineInstallation(r.Context(), principal.User.ID, principal.Client.SessionID, r.PathValue("machine_id"), usermachines.AuthenticatedMachineInstallationInput{
			OperationID: r.Header.Get("Idempotency-Key"), Verifier: body.Verifier,
			PublicIdentityKey: body.PublicIdentityKey, InstallationGeneration: body.InstallationGeneration,
			SSHUser: body.SSHUser, SSHPort: body.SSHPort,
			CanReuseRuntimeIdentity: body.CanReuseRuntimeIdentity,
			KeepAwake:               body.KeepAwake,
		})
		switch {
		case errors.Is(err, usermachines.ErrInvalidMachineInstallation):
			writeError(w, r, http.StatusConflict, "machine_installation_invalid", "Machine installation changed before installation material could be issued. Retry setup.")
		case errors.Is(err, usermachines.ErrMachineInstallationOperationConflict):
			writeError(w, r, http.StatusConflict, "idempotency_key_conflict", "Idempotency-Key conflicts with an existing Machine installation request.")
		case errors.Is(err, usermachines.ErrProvisioningUnavailable):
			writeError(w, r, http.StatusServiceUnavailable, "machine_installation_unavailable", "Machine installation provisioning is temporarily unavailable.", err)
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "machine_installation_failed", "Unable to prepare machine installation material.", err)
		default:
			noStore(w)
			writeJSON(w, http.StatusCreated, SuccessResponse{Data: result})
		}
	}
}

func userMachineEnrollmentStart(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body struct {
			Shell string `json:"shell"`
		}
		if r.Body != nil {
			decoder := json.NewDecoder(io.LimitReader(r.Body, 1025))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body accepts only the installer shell.")
				return
			}
		}
		if body.Shell == "" {
			body.Shell = "posix"
		}
		result, err := service.StartEnrollmentWithOptions(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), usermachines.EnrollmentOptions{Shell: body.Shell})
		if err != nil {
			if errors.Is(err, usermachines.ErrIdempotencyKeyRequired) {
				writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "A valid Idempotency-Key header is required.")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "user_machine_enrollment_start_failed", "Unable to start machine enrollment.", err)
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: result})
	}
}

func userMachineEnrollmentStatus(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		result, err := service.Enrollment(r.Context(), p.User.ID, r.PathValue("enrollment_id"))
		if errors.Is(err, usermachines.ErrEnrollmentNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_enrollment_not_found", "User-machine enrollment was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "temporarily_unavailable", "User-machine enrollment status is temporarily unavailable. Retry after service recovers.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: result})
	}
}

func userMachineEnrollmentCancel(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if err := service.CancelEnrollment(r.Context(), p.User.ID, r.PathValue("enrollment_id")); errors.Is(err, usermachines.ErrEnrollmentState) {
			writeError(w, r, http.StatusConflict, "user_machine_enrollment_not_cancellable", "User-machine enrollment cannot be cancelled in its current state.")
			return
		} else if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "temporarily_unavailable", "User-machine enrollment could not be cancelled. Retry after service recovers.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]bool{"cancelled": true}})
	}
}

func userMachineEnrollmentRetry(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body struct {
			Shell string `json:"shell"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 1025))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body accepts only the installer shell.")
			return
		}
		if body.Shell == "" {
			body.Shell = "posix"
		}
		result, err := service.RetryEnrollmentWithOptions(r.Context(), p.User.ID, r.PathValue("enrollment_id"), usermachines.EnrollmentOptions{Shell: body.Shell})
		if errors.Is(err, usermachines.ErrEnrollmentState) {
			writeError(w, r, http.StatusConflict, "user_machine_enrollment_not_retryable", "User-machine enrollment cannot be retried in its current state.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "temporarily_unavailable", "User-machine enrollment could not be retried. Retry after service recovers.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: result})
	}
}

type userMachineInstallationConsumer interface {
	ConsumeInstallationForIdentityState(context.Context, string, string, bool) (json.RawMessage, error)
}

func userMachineInstallationConsume(service userMachineInstallationConsumer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Verifier          string `json:"verifier"`
			PublicIdentityKey string `json:"public_identity_key"`
			RuntimeEnrolled   bool   `json:"runtime_enrolled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		material, err := service.ConsumeInstallationForIdentityState(r.Context(), body.Verifier, body.PublicIdentityKey, body.RuntimeEnrolled)
		if err != nil {
			switch {
			case errors.Is(err, usermachines.ErrInstallationPending):
				writeError(w, r, http.StatusConflict, "user_machine_approval_pending", "UserMachine approval is pending.")
			case errors.Is(err, usermachines.ErrInstallationDenied):
				writeError(w, r, http.StatusForbidden, "user_machine_pairing_denied", "UserMachine pairing was denied.")
			case errors.Is(err, usermachines.ErrInstallationExpired):
				writeError(w, r, http.StatusGone, "user_machine_pairing_expired", "UserMachine pairing expired.")
			case errors.Is(err, usermachines.ErrInstallationUnavailable):
				writeError(w, r, http.StatusGone, "user_machine_installation_unavailable", "Installation material is unavailable or has already been used.")
			default:
				writeError(w, r, http.StatusServiceUnavailable, "temporarily_unavailable", "Machine installation authority is temporarily unavailable. Retry after service recovers.", err)
			}
			return
		}
		var data any
		if err := json.Unmarshal(material, &data); err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: data})
	}
}

func machinesList(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		query, ok := readInventoryQuery(w, r, []string{"online", "offline", "disconnected", "revoked", "pending"}, false)
		if !ok {
			return
		}
		items, total, err := service.ListInventory(personalWorkspaceContext(r.Context()), p.User.ID, usermachines.InventoryOptions{Limit: query.Limit, Offset: query.Offset, Q: query.Q, State: query.State, Owner: query.Owner})
		if err != nil {
			writeError(w, r, 500, "internal_error", "Unable to list machines.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"items": items, "pagination": inventoryPagination(query.Limit, query.Offset, total, len(items))}})
	}
}

func userMachineOverview(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		overview, err := service.Overview(r.Context(), p.User.ID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to load machine usage.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: overview})
	}
}

func transferDestinationDefault(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		switch r.Method {
		case http.MethodGet:
			machine, err := service.TransferDestinationDefault(r.Context(), p.User.ID)
			if errors.Is(err, usermachines.ErrNotFound) {
				writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": false, "machine": nil}})
				return
			}
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "transfer_destination_default_failed", "Unable to load the default transfer destination.", err)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": true, "machine": machine}})
		case http.MethodPut:
			var body struct {
				MachineID string `json:"machine_id"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.MachineID) == "" {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "A valid machine_id is required.")
				return
			}
			machine, err := service.SetTransferDestinationDefault(r.Context(), p.User.ID, body.MachineID)
			if errors.Is(err, usermachines.ErrTransferDestinationInvalid) {
				writeError(w, r, http.StatusConflict, "transfer_destination_unavailable", "The selected transfer destination is unavailable.")
				return
			}
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "transfer_destination_default_failed", "Unable to set the default transfer destination.", err)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": true, "machine": machine}})
		case http.MethodDelete:
			if err := service.ClearTransferDestinationDefault(r.Context(), p.User.ID); err != nil {
				writeError(w, r, http.StatusInternalServerError, "transfer_destination_default_failed", "Unable to clear the default transfer destination.", err)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": false, "machine": nil}})
		}
	}
}

func terminalSessionTransferDestination(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		sessionID := r.PathValue("session_id")
		switch r.Method {
		case http.MethodGet:
			machine, err := service.TerminalSessionTransferDestination(r.Context(), p.User.ID, sessionID)
			if errors.Is(err, usermachines.ErrNotFound) {
				writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": false, "machine": nil}})
				return
			}
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "transfer_destination_session_failed", "Unable to load the session transfer destination.", err)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": true, "machine": machine}})
		case http.MethodPut:
			var body struct {
				MachineID string `json:"machine_id"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.MachineID) == "" {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "A valid machine_id is required.")
				return
			}
			machine, err := service.SetTerminalSessionTransferDestination(r.Context(), p.User.ID, sessionID, body.MachineID)
			if errors.Is(err, usermachines.ErrTransferDestinationInvalid) {
				writeError(w, r, http.StatusConflict, "transfer_destination_unavailable", "The session or selected transfer destination is unavailable.")
				return
			}
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "transfer_destination_session_failed", "Unable to set the session transfer destination.", err)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": true, "machine": machine}})
		case http.MethodDelete:
			if err := service.ClearTerminalSessionTransferDestination(r.Context(), p.User.ID, sessionID); errors.Is(err, usermachines.ErrNotFound) {
				writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
				return
			} else if err != nil {
				writeError(w, r, http.StatusInternalServerError, "transfer_destination_session_failed", "Unable to clear the session transfer destination.", err)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"configured": false, "machine": nil}})
		}
	}
}

func terminalSessionTransferDestinations(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		sessionID := r.PathValue("session_id")
		var items []usermachines.UserMachine
		var err error
		items, err = service.EligibleTerminalSessionTransferDestinations(r.Context(), p.User.ID, p.Client.SessionID, sessionID)
		if errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
			writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
			return
		}
		if errors.Is(err, access.ErrTunnelUnavailable) || errors.Is(err, access.ErrTerminalRuntimeUnavailable) {
			writeError(w, r, http.StatusConflict, "machine_offline", "Session host is offline.")
			return
		}
		if errors.Is(err, usermachines.ErrProvisioningUnavailable) {
			writeError(w, r, http.StatusConflict, "machine_offline", "Session host is offline.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "transfer_destinations_unavailable", "Eligible transfer destinations are unavailable.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": items}})
	}
}

func userMachineGet(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		machine, err := service.Get(r.Context(), p.User.ID, r.PathValue("machine_id"))
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
			writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: machine})
	}
}

func userMachineRename(service *usermachines.Service) http.HandlerFunc {
	type request struct {
		Alias       string `json:"alias"`
		Description string `json:"description"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body request
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		machine, err := service.Rename(r.Context(), p.User.ID, r.PathValue("machine_id"), body.Alias, body.Description)
		switch {
		case errors.Is(err, usermachines.ErrInvalidMachineName):
			writeError(w, r, http.StatusBadRequest, "invalid_machine_name", "Use a valid lowercase machine alias of 1–63 characters and a description of at most 160 characters without control characters.")
		case errors.Is(err, usermachines.ErrMachineNameConflict):
			writeError(w, r, http.StatusConflict, "machine_name_conflict", "A machine with this alias already exists in this ownership scope. Choose another alias.")
		case errors.Is(err, usermachines.ErrNotFound):
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "machine_rename_failed", "Unable to rename this machine.", err)
		default:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: machine})
		}
	}
}

func userMachineConnectionDescriptor(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body struct {
			TerminalSessionID string `json:"terminal_session_id"`
			SourceMachineID   string `json:"source_machine_id"`
			CreateSession     *struct {
				Name           string  `json:"name"`
				IdempotencyKey string  `json:"idempotency_key"`
				CWD            *string `json:"cwd,omitempty"`
			} `json:"create_session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		terminalSessionID := body.TerminalSessionID
		var created *usermachines.TerminalSession
		if body.CreateSession != nil {
			// create-and-connect collapses session creation and descriptor
			// issuance into one round trip. The idempotency key makes retried
			// requests resolve the same durable session.
			if terminalSessionID != "" || strings.TrimSpace(body.CreateSession.IdempotencyKey) == "" {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must not mix terminal_session_id with create_session.")
				return
			}
			cwd := ""
			if body.CreateSession.CWD != nil {
				cwd = *body.CreateSession.CWD
				if cwd == "" {
					userMachineTerminalSessionError(w, r, usermachines.ErrTerminalSessionInvalidCWD)
					return
				}
			}
			item, err := service.CreateConfiguredTerminalSessionInDirectory(r.Context(), p.User.ID, body.SourceMachineID, r.PathValue("machine_id"), body.CreateSession.Name, body.CreateSession.IdempotencyKey, cwd)
			if userMachineTerminalSessionError(w, r, err) {
				return
			}
			terminalSessionID = item.ID
			created = &item
		}
		response, err := service.ConnectTerminalSession(r.Context(), p.User.ID, body.SourceMachineID, r.PathValue("machine_id"), p.Client.SessionID, terminalSessionID)
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
			writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "connector_unavailable", "Machine credentials are unavailable.", err)
			return
		}
		if created != nil {
			writeJSON(w, http.StatusCreated, SuccessResponse{Data: map[string]any{"descriptor": response, "terminal_session": created}})
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: response})
	}
}

func sharedTerminalConnectionDescriptor(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body struct {
			SourceMachineID string `json:"source_machine_id"`
		}
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		response, err := service.ConnectSharedTerminalSession(r.Context(), p.User.ID, body.SourceMachineID, p.Client.SessionID, r.PathValue("session_id"))
		if errors.Is(err, usermachines.ErrAmbiguousTeamAccess) {
			writeError(w, r, http.StatusConflict, "ambiguous_team_access", "More than one team grants access to this resource. Use a resource with one team grant until team selection is available.")
			return
		}
		switch {
		case errors.Is(err, usermachines.ErrTerminalSessionNotFound), errors.Is(err, usermachines.ErrNotFound):
			writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
		case errors.Is(err, usermachines.ErrMachineCapabilityUnavailable):
			writeError(w, r, http.StatusConflict, "terminal_host_unavailable", "The terminal host is unavailable.")
		case err != nil:
			writeError(w, r, http.StatusServiceUnavailable, "connector_unavailable", "Terminal credentials are unavailable.", err)
		default:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: response})
		}
	}
}

func userMachineExecDescriptor(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body struct {
			SourceMachineID string `json:"source_machine_id"`
			OperationID     string `json:"operation_id"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must contain a valid source machine and operation ID.")
			return
		}
		response, err := service.ExecDescriptor(r.Context(), p.User.ID, body.SourceMachineID, r.PathValue("machine_id"), p.Client.SessionID, body.OperationID)
		if errors.Is(err, usermachines.ErrAmbiguousTeamAccess) {
			writeError(w, r, http.StatusConflict, "ambiguous_team_access", "More than one team grants access to this resource. Use a resource with one team grant until team selection is available.")
			return
		}
		if errors.Is(err, usermachines.ErrExecOperationInvalid) {
			writeError(w, r, http.StatusBadRequest, "invalid_operation_id", "Operation ID is invalid.")
			return
		}
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if errors.Is(err, usermachines.ErrMachineCapabilityUnavailable) {
			writeError(w, r, http.StatusConflict, "machine_not_ready", "Machine is not ready for execution.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "credential_unavailable", "Execution credentials are unavailable.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: response})
	}
}

func userMachineSSHDescriptor(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body struct {
			SourceMachineID string `json:"source_machine_id"`
			OperationID     string `json:"operation_id"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must contain a valid source machine and operation ID.")
			return
		}
		response, err := service.SSHDescriptor(r.Context(), p.User.ID, body.SourceMachineID, r.PathValue("machine_id"), p.Client.SessionID, body.OperationID)
		if errors.Is(err, usermachines.ErrAmbiguousTeamAccess) {
			writeError(w, r, http.StatusConflict, "ambiguous_team_access", "More than one team grants access to this resource. Use a resource with one team grant until team selection is available.")
			return
		}
		if errors.Is(err, usermachines.ErrSSHOperationInvalid) {
			writeError(w, r, http.StatusBadRequest, "invalid_operation_id", "Operation ID is invalid.")
			return
		}
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if errors.Is(err, usermachines.ErrMachineCapabilityUnavailable) {
			writeError(w, r, http.StatusConflict, "machine_not_ready", "Machine is not ready for SSH.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "credential_unavailable", "SSH credentials are unavailable.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: response})
	}
}

func userMachineFileTransferDescriptor(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body struct {
			SourceMachineID string `json:"source_machine_id"`
			SessionID       string `json:"session_id"`
			RequestID       string `json:"request_id"`
			ManifestDigest  string `json:"manifest_digest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		response, err := service.FileTransferDescriptorForRequest(r.Context(), p.User.ID, body.SourceMachineID, r.PathValue("machine_id"), p.Client.SessionID, body.SessionID, body.RequestID, body.ManifestDigest)
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
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "file_transfer_unavailable", "File transfer credentials are unavailable.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: response})
	}
}

func userMachineConnectionReadiness(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		response, err := service.ConnectionReadinessForTerminalSession(r.Context(), p.User.ID, r.PathValue("machine_id"), r.URL.Query().Get("terminal_session_id"))
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
			writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "connector_unavailable", "Machine status is unavailable.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: response})
	}
}

func userMachineTerminalSessionsList(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		query, valid := readInventoryQuery(w, r, []string{"running", "closed", "unavailable"}, true)
		if !valid {
			return
		}
		items, total, err := service.ListTerminalSessionInventory(personalWorkspaceContext(r.Context()), p.User.ID, r.PathValue("machine_id"), usermachines.InventoryOptions{Limit: query.Limit, Offset: query.Offset, Q: query.Q, State: query.State, Owner: query.Owner, Closed: query.Closed})
		if userMachineTerminalSessionError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": items, "pagination": inventoryPagination(query.Limit, query.Offset, total, len(items))}})
	}
}

func userMachineTerminalSessionsCreate(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		item, err := service.CreateConfiguredTerminalSession(r.Context(), p.User.ID, r.PathValue("machine_id"), body.Name, r.Header.Get("Idempotency-Key"))
		if userMachineTerminalSessionError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: item})
	}
}

func userMachineTerminalSessionsRename(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		item, err := service.RenameTerminalSession(r.Context(), p.User.ID, r.PathValue("machine_id"), r.PathValue("session_id"), body.Name)
		if userMachineTerminalSessionError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: item})
	}
}

func userMachineTerminalSessionsClose(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		applied, err := service.CloseTerminalSession(r.Context(), p.User.ID, r.PathValue("machine_id"), r.PathValue("session_id"))
		if userMachineTerminalSessionError(w, r, err) {
			return
		}
		if !applied {
			writeJSON(w, http.StatusAccepted, SuccessResponse{Data: map[string]string{"operation_state": "pending"}})
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]string{"operation_state": "applied"}})
	}
}

func userMachineTerminalSessionsDelete(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		applied, err := service.DeleteTerminalSession(r.Context(), p.User.ID, r.PathValue("machine_id"), r.PathValue("session_id"))
		if userMachineTerminalSessionError(w, r, err) {
			return
		}
		if !applied {
			writeJSON(w, http.StatusAccepted, SuccessResponse{Data: map[string]string{"purge_state": "pending"}})
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]string{"purge_state": "purged"}})
	}
}

func userMachineTerminalSessionError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, usermachines.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
	case errors.Is(err, usermachines.ErrTerminalSessionNotFound):
		writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
	case errors.Is(err, usermachines.ErrTerminalSessionReserved):
		writeError(w, r, http.StatusConflict, "terminal_session_reserved", "The default terminal session cannot be changed this way.")
	case errors.Is(err, usermachines.ErrTerminalSessionNotClosed):
		writeError(w, r, http.StatusConflict, "terminal_session_not_closed", "Close the terminal and wait for the host to confirm it is closed before deleting its history.")
	case errors.Is(err, usermachines.ErrTerminalSessionLimit):
		var capacity *usermachines.TerminalSessionCapacityError
		if errors.As(err, &capacity) {
			writeError(w, r, http.StatusConflict, "terminal_session_limit_reached", capacity.Error())
		} else {
			writeError(w, r, http.StatusConflict, "terminal_session_limit_reached", "This machine has reached its terminal session limit.")
		}
	case errors.Is(err, usermachines.ErrTerminalSessionConflict):
		writeError(w, r, http.StatusConflict, "terminal_session_name_conflict", "A terminal session already uses that name.")
	case errors.Is(err, usermachines.ErrTerminalSessionInvalidCWD):
		writeError(w, r, http.StatusBadRequest, "invalid_terminal_session_cwd", "Terminal launch directory must be an absolute directory path for this machine, at most 1024 bytes. No terminal was created.")
	case errors.Is(err, usermachines.ErrTerminalSessionInvalidName):
		writeError(w, r, http.StatusBadRequest, "invalid_terminal_session_name", "Terminal session name is invalid.")
	case errors.Is(err, usermachines.ErrTerminalSessionIdempotency):
		writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key is required.")
	case errors.Is(err, usermachines.ErrMachineCapabilityUnavailable):
		writeError(w, r, http.StatusConflict, "machine_capability_unavailable", "This machine is not configured to host terminals.")
	case errors.Is(err, usermachines.ErrMachineOffline):
		writeError(w, r, http.StatusConflict, "machine_offline", "This terminal host is offline.")
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
	}
	return true
}

func userMachineDisconnect(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if err := service.Disconnect(r.Context(), p.User.ID, r.PathValue("machine_id")); errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		} else if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "temporarily_unavailable", "Machine could not be disconnected. Retry after service recovers.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]bool{"disconnected": true}})
	}
}

func userMachineDelete(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if err := service.Delete(r.Context(), p.User.ID, r.PathValue("machine_id")); err != nil {
			if errors.Is(err, usermachines.ErrNotFound) {
				writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "user_machine_delete_failed", "Unable to delete this machine.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]bool{"deleted": true}})
	}
}
