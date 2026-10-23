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

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/pinksaucepasta/paperboat/server/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

const connectorOriginHealthBodyLimit = 16 << 10

type connectorOriginHealthRecorder interface {
	RecordOriginHealth(ctx context.Context, accountID, tunnelID, connectorID, sessionID, correlationID string, routes []tunnelv1.OriginRouteReport, now time.Time) error
}

// NewConnectorOriginHealthHandler accepts the host's report of whether each route's local app is
// reachable. It is authenticated like carrier bootstrap: the renewable machine identity and a
// detached proof over the exact method, path and body, bound to the active connector session.
func NewConnectorOriginHealthHandler(sessions *connectorprotocol.ActiveControlSessions, recorder connectorOriginHealthRecorder, identities *controlplane.EnrollmentService) (http.Handler, error) {
	if sessions == nil || recorder == nil || identities == nil {
		return nil, wire.ErrInvalidInput
	}
	return connectorOriginHealth(sessions, recorder, identities, time.Now), nil
}

func connectorOriginHealth(sessions activeConnectorControlSessionReader, recorder connectorOriginHealthRecorder, identities machineRequestVerifier, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			writePreviewTunnelError(w, r, http.StatusUnsupportedMediaType, "invalid_content_type", "Content-Type must be application/json.", "unchanged", false, "fix_request")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, connectorOriginHealthBodyLimit+1))
		if err != nil || len(body) == 0 || len(body) > connectorOriginHealthBodyLimit {
			writePreviewTunnelError(w, r, http.StatusBadRequest, "invalid_request", "Origin health report is invalid.", "unchanged", false, "fix_request")
			return
		}
		var request connectorprotocol.OriginHealthRequest
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		reportedAt := now().UTC()
		if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF || request.Validate(reportedAt) != nil {
			writePreviewTunnelError(w, r, http.StatusBadRequest, "invalid_request", "Origin health report is invalid.", "unchanged", false, "fix_request")
			return
		}
		tunnelID, connectorID := r.PathValue("tunnel_id"), r.PathValue("connector_id")
		active, ok := sessions.Lookup(tunnelID, connectorID, request.SessionID, request.ProcessGeneration)
		if !ok {
			writePreviewTunnelError(w, r, http.StatusConflict, "connector_session_stale", "The connector session changed before the origin health report.", "unchanged", true, "reconnect")
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
		if claims.MachineID != active.HostID || claims.UserID != active.AccountID || claims.OperationID != strings.TrimSpace(operationValues[0]) {
			logHTTPRejection(r, http.StatusForbidden, "connector_access_forbidden", controlplane.ErrHelperProof)
			writePreviewTunnelError(w, r, http.StatusForbidden, "connector_access_forbidden", "This machine cannot report origin health for the connector session.", "unchanged", false, "use_authorized_host")
			return
		}
		routes := make([]tunnelv1.OriginRouteReport, 0, len(request.Routes))
		for _, route := range request.Routes {
			routes = append(routes, tunnelv1.OriginRouteReport{RouteID: route.RouteID, State: route.State, Code: route.Code, ObservedAt: route.ObservedAt})
		}
		if err := recorder.RecordOriginHealth(r.Context(), active.AccountID, active.TunnelID, active.ConnectorID, active.SessionID, supportReference(r.Context()), routes, reportedAt); err != nil {
			if errors.Is(err, tunnelv1.ErrInvalidInput) {
				writePreviewTunnelError(w, r, http.StatusBadRequest, "invalid_request", "Origin health report is invalid.", "unchanged", false, "fix_request")
				return
			}
			writePreviewTunnelError(w, r, http.StatusServiceUnavailable, "connector_control_unavailable", "Connector control is temporarily unavailable.", "unchanged", true, "retry", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
