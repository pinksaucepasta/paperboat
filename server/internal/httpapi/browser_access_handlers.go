package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/lazyaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat-server/internal/previewv1"

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat-server/internal/browseringress"
	"github.com/pinksaucepasta/paperboat-server/internal/privateaccess"
)

type BrowserAccessHandlers struct {
	Lazy                   *lazyaccess.Activator
	Access                 *browseraccess.Service
	Ingress                *browseringress.Service
	Edge                   previewattachment.PreviewEdgeRequestVerifier
	Machine                previewattachment.MachineProofVerifier
	ViewerPolicyReconciler interface {
		ReconcileTunnelEdgeRouteAssignments(context.Context, int32) (int, error)
	}
}

func browserAccessError(w http.ResponseWriter, r *http.Request, err error) {
	activationStatus, activationCode, activation := browserActivationResponse(err)
	switch {
	case activation:
		writeError(w, r, activationStatus, activationCode, "Private service activation did not complete. No application request was forwarded; retry after checking host and service readiness.", err)
	case expectedHTTPErrorOnly(err, lazyaccess.ErrCapacity):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, 429, "activation_limit", "Private service activation is busy. Retry after one second.")
	case expectedHTTPErrorOnly(err, browseraccess.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Invalid browser access request.")
	case expectedHTTPErrorOnly(err, browseraccess.ErrExpired, browseraccess.ErrAlreadyRedeemed):
		writeError(w, r, 401, "authentication_required", "Sign in again.")
	case expectedHTTPErrorOnly(err, browseraccess.ErrAmbiguousTeamAccess):
		writeError(w, r, 409, "ambiguous_team_access", "More than one team grants access to this resource. Use a resource with one team grant until team selection is available.")
	case expectedHTTPErrorOnly(err, accountingauthority.ErrQuotaExhausted):
		writeError(w, r, 409, "bandwidth_quota_exhausted", "This workspace's bandwidth allowance is exhausted. Access can resume when its next allowance period begins.")
	case expectedHTTPErrorOnly(err, browseraccess.ErrNotAuthorized, connectorprotocol.ErrIngressDenied):
		writeError(w, r, 404, "not_found", "Resource not found.")
	default:
		writeError(w, r, 503, "authorization_unavailable", "Browser authorization is unavailable.", err)
	}
}

// Activation state belongs to lazyaccess. Only one complete expected activation
// can retain its public status; independent operational failures use the safe
// request owner below instead of being hidden behind an activation code.
func browserActivationResponse(err error) (httpStatus int, code string, expected bool) {
	defer func() {
		if recover() != nil {
			httpStatus, code, expected = 0, "", false
		}
	}()
	for depth := 0; err != nil && depth < 8; depth++ {
		if activation, ok := err.(*lazyaccess.ActivationError); ok {
			if activation == nil {
				return 0, "", false
			}
			code = activation.Code
			httpStatus = http.StatusServiceUnavailable
			var allowed []error
			switch code {
			case "activation_timeout":
				httpStatus = http.StatusGatewayTimeout
				allowed = []error{context.Canceled, context.DeadlineExceeded}
			case "generation_conflict":
				httpStatus = http.StatusConflict
				allowed = []error{pgx.ErrNoRows, previewtunnelstore.ErrGenerationConflict, lazyaccess.ErrConflict}
			case "host_offline":
				allowed = []error{previewtunnelstore.ErrOwnerNotFound}
			case "owner_replaced":
				allowed = []error{previewv1.ErrOwnerDenied}
			case "activation_cooldown", "origin_unavailable":
			case "forwarding_failed":
				return httpStatus, code, true
			default:
				return 0, "", false
			}
			return httpStatus, code, activation.Cause == nil || expectedHTTPErrorOnly(activation.Cause, allowed...)
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) != 1 {
				return 0, "", false
			}
			err = children[0]
			continue
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			err = wrapped.Unwrap()
			continue
		}
		return 0, "", false
	}
	return 0, "", false
}

type browserBodyFailure struct{ cause error }

func (*browserBodyFailure) Error() string   { return "browser request body could not be read" }
func (e *browserBodyFailure) Unwrap() error { return e.cause }

