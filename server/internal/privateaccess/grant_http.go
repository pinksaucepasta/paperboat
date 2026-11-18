package privateaccess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const GrantPath = "/v1/edge/private-access/grants"

// GrantIssueRequest deliberately omits account, machine, and session fields.
// They are derived from the renewable identity proof and never accepted from
// the request body as authority.
type GrantIssueRequest = connectorprotocol.PrivateAccessGrantIssue

func bindGrantIssue(r GrantIssueRequest, identity Identity) Request {
	return Request{
		AccountID: identity.AccountID, ResourceKind: r.ResourceKind, ResourceID: r.ResourceID,
		RouteID: r.RouteID, Audience: r.Audience, MachineID: identity.MachineID,
		SessionID: identity.SessionID, InstallationGeneration: identity.InstallationGeneration, ExpiresAt: r.ExpiresAt, Nonce: r.Nonce,
		OperationID: r.OperationID, ConnectorID: r.ConnectorID, CarrierSessionID: r.CarrierSessionID,
		RouteGeneration: r.RouteGeneration, ProcessGeneration: r.ProcessGeneration,
		ConfigGeneration: r.ConfigGeneration, SessionGeneration: r.SessionGeneration,
		AssignmentGeneration: r.AssignmentGeneration, EdgeNodeID: r.EdgeNodeID, EdgeProcessEpoch: r.EdgeProcessEpoch,
		Protocol: r.Protocol, Method: r.Method,
		Host: r.Host, Path: r.Path, IdempotencyKey: r.IdempotencyKey,
		RequestID: r.RequestID, CorrelationID: r.CorrelationID,
	}
}

// GrantIssueResponse carries the server-normalized signed request: binding metadata only, never proof bytes or a
// private credential, so the edge can make the second authorize call.
type GrantIssueResponse = connectorprotocol.PrivateAccessGrantResponse

type GrantHTTPHandler struct {
	service *Service
	machine MachineRequestVerifier
}

type MachineRequestVerifier interface {
	VerifyMachineControlRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error)
}

func NewGrantHTTPHandler(service *Service, machine MachineRequestVerifier) (*GrantHTTPHandler, error) {
	if service == nil || machine == nil {
		return nil, fmt.Errorf("%w: private access grant HTTP dependencies are incomplete", ErrInvalid)
	}
	return &GrantHTTPHandler{service: service, machine: machine}, nil
}

func (h *GrantHTTPHandler) Register(mux *http.ServeMux) error {
	if h == nil || mux == nil {
		return fmt.Errorf("%w: private access grant HTTP handler or mux is nil", ErrInvalid)
	}
	mux.Handle("POST "+GrantPath, h)
	return nil
}

