package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat/server/internal/lazyaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/metering"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

type runtimeObservationRepository interface {
	RecordRuntimeObservation(context.Context, metering.RuntimeObservation) error
}

type terminalJoinRepository interface {
	RecordTerminalJoin(context.Context, string, string, usermachines.TerminalJoinObservation) error
}

type runtimeIdentityVerifier interface {
	VerifyRuntimeObservation(context.Context, string, []byte, []byte, string, string) error
}
type availabilityObservationRepository interface {
	RecordAvailabilityObservation(context.Context, string, string, usermachines.AvailabilityObservation) error
}

type updateObservationRepository interface {
	RecordUpdateObservation(context.Context, string, string, usermachines.UpdateObservation) error
}

type machineCapabilitiesRepository interface {
	MachineCapabilitiesEnabled() bool
	ResolveMachineCapabilities(context.Context, string, string) (usermachines.MachineCapabilityPolicy, error)
	RecordMachineCapabilitiesObservation(context.Context, string, string, usermachines.MachineCapabilitiesObservation) error
}

type machineServicesRepository interface {
	RecordMachineServices(context.Context, string, string, wire.Snapshot, time.Time) error
	RuntimePolicy(context.Context, string, string) (wire.RuntimePolicy, error)
}

type runtimeAuxiliaryRejection struct {
	Observation string `json:"observation"`
	Code        string `json:"code"`
}

