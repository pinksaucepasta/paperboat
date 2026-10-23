package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

const machineEventsBodyLimit = 4 << 10

// MachineEventsPath is held open for up to machineevents.MaxWait, so the request timeout does not apply.
const MachineEventsPath = "/v1/machine-events/wait"

// MachineEventsAnswerHeader carries the signed answer (typ mint.MachineEventsType) to every wait, 200 or 204.
// The body is unchanged, so clients that predate the header keep working.
const MachineEventsAnswerHeader = "X-Paperboat-Events-Answer"

type machineEventSigner interface {
	SignMachineEvents(mint.MachineEventsAnswer) (string, error)
}

// MachineEventSigning signs each wait answer with the trust key the host already verifies, naming the public
// API origin as issuer. Prior art: tailscale control/controlclient/map.go (one long-poll per node that answers
// with the changes since its last map, PeersChanged/PeersRemoved in tailcfg MapResponse); ours answers with
// generations and signs the answer, so "nothing changed" is an authenticated statement too.
type MachineEventSigning struct {
	signer machineEventSigner
	issuer string
}

// NewMachineEventSigning requires the signer and the issuer; without them an answer could not be authenticated.
func NewMachineEventSigning(signer machineEventSigner, issuer string) (MachineEventSigning, error) {
	if signer == nil || strings.TrimSpace(issuer) == "" {
		return MachineEventSigning{}, errors.New("machine event signing needs a signer and an issuer")
	}
	return MachineEventSigning{signer: signer, issuer: issuer}, nil
}

// sign returns the signed answer for one wait, bound to the exact request body it answers.
func (s MachineEventSigning) sign(machineID string, body []byte, changed map[string]int64) (string, error) {
	if s.signer == nil {
		return "", errors.New("machine event signing is not configured")
	}
	sum := sha256.Sum256(body)
	return s.signer.SignMachineEvents(mint.MachineEventsAnswer{Issuer: s.issuer, MachineID: machineID, RequestSHA256: hex.EncodeToString(sum[:]), Topics: changed, IssuedAt: time.Now().UTC()})
}

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
func machineEventsWait(waiter machineEventWaiter, identities machineRequestVerifier, signing MachineEventSigning) http.HandlerFunc {
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
		answerMachineEvents(w, r, waiter, signing, claims.MachineID, body, request)
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

func answerMachineEvents(w http.ResponseWriter, r *http.Request, waiter machineEventWaiter, signing MachineEventSigning, machineID string, body []byte, request machineEventsRequest) {
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
	answer, err := signing.sign(machineID, body, changed)
	if err != nil {
		writePreviewTunnelError(w, r, http.StatusServiceUnavailable, "events_unavailable", "Machine events are temporarily unavailable.", "unchanged", true, "retry", err)
		return
	}
	w.Header().Set(MachineEventsAnswerHeader, answer)
	if len(changed) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, machineEventsResponse{Topics: changed})
}

// machineEventsWaitForUser is the same wait for the signed-in CLI daemon, which holds the user's
// credential rather than a machine identity (system-scope installs keep that under root's state).
// The user may wait only on a machine of their own account.
func machineEventsWaitForUser(machines machineLookup, waiter machineEventWaiter, signing MachineEventSigning) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		request, body, ok := readMachineEventsRequest(w, r)
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
		answerMachineEvents(w, r, waiter, signing, machine.ID, body, request)
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
func NewMachineEventsHandler(waiter machineEventWaiter, identities machineRequestVerifier, signing MachineEventSigning) (http.Handler, error) {
	if waiter == nil || identities == nil || signing.signer == nil {
		return nil, errors.New("machine events dependencies are required")
	}
	return machineEventsWait(waiter, identities, signing), nil
}