func (h *GrantHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.service == nil || h.machine == nil {
		capturePrivateAccessFailure(requestContext(r), http.StatusServiceUnavailable, "authorization_unavailable", ErrIdentityUnavailable, false)
		writeHTTPError(requestContext(r), w, http.StatusServiceUnavailable, "private_access_unavailable", "Private access is unavailable.", true)
		return
	}
	if r == nil || r.Method != http.MethodPost || r.URL == nil || strings.TrimSuffix(r.URL.Path, "/") != GrantPath {
		writeHTTPError(requestContext(r), w, http.StatusMethodNotAllowed, "method_not_allowed", "Private access grant issuance requires POST.", false)
		return
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || contentTypes[0] != "application/json" {
		writeHTTPError(requestContext(r), w, http.StatusUnsupportedMediaType, "content_type_required", "Private access grant issuance requires application/json.", false)
		return
	}
	raw, err := readBody(r)
	if err != nil {
		writeBodyReadError(w, r, err, "authorization_unavailable", "Private access grant request is invalid.")
		return
	}
	issueRequest, err := decodeGrantIssueRequest(raw)
	if err != nil {
		writeHTTPError(requestContext(r), w, http.StatusBadRequest, "invalid_request", "Private access grant request is invalid.", false)
		return
	}
	authorization, authErr := singleHeader(r.Header, "Authorization", false)
	machineIdentity, identityErr := singleHeader(r.Header, "X-Paperboat-Machine-Identity", false)
	proofText, proofErr := singleHeader(r.Header, "X-Paperboat-Machine-Proof", false)
	proof, decodeErr := base64.RawURLEncoding.Strict().DecodeString(proofText)
	if authErr != nil || identityErr != nil || proofErr != nil || decodeErr != nil || !strings.HasPrefix(authorization, "Bearer ") || strings.TrimPrefix(authorization, "Bearer ") != machineIdentity {
		capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "machine_identity_invalid", ErrDenied, false)
		writeHTTPError(requestContext(r), w, http.StatusUnauthorized, "machine_identity_invalid", "The machine identity could not be verified.", false)
		return
	}
	claims, err := h.machine.VerifyMachineControlRequest(r.Context(), machineIdentity, proof, r.Method, r.URL.Path, raw)
	if err != nil {
		if clockSkewed(err) {
			writeClockSkew(requestContext(r), w, err)
			return
		}
		if pureExpectedPrivateAccessError(err, controlplane.ErrHelperProof) {
			capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "machine_identity_invalid", err, false)
			writeHTTPError(requestContext(r), w, http.StatusUnauthorized, "machine_identity_invalid", "The machine identity could not be verified.", false)
		} else {
			capturePrivateAccessFailure(r.Context(), http.StatusServiceUnavailable, "authorization_unavailable", err, false)
			writeHTTPError(requestContext(r), w, http.StatusServiceUnavailable, "authorization_unavailable", "Private access authorization is unavailable.", true)
		}
		return
	}
	if claims.UserID == "" || claims.MachineID == "" || claims.InstallationGeneration <= 0 || claims.CredentialJTI == "" || claims.SessionGeneration <= 0 || claims.OperationID != issueRequest.IdempotencyKey {
		capturePrivateAccessFailure(r.Context(), http.StatusUnauthorized, "machine_identity_invalid", ErrDenied, false)
		writeHTTPError(requestContext(r), w, http.StatusUnauthorized, "machine_identity_invalid", "The machine identity could not be verified.", false)
		return
	}
	// JWT NumericDate uses whole seconds. Normalize after authenticating the
	// original bytes so the request hash, signed expiry and response agree.
	issueRequest.ExpiresAt = issueRequest.ExpiresAt.UTC().Truncate(time.Second)
	identity := Identity{AccountID: claims.UserID, UserID: claims.UserID, MachineID: claims.MachineID, SessionID: claims.CredentialJTI, InstallationGeneration: uint64(claims.InstallationGeneration), ExpiresAt: issueRequest.ExpiresAt, Method: "machine"}
	request := bindGrantIssue(issueRequest, identity)
	grant, err := h.service.IssueGrant(r.Context(), identity, EdgeIdentity{NodeID: request.EdgeNodeID, ProcessEpoch: request.EdgeProcessEpoch}, request)
	if err != nil {
		if denied, ok := privateAccessDenial(err); ok {
			status := statusForDenied(denied.Reason)
			if denied.DiagnosticCause() != nil {
				status = http.StatusServiceUnavailable
			}
			if status >= http.StatusInternalServerError {
				capturePrivateAccessDenial(r.Context(), status, denied)
			} else {
				capturePrivateAccessFailure(r.Context(), status, privateAccessFailureCode(status), denied, false)
			}
			writeHTTPError(requestContext(r), w, status, "private_access_denied", "Private access grant issuance was denied.", denied.Retryable)
			return
		}
		if errors.Is(err, ErrInvalid) {
			capturePrivateAccessFailure(r.Context(), http.StatusBadRequest, "invalid_request", err, false)
			writeHTTPError(requestContext(r), w, http.StatusBadRequest, "invalid_request", "Private access grant request is invalid.", false)
			return
		}
		if pureExpectedPrivateAccessError(err, ErrIdempotencyConflict) {
			capturePrivateAccessFailure(r.Context(), http.StatusConflict, "idempotency_conflict", err, false)
			writeHTTPError(requestContext(r), w, http.StatusConflict, "idempotency_conflict", "The private access grant retry does not match the original request.", false)
			return
		}
		capturePrivateAccessFailure(r.Context(), http.StatusServiceUnavailable, "authorization_unavailable", err, false)
		writeHTTPError(requestContext(r), w, http.StatusServiceUnavailable, "authorization_unavailable", "Private access authorization is unavailable.", true)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(GrantIssueResponse{Schema: Schema, Kind: Kind, Grant: grant, ExpiresAt: request.ExpiresAt, RequestID: request.RequestID, CorrelationID: request.CorrelationID, Request: request}); err != nil {
		capturePrivateAccessFailure(r.Context(), http.StatusOK, "authorization_unavailable", err, false)
	}
}

func decodeGrantIssueRequest(raw []byte) (GrantIssueRequest, error) {
	if len(raw) == 0 || len(raw) > MaximumRequestBody || canonicaljson.RejectDuplicateFields(raw) != nil {
		return GrantIssueRequest{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var request GrantIssueRequest
	if err := decoder.Decode(&request); err != nil {
		return GrantIssueRequest{}, ErrInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return GrantIssueRequest{}, ErrInvalid
	}
	return request, nil
}

func statusForDenied(reason DenyReason) int {
	switch reason {
	case ReasonUnauthenticated, ReasonMachineRevoked, ReasonSessionExpired, ReasonIdentityInvalid:
		return http.StatusUnauthorized
	case ReasonInternal, ReasonRoutePaused, ReasonRateLimited:
		return http.StatusServiceUnavailable
	default:
		return http.StatusForbidden
	}
}