func runtimeObservation(repo runtimeObservationRepository, identities runtimeIdentityVerifier, _ int, observationSinks ...any) http.HandlerFunc {
	var transferPolicy interface {
		RuntimeFileTransferPolicy() accessdescriptor.FileTransferPolicy
	}
	var lazyRuntime interface {
		RecordLazyRuntimeObservation(context.Context, string, string, lazyaccess.RuntimeObservation) error
	}
	var terminalJoins terminalJoinRepository
	var availability availabilityObservationRepository
	var updates updateObservationRepository
	var machineCapabilities machineCapabilitiesRepository
	var machineServices machineServicesRepository
	for _, sink := range observationSinks {
		// Optional router dependencies arrive through interfaces; typed nils
		// must remain absent rather than exposing methods on a nil receiver.
		switch candidate := sink.(type) {
		case *usermachines.Service:
			if candidate == nil {
				continue
			}
		case *machineservices.Service:
			if candidate == nil {
				continue
			}
		case *lazyaccess.Activator:
			if candidate == nil {
				continue
			}
		}
		if candidate, ok := sink.(interface {
			RecordLazyRuntimeObservation(context.Context, string, string, lazyaccess.RuntimeObservation) error
		}); ok {
			lazyRuntime = candidate
		}
		if candidate, ok := sink.(interface {
			RuntimeFileTransferPolicy() accessdescriptor.FileTransferPolicy
		}); ok {
			transferPolicy = candidate
		}
		if candidate, ok := sink.(machineServicesRepository); ok {
			machineServices = candidate
		}
		if candidate, ok := sink.(terminalJoinRepository); ok {
			terminalJoins = candidate
		}
		if candidate, ok := sink.(availabilityObservationRepository); ok {
			availability = candidate
		}
		if candidate, ok := sink.(updateObservationRepository); ok {
			updates = candidate
		}
		if candidate, ok := sink.(machineCapabilitiesRepository); ok {
			if candidate.MachineCapabilitiesEnabled() {
				machineCapabilities = candidate
			}
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		var req struct {
			MachineServices     *wire.Snapshot                               `json:"machine_services,omitempty"`
			TerminalJoin        *usermachines.TerminalJoinObservation        `json:"terminal_join,omitempty"`
			LazyRuntime         *lazyaccess.RuntimeObservation               `json:"lazy_runtime,omitempty"`
			EnvironmentID       string                                       `json:"environment_id"`
			ResourceID          string                                       `json:"resource_id"`
			ReporterVersion     string                                       `json:"reporter_version"`
			SampledAt           time.Time                                    `json:"sampled_at"`
			Availability        *usermachines.AvailabilityObservation        `json:"availability"`
			MachineCapabilities *usermachines.MachineCapabilitiesObservation `json:"machine_capabilities"`
			RuntimeDiagnostics  *struct {
				Capabilities        []string  `json:"capabilities"`
				WorkerGeneration    uint64    `json:"worker_generation"`
				OSBootID            string    `json:"os_boot_id"`
				WorkerServiceScope  string    `json:"worker_service_scope"`
				ConnectorState      string    `json:"connector_state"`
				ConnectorGeneration uint64    `json:"connector_generation"`
				ObservedAt          time.Time `json:"observed_at"`
			} `json:"runtime_diagnostics"`
			Update *usermachines.UpdateObservation `json:"update,omitempty"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err != nil || len(body) > 1<<20 || decoder.Decode(&req) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Heartbeat payload is invalid JSON.")
			return
		}
		if req.EnvironmentID == "" || req.ResourceID == "" || req.SampledAt.IsZero() {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Runtime observation is missing required fields.")
			return
		}
		if req.RuntimeDiagnostics != nil && (req.RuntimeDiagnostics.WorkerGeneration < 1 || req.RuntimeDiagnostics.OSBootID == "" || len(req.RuntimeDiagnostics.OSBootID) > 256 || strings.ContainsAny(req.RuntimeDiagnostics.OSBootID, "\x00\r\n") || !validObservedCapabilities(req.RuntimeDiagnostics.Capabilities) || !slices.Contains([]string{"unknown", "user", "system"}, req.RuntimeDiagnostics.WorkerServiceScope) || !slices.Contains([]string{"ready", "degraded", "unavailable"}, req.RuntimeDiagnostics.ConnectorState) || req.RuntimeDiagnostics.ObservedAt.IsZero()) {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Runtime diagnostics are invalid.")
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if identities == nil || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Current signed host identity is required.")
			return
		}
		proof, proofErr := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		authErr := proofErr
		if proofErr == nil {
			authErr = identities.VerifyRuntimeObservation(r.Context(), got, proof, body, req.EnvironmentID, req.ResourceID)
		}
		if authErr != nil {
			if proofErr != nil {
				writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Runtime observation credential is invalid.")
				return
			}
			writeMachineProofFailure(w, r, authErr, "unauthenticated", "Runtime observation credential is invalid.")
			return
		}
		if req.TerminalJoin != nil {
			if identities == nil || r.Header.Get("X-Paperboat-Machine-Proof") == "" || terminalJoins == nil {
				writeError(w, r, http.StatusForbidden, "terminal_join_denied", "Current signed host identity is required to record attachment.")
				return
			}
			if req.MachineServices != nil || req.LazyRuntime != nil || req.Availability != nil || req.MachineCapabilities != nil || req.RuntimeDiagnostics != nil || req.Update != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Terminal join must be reported separately from presence.")
				return
			}
			err := terminalJoins.RecordTerminalJoin(r.Context(), req.EnvironmentID, req.ResourceID, *req.TerminalJoin)
			switch {
			case errors.Is(err, usermachines.ErrTerminalJoinInvalid):
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Terminal join identifiers are invalid.")
			case errors.Is(err, usermachines.ErrTerminalSessionNotFound):
				writeError(w, r, http.StatusForbidden, "terminal_join_denied", "Terminal access is unavailable; reconnect with current access.")
			case err != nil:
				writeError(w, r, http.StatusServiceUnavailable, "terminal_join_unavailable", "Attachment could not be recorded; retry attachment.", err)
			default:
				writeJSON(w, http.StatusAccepted, SuccessResponse{Data: map[string]bool{"recorded": true}})
			}
			return
		}
		if req.LazyRuntime != nil {
			if lazyRuntime == nil || identities == nil || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
				writeError(w, r, 403, "lazy_registration_denied", "Signed current machine identity is required for port sharing.")
				return
			}
			if err := lazyRuntime.RecordLazyRuntimeObservation(r.Context(), req.EnvironmentID, req.ResourceID, *req.LazyRuntime); err != nil {
				lazyPolicyError(w, r, err)
				return
			}
		}
		if req.MachineServices != nil {
			if machineServices == nil {
				writeError(w, r, http.StatusServiceUnavailable, "machine_services_unavailable", "Machine service observation storage is unavailable.")
				return
			}
			if err := machineServices.RecordMachineServices(r.Context(), req.EnvironmentID, req.ResourceID, *req.MachineServices, req.SampledAt); err != nil {
				switch {
				case expectedHTTPErrorOnly(err, wire.ErrInvalid):
					writeError(w, r, 400, "invalid_request", "Machine service snapshot is invalid.")
				case expectedHTTPErrorOnly(err, machineservices.ErrDenied):
					writeError(w, r, 403, "machine_services_denied", "Machine service policy changed; refresh policy before announcing ports.")
				case expectedHTTPErrorOnly(err, machineservices.ErrConflict):
					writeError(w, r, 409, "machine_services_conflict", "Machine service generation is stale; restart with the current installation identity.")
				default:
					writeError(w, r, 503, "machine_services_unavailable", "Machine service observation could not be stored; retry.", err)
				}
				return
			}
		}
		observation := metering.RuntimeObservation{
			EnvironmentID:   req.EnvironmentID,
			MachineID:       req.ResourceID,
			ObservedAt:      req.SampledAt.UTC(),
			ReporterVersion: req.ReporterVersion,
		}
		if req.RuntimeDiagnostics != nil {
			observation.WorkerGeneration = req.RuntimeDiagnostics.WorkerGeneration
			observation.OSBootID = req.RuntimeDiagnostics.OSBootID
			observation.WorkerServiceScope = req.RuntimeDiagnostics.WorkerServiceScope
			observation.ConnectorState = req.RuntimeDiagnostics.ConnectorState
			observation.ConnectorGeneration = req.RuntimeDiagnostics.ConnectorGeneration
			observation.Capabilities = append([]string(nil), req.RuntimeDiagnostics.Capabilities...)
			observation.DiagnosticsObservedAt = req.RuntimeDiagnostics.ObservedAt.UTC()
		}
		if err := repo.RecordRuntimeObservation(r.Context(), observation); err != nil {
			if expectedHTTPErrorOnly(err, metering.ErrDuplicateMachineIdentity) {
				writeError(w, r, http.StatusConflict, "duplicate_machine_identity", "This machine identity is active on another installation. Run pb setup to create a distinct machine identity.")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		// Runtime presence is durable at this point. Optional observation storage
		// can be temporarily unavailable without making the heartbeat retry.
		auxiliaryRejections := make([]runtimeAuxiliaryRejection, 0, 3)
		if req.Availability != nil {
			if availability == nil {
				auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "availability", Code: "availability_observation_unavailable"})
			} else {
				normalized, _, validOrder := normalizeStatusTimestamps(req.Availability.ObservedAt, req.SampledAt, time.Now().UTC())
				if !validOrder {
					auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "availability", Code: "availability_observation_invalid"})
				} else {
					req.Availability.ObservedAt = normalized
					if err := availability.RecordAvailabilityObservation(r.Context(), req.EnvironmentID, req.ResourceID, *req.Availability); err != nil {
						switch {
						case errors.Is(err, usermachines.ErrAvailabilityInvalid):
							auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "availability", Code: "availability_observation_invalid"})
						case errors.Is(err, usermachines.ErrAvailabilityObservationStale):
							auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "availability", Code: "availability_observation_stale"})
						default:
							auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "availability", Code: "availability_observation_unavailable"})
							captureRuntimeObservationFailure(r, "availability_observation_unavailable", err)
						}
					}
				}
			}
		}
		if req.MachineCapabilities != nil {
			if machineCapabilities == nil {
				auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "machine_capabilities", Code: "capabilities_observation_unavailable"})
			} else if err := machineCapabilities.RecordMachineCapabilitiesObservation(r.Context(), req.ResourceID, req.EnvironmentID, *req.MachineCapabilities); err != nil {
				code := "capabilities_observation_unavailable"
				if errors.Is(err, usermachines.ErrCapabilitiesInvalid) {
					code = "capabilities_observation_invalid"
				} else if errors.Is(err, usermachines.ErrCapabilitiesObservationStale) {
					code = "capabilities_observation_stale"
				} else {
					captureRuntimeObservationFailure(r, "capabilities_observation_unavailable", err)
				}
				auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "machine_capabilities", Code: code})
			}
		}
		updateRecorded := false
		if req.Update != nil {
			if req.Update.Validate(time.Now().UTC()) != nil {
				auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "update", Code: "update_observation_invalid"})
			} else if updates == nil {
				auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "update", Code: "update_observation_unavailable"})
			} else if err := updates.RecordUpdateObservation(r.Context(), req.EnvironmentID, req.ResourceID, *req.Update); err != nil {
				code := "update_observation_unavailable"
				captureFailure := true
				switch {
				case errors.Is(err, usermachines.ErrUpdateObservationStale):
					code = "update_observation_stale"
					captureFailure = false
				case errors.Is(err, usermachines.ErrUpdateObservationInvalid):
					code = "update_observation_invalid"
					captureFailure = false
				case errors.Is(err, usermachines.ErrUpdateObservationConflict):
					code = "update_observation_conflict"
					captureFailure = false
				case errors.Is(err, usermachines.ErrNotFound):
					captureFailure = false
					writeError(w, r, http.StatusNotFound, "machine_not_found", "Machine was not found.")
					return
				}
				if captureFailure {
					captureRuntimeObservationFailure(r, "update_observation_unavailable", err)
				}
				auxiliaryRejections = append(auxiliaryRejections, runtimeAuxiliaryRejection{Observation: "update", Code: code})
			} else {
				updateRecorded = true
			}
		}
		response := map[string]any{"accepted": true, "update_observation_recorded": updateRecorded}
		if machineServices != nil {
			policy, err := machineServices.RuntimePolicy(r.Context(), req.EnvironmentID, req.ResourceID)
			if err != nil {
				writeError(w, r, 503, "machine_services_unavailable", "Machine service policy is unavailable; retry.", err)
				return
			}
			response["machine_services_policy"] = policy
		}
		if transferPolicy != nil {
			response["file_transfer_policy"] = transferPolicy.RuntimeFileTransferPolicy()
		}
		if machineCapabilities != nil {
			policy, policyErr := machineCapabilities.ResolveMachineCapabilities(r.Context(), req.ResourceID, req.EnvironmentID)
			if policyErr != nil {
				writeError(w, r, http.StatusServiceUnavailable, "capabilities_unavailable", "Machine capability policy is temporarily unavailable.", policyErr)
				return
			}
			response["machine_capabilities"] = policy
		}
		if len(auxiliaryRejections) > 0 {
			response["auxiliary_rejections"] = auxiliaryRejections
		}
		writeJSON(w, http.StatusAccepted, SuccessResponse{Data: response})
	}
}

func captureRuntimeObservationFailure(r *http.Request, code string, err error) {
	if r == nil || err == nil {
		return
	}
	ctx := supportreport.WithFailureIdentity(r.Context(), "", requestIDFromContext(r.Context()), supportReference(r.Context()))
	supportreport.CaptureFromContext(ctx, "paperboat-server", supportOperation(r.URL.Path), "service_error", code, err)
}

func validObservedCapabilities(capabilities []string) bool {
	allowed := []string{"file_receive", "preview_launch", "terminal_host", "session_host", "ssh_host", "keep_awake", "environment_injection"}
	if len(capabilities) > len(allowed) {
		return false
	}
	for index, capability := range capabilities {
		if !slices.Contains(allowed, capability) || slices.Contains(capabilities[:index], capability) {
			return false
		}
	}
	return true
}

func normalizeStatusTimestamps(statusUpdated, sampledAt, serverNow time.Time) (time.Time, time.Time, bool) {
	statusUpdated = statusUpdated.UTC()
	sampledAt = sampledAt.UTC()
	serverNow = serverNow.UTC()
	if statusUpdated.After(sampledAt) {
		if sampledAt.After(serverNow) {
			return serverNow, serverNow, false
		}
		return sampledAt, serverNow, false
	}
	observed := serverNow.Add(-sampledAt.Sub(statusUpdated))
	if sampledAt.After(serverNow) {
		return observed, observed, true
	}
	return statusUpdated, observed, true
}