func browserDecode(raw []byte, v any) error {
	if canonicaljson.RejectDuplicateFields(raw) != nil {
		return browseraccess.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return browseraccess.ErrInvalid
	}
	return nil
}
func browserBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Header.Get("Content-Type") != "application/json" {
		return nil, browseraccess.ErrInvalid
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		if oversized, ok := err.(*http.MaxBytesError); ok && oversized != nil {
			return nil, browseraccess.ErrInvalid
		}
		return nil, &browserBodyFailure{cause: err}
	}
	return raw, nil
}
func (h *BrowserAccessHandlers) Issue(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	p, ok := principalFromContext(r.Context())
	if !ok || p.Client != nil || p.Session.ID == "" {
		writeError(w, r, 401, "authentication_required", "Sign in again.")
		return
	}
	raw, err := browserBody(w, r)
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	var input browseraccess.IssueRequest
	if err = browserDecode(raw, &input); err != nil {
		browserAccessError(w, r, err)
		return
	}
	input.Principal = p.Session
	out, err := h.Access.Issue(r.Context(), input)
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	writeJSON(w, 200, SuccessResponse{Data: out})
}
func (h *BrowserAccessHandlers) EdgeRequest(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	raw, err := browserBody(w, r)
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	edge, err := h.Edge.VerifyPreviewEdgeRequest(r.Context(), r, raw)
	if err != nil {
		if errors.Is(err, previewattachment.ErrUnauthorized) {
			logHTTPRejection(r, http.StatusUnauthorized, "edge_identity_invalid", err)
			writeError(w, r, http.StatusUnauthorized, "edge_identity_invalid", "Edge authentication required.")
		} else {
			writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "Edge authorization is temporarily unavailable.", err)
		}
		return
	}
	var out any
	switch r.PathValue("operation") {
	case "begin":
		var input browseraccess.BeginRequest
		if err = browserDecode(raw, &input); err == nil {
			out, err = h.Access.Begin(r.Context(), input)
		}
	case "redeem":
		var input browseraccess.RedeemRequest
		if err = browserDecode(raw, &input); err == nil {
			out, err = h.Access.Redeem(r.Context(), input)
		}
	case "authorize", "activate":
		var input struct {
			browseraccess.AuthorizeRequest
			CredentialKind string `json:"credential_kind,omitempty"`
		}
		if err = browserDecode(raw, &input); err == nil {
			var a browseraccess.Authorization
			switch {
			case input.GrantID != "":
				a, err = h.Access.AuthorizeGrant(r.Context(), input.GrantID, input.Host, input.ResourceKind, input.ResourceID, input.RouteID)
			case input.CredentialKind == "" || input.CredentialKind == "browser":
				a, err = h.Access.Authorize(r.Context(), input.AuthorizeRequest)
			case input.CredentialKind == "machine":
				a, err = h.Access.AuthorizeMachineCredential(r.Context(), input.Token, input.Host, input.ResourceKind, input.ResourceID, input.RouteID, "use")
			default:
				err = browseraccess.ErrInvalid
			}
			if err == nil && r.PathValue("operation") == "activate" {
				if h.Lazy == nil || a.ResourceKind != "lazy_policy" {
					err = browseraccess.ErrNotAuthorized
				} else {
					original := a
					check := func(ctx context.Context) error {
						var current browseraccess.Authorization
						var e error
						if input.CredentialKind == "machine" {
							current, e = h.Access.AuthorizeMachineCredential(ctx, input.Token, input.Host, "lazy_policy", original.ResourceID, original.RouteID, "use")
						} else {
							current, e = h.Access.Authorize(ctx, browseraccess.AuthorizeRequest{Token: input.Token, Host: input.Host, ResourceKind: "lazy_policy", ResourceID: original.ResourceID, RouteID: original.RouteID})
						}
						if e == nil && (current.ResourceGeneration != original.ResourceGeneration || current.OwnerAccountID != original.OwnerAccountID) {
							return browseraccess.ErrNotAuthorized
						}
						a = current
						return e
					}
					_, err = h.Lazy.Activate(r.Context(), lazyaccess.ActivationRequest{PolicyID: a.ResourceID, OwnerAccountID: a.OwnerAccountID, PolicyGeneration: a.ResourceGeneration, AuthorityDeadline: a.SessionExpiresAt, Check: check})
				}
			}
			if err == nil {
				out, err = h.Ingress.Resolve(r.Context(), a, edge.NodeID, edge.ProcessEpoch)
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}
func (h *BrowserAccessHandlers) MachineRequest(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	raw, err := browserBody(w, r)
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	proof, err := h.Machine.VerifyMachineRequest(r.Context(), r, raw)
	if err != nil {
		writeMachineProofFailure(w, r, err, "machine_identity_invalid", "Machine authentication required.")
		return
	}
	var input struct {
		Decision connectorprotocol.IngressDecision `json:"decision"`
		Open     connectorprotocol.StreamOpen      `json:"open"`
	}
	if err = browserDecode(raw, &input); err != nil {
		browserAccessError(w, r, err)
		return
	}
	d := input.Decision
	// Expired decisions are selectors during refresh, never fresh authority.
	if d.Binding.Validate() != nil || input.Open.Validate() != nil || d.Binding.Audience == "public" || d.Binding.AccountID != proof.UserID || d.Binding.HostID != proof.MachineID || d.Binding.InstallationGeneration != proof.InstallationGeneration {
		browserAccessError(w, r, connectorprotocol.ErrIngressDenied)
		return
	}
	if d.NativeAuthorization != nil {
		if h.Ingress.Native == nil || d.NativeAuthorization.Validate(time.Time{}) != nil {
			browserAccessError(w, r, connectorprotocol.ErrIngressDenied)
			return
		}
		a := d.NativeAuthorization
		result, err := h.Ingress.Native.AuthorizeIngress(r.Context(), privateaccess.AuthorizeInput{Grant: a.Grant, Request: privateaccess.Request(a.Request), Edge: privateaccess.EdgeIdentity{NodeID: d.EdgeNodeID, ProcessEpoch: d.EdgeProcessEpoch}, AdmittedDecisionID: d.DecisionID})
		if err != nil {
			var denied *privateaccess.DeniedError
			if errors.As(err, &denied) && denied.DiagnosticCause() == nil && denied.Reason != privateaccess.ReasonInternal {
				browserAccessError(w, r, connectorprotocol.ErrIngressDenied)
			} else {
				browserAccessError(w, r, err)
			}
			return
		}
		if !result.Allowed || result.Ingress == nil {
			browserAccessError(w, r, connectorprotocol.ErrIngressDenied)
			return
		}
		current := *result.Ingress
		// Publisher proof and current projection must agree independently of accessor identity.
		if current.Binding.AccountID != proof.UserID || current.Binding.HostID != proof.MachineID || current.Binding.InstallationGeneration != proof.InstallationGeneration {
			browserAccessError(w, r, connectorprotocol.ErrIngressDenied)
			return
		}
		d.IssuedAt, d.ExpiresAt = current.IssuedAt, current.ExpiresAt
		if d.Revalidate(current, input.Open, current.EdgeNodeID, current.EdgeProcessEpoch, time.Now().UTC()) != nil {
			browserAccessError(w, r, connectorprotocol.ErrIngressDenied)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: current})
		return
	}
	kind, id := "tunnel", d.Binding.TunnelID
	if d.Binding.Lifecycle == connectorprotocol.TunnelEphemeral {
		kind, id = "preview", d.Binding.PublicationID
	}
	a, err := h.Access.AuthorizeGrant(r.Context(), d.GrantID, d.Binding.Hostname, kind, id, d.Binding.RouteID)
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	current, err := h.Ingress.Resolve(r.Context(), a, d.EdgeNodeID, d.EdgeProcessEpoch)
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	d.IssuedAt, d.ExpiresAt = current.IssuedAt, current.ExpiresAt
	if d.Revalidate(current, input.Open, current.EdgeNodeID, current.EdgeProcessEpoch, time.Now().UTC()) != nil {
		browserAccessError(w, r, connectorprotocol.ErrIngressDenied)
		return
	}
	writeJSON(w, 200, SuccessResponse{Data: current})
}

func (h *BrowserAccessHandlers) MachineCredential(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	p, ok := principalFromContext(r.Context())
	if !ok {
		writeError(w, r, 401, "authentication_required", "Sign in again.")
		return
	}
	raw, err := browserBody(w, r)
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	var input struct {
		Host         string `json:"host"`
		ResourceKind string `json:"resource_kind"`
		ResourceID   string `json:"resource_id"`
		RouteID      string `json:"route_id"`
	}
	if err = browserDecode(raw, &input); err != nil {
		browserAccessError(w, r, err)
		return
	}
	out, err := h.Access.IssueMachineCredential(r.Context(), browseraccess.MachineCredentialRequest{AccountID: p.User.ID, Host: input.Host, ResourceKind: input.ResourceKind, ResourceID: input.ResourceID, RouteID: input.RouteID, Action: "use", TTL: 5 * time.Minute})
	if err != nil {
		browserAccessError(w, r, err)
		return
	}
	writeJSON(w, 200, SuccessResponse{Data: out})
}
