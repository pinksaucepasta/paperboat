package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

const machineEventsBodyLimit = 4 << 10

// MachineEventsPath is held open for up to machineevents.MaxWait, so the request timeout does not apply.
const MachineEventsPath = "/v1/machine-events/wait"

type machineEventWaiter interface {
	Wait(ctx context.Context, machineID string, known map[string]int64, maxWait time.Duration) (map[string]int64, error)
}

type machineEventsRequest struct {
	Topics map[string]int64 `json:"topics"`
}

type machineEventsResponse struct {
	Topics map[string]int64 `json:"topics"`
}

// machineEventsWait answers a host that waits for changes to what it caches. The host sends the
// generation it last saw per topic; the answer is 200 with the topics that moved past it as soon as
// there are any, or 204 after 30 seconds without a change. It carries no data, only generations.
// Authentication is the renewable machine identity and a detached proof, as for other host calls.
func machineEventsWait(waiter machineEventWaiter, identities machineRequestVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, body, ok := readMachineEventsRequest(w, r)
		if !ok {
			return
		}
		proofValues := r.Header.Values("X-Paperboat-Machine-Proof")
		identityValues := r.Header.Values("X-Paperboat-Machine-Identity")
		operationValues := r.Header.Values("Idempotency-Key")
		if len(proofValues) != 1 || len(identityValues) != 1 || len(operationValues) != 1 || strings.TrimSpace(operationValues[0]) == "" {
			writePreviewTunnelError(w, r, http.StatusUnauthorized, "machine_identity_required", "A current machine identity is required.", "unchanged", false, "reauthenticate")
			return
		}
		proof, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(proofValues[0]))
		if err != nil || len(proof) == 0 {
			writePreviewTunnelError(w, r, http.StatusUnauthorized, "machine_identity_invalid", "The machine identity could not be verified.", "unchanged", false, "reauthenticate")
			return
		}
		claims, err := identities.VerifyMachineRequest(r.Context(), strings.TrimSpace(identityValues[0]), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "machine_identity_invalid", "The machine identity could not be verified.")
			return
		}
		if claims.MachineID == "" || claims.OperationID != strings.TrimSpace(operationValues[0]) {
			logHTTPRejection(r, http.StatusForbidden, "machine_events_forbidden", nil)
			writePreviewTunnelError(w, r, http.StatusForbidden, "machine_events_forbidden", "This machine cannot wait for events.", "unchanged", false, "use_authorized_host")
			return
		}
		answerMachineEvents(w, r, waiter, claims.MachineID, request)
	}
}

// readMachineEventsRequest validates the request framing shared by both ways of authenticating.
func readMachineEventsRequest(w http.ResponseWriter, r *http.Request) (machineEventsRequest, []byte, bool) {
	w.Header().Set("Cache-Control", "no-store")
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writePreviewTunnelError(w, r, http.StatusUnsupportedMediaType, "invalid_content_type", "Content-Type must be application/json.", "unchanged", false, "fix_request")
		return machineEventsRequest{}, nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, machineEventsBodyLimit+1))
	if err != nil || len(body) == 0 || len(body) > machineEventsBodyLimit {
		writePreviewTunnelError(w, r, http.StatusBadRequest, "invalid_request", "Machine events request is invalid.", "unchanged", false, "fix_request")
		return machineEventsRequest{}, nil, false
	}
	var request machineEventsRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF || request.Topics == nil {
		writePreviewTunnelError(w, r, http.StatusBadRequest, "invalid_request", "Machine events request is invalid.", "unchanged", false, "fix_request")
		return machineEventsRequest{}, nil, false
	}
	for name, generation := range request.Topics {
		if !machineevents.ValidTopic(name) || generation < 0 {
			writePreviewTunnelError(w, r, http.StatusBadRequest, "invalid_request", "Machine events request is invalid.", "unchanged", false, "fix_request")
			return machineEventsRequest{}, nil, false
		}
	}
	return request, body, true
}

// statusClientClosedRequest is logged when the client hung up before the server answered.
const statusClientClosedRequest = 499

func answerMachineEvents(w http.ResponseWriter, r *http.Request, waiter machineEventWaiter, machineID string, request machineEventsRequest) {
	changed, err := waiter.Wait(r.Context(), machineID, request.Topics, machineevents.MaxWait)
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			// The host went away before the answer: record it as such, not as a silent 200.
			w.WriteHeader(statusClientClosedRequest)
			return
		}
		writePreviewTunnelError(w, r, http.StatusServiceUnavailable, "events_unavailable", "Machine events are temporarily unavailable.", "unchanged", true, "retry", err)
		return
	}
	if len(changed) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, machineEventsResponse{Topics: changed})
}

// machineEventsWaitForUser is the same wait for the signed-in CLI daemon, which holds the user's
// credential rather than a machine identity (system-scope installs keep that under root's state).
// The user may wait only on a machine of their own account.
func machineEventsWaitForUser(machines machineLookup, waiter machineEventWaiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		request, _, ok := readMachineEventsRequest(w, r)
		if !ok {
			return
		}
		machine, err := machines.Get(r.Context(), principal.User.ID, r.PathValue("machine_id"))
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		answerMachineEvents(w, r, waiter, machine.ID, request)
	}
}

// isMachineEventsWait reports whether path is one of the long-held event waits.
func isMachineEventsWait(path string) bool {
	if path == MachineEventsPath {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/v1/machines/")
	return ok && strings.HasSuffix(rest, "/events/wait") && !strings.Contains(strings.TrimSuffix(rest, "/events/wait"), "/")
}

// NewMachineEventsHandler mounts the host's event wait on the machine identity verifier.
func NewMachineEventsHandler(waiter machineEventWaiter, identities machineRequestVerifier) (http.Handler, error) {
	if waiter == nil || identities == nil {
		return nil, errors.New("machine events dependencies are required")
	}
	return machineEventsWait(waiter, identities), nil
}
