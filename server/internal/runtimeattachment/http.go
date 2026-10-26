package runtimeattachment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

type Handler struct {
	Service      *Service
	Machine      previewattachment.MachineProofVerifier
	Edge         previewattachment.PreviewEdgeRequestVerifier
	OriginHealth OriginHealthRecorder
}

// OriginHealthRecorder stores a machine's report of which tunnel origins are reachable.
type OriginHealthRecorder interface {
	RecordOriginHealth(ctx context.Context, accountID, machineID, correlationID string, routes []tunnelv1.OriginRouteReport, now time.Time) error
}

func originHealthReports(request runtimewire.OriginHealthRequest, now time.Time) ([]tunnelv1.OriginRouteReport, bool) {
	if len(request.Routes) == 0 || len(request.Routes) > runtimewire.MaximumOriginHealthRoutes {
		return nil, false
	}
	reports := make([]tunnelv1.OriginRouteReport, 0, len(request.Routes))
	for _, route := range request.Routes {
		report := tunnelv1.OriginRouteReport{TunnelID: route.TunnelID, RouteID: route.RouteID, Code: route.Code, ObservedAt: now}
		switch route.State {
		case "reachable":
			report.State = "ready"
		case "unreachable":
			report.State = "unreachable"
		}
		codeOK := route.Code == "" && report.State == "ready" ||
			report.State == "unreachable" && (route.Code == "refused" || route.Code == "timeout" || route.Code == "unreachable" || route.Code == "tls")
		if report.State == "" || !codeOK || route.TunnelID == "" || route.RouteID == "" {
			return nil, false
		}
		reports = append(reports, report)
	}
	return reports, true
}

func decode(raw []byte, out any) error {
	if canonicaljson.RejectDuplicateFields(raw) != nil {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrInvalid
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	maximum := int64(1 << 20)
	if r.URL.Path == runtimewire.HostPath || r.URL.Path == runtimewire.HostTunnelsPath {
		maximum = 4096
	}
	if r.URL.Path == runtimewire.HostTunnelOriginHealthPath {
		maximum = 128 << 10
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maximum))
	if err != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	if r.URL.Path == runtimewire.HostPath {
		var request runtimewire.Request
		if decode(raw, &request) != nil {
			writeError(w, 400, "invalid_request")
			return
		}
		proof, err := h.Machine.VerifyMachineRequest(r.Context(), r, raw)
		if err != nil {
			if err == previewattachment.ErrUnauthorized {
				writeError(w, 401, "device_identity_invalid")
			} else {
				serviceError(w, r, err)
			}
			return
		}
		admission, err := h.Service.Attach(r.Context(), proof, request)
		if err != nil {
			serviceError(w, r, err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": admission})
		return
	}
	if r.URL.Path == runtimewire.HostTunnelOriginHealthPath {
		var request runtimewire.OriginHealthRequest
		if decode(raw, &request) != nil {
			writeError(w, 400, "invalid_request")
			return
		}
		proof, err := h.Machine.VerifyMachineRequest(r.Context(), r, raw)
		if err != nil {
			if err == previewattachment.ErrUnauthorized {
				writeError(w, 401, "device_identity_invalid")
			} else {
				serviceError(w, r, err)
			}
			return
		}
		now := time.Now().UTC()
		reports, ok := originHealthReports(request, now)
		if proof.Validate() != nil || !ok {
			writeError(w, 400, "invalid_request")
			return
		}
		if err := h.OriginHealth.RecordOriginHealth(r.Context(), proof.UserID, proof.MachineID, proof.OperationID, reports, now); err != nil {
			serviceError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == runtimewire.HostTunnelsPath {
		var request runtimewire.TunnelsRequest
		if decode(raw, &request) != nil {
			writeError(w, 400, "invalid_request")
			return
		}
		proof, err := h.Machine.VerifyMachineRequest(r.Context(), r, raw)
		if err != nil {
			if err == previewattachment.ErrUnauthorized {
				writeError(w, 401, "device_identity_invalid")
			} else {
				serviceError(w, r, err)
			}
			return
		}
		tunnels, err := h.Service.Tunnels(r.Context(), proof, request)
		if err != nil {
			serviceError(w, r, err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"tunnels": tunnels}})
		return
	}
	if r.URL.Path != runtimewire.EdgePullPath && r.URL.Path != runtimewire.EdgeObservePath && r.URL.Path != runtimewire.EdgeEndpointPullPath {
		writeError(w, 404, "not_found")
		return
	}
	var request runtimewire.EdgeRequest
	if decode(raw, &request) != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	// The shared verifier names the epoch header explicitly. This endpoint's
	// process_epoch field is its wire spelling; supply it as a candidate only
	// when absent, then let the verifier check the credential and current DB epoch.
	verifiedRequest := r.Clone(r.Context())
	if len(verifiedRequest.Header.Values("X-Paperboat-Edge-Process-Epoch")) == 0 {
		verifiedRequest.Header.Set("X-Paperboat-Edge-Process-Epoch", request.ProcessEpoch)
	}
	edge, err := h.Edge.VerifyPreviewEdgeRequest(r.Context(), verifiedRequest, raw)
	if err != nil {
		if err == previewattachment.ErrUnauthorized {
			writeError(w, 401, "edge_identity_invalid")
		} else {
			serviceError(w, r, err)
		}
		return
	}
	if edge.NodeID != request.NodeID || edge.ProcessEpoch != request.ProcessEpoch {
		writeError(w, 401, "edge_identity_invalid")
		return
	}
	if r.URL.Path == runtimewire.EdgeEndpointPullPath {
		if len(request.Observations) != 0 || !runtimewire.HasCapability(request.Capabilities, runtimewire.CapabilityEndpoints) || !runtimewire.ValidEdgeCapabilities(request.Capabilities) {
			writeError(w, 400, "invalid_request")
			return
		}
		snapshot, err := h.Service.PullEndpoints(r.Context(), edge, request.Capabilities, request.Since, request.Page)
		if err != nil {
			serviceError(w, r, err)
			return
		}
		_ = json.NewEncoder(w).Encode(snapshot)
		return
	}
	if r.URL.Path == runtimewire.EdgePullPath {
		if len(request.Observations) != 0 {
			writeError(w, 400, "invalid_request")
			return
		}
		admissions, err := h.Service.Pull(r.Context(), edge, request.Capabilities)
		if err != nil {
			serviceError(w, r, err)
			return
		}
		encoded, err := json.Marshal(runtimewire.Snapshot{Schema: runtimewire.Schema, Complete: true, Admissions: &admissions})
		if err != nil {
			serviceError(w, r, err)
			return
		}
		if len(encoded) > 16<<20 {
			cause := errors.Join(ErrUnavailable, errors.New("runtime carrier response exceeds size limit"))
			serviceError(w, r, cause)
			return
		}
		_, _ = w.Write(encoded)
		return
	}
	if len(request.Capabilities) != 0 {
		writeError(w, 400, "invalid_request")
		return
	}
	if err := h.Service.Observe(r.Context(), edge, request.Observations); err != nil {
		serviceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func serviceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == ErrInvalid:
		writeError(w, 400, "invalid_request")
	case err == ErrStale:
		writeError(w, 409, "runtime_carrier_stale")
	default:
		statusErr := supportreport.WithDiagnosticStatus(http.StatusServiceUnavailable, err)
		if err == ErrUnavailable {
			statusErr = supportreport.WithExpectedRetryableStatus(http.StatusServiceUnavailable, err)
		}
		operation := "http_other"
		if r.URL.Path == runtimewire.EdgePullPath || r.URL.Path == runtimewire.EdgeObservePath || r.URL.Path == runtimewire.EdgeEndpointPullPath {
			operation = "http_edge"
		}
		supportreport.CaptureFromContext(r.Context(), "paperboat-server", operation, "service_error", "carrier_unavailable", statusErr)
		writeError(w, 503, "runtime_carrier_unavailable")
	}
}
func writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "Runtime carrier request could not be completed.", "retryable": status == 503}})
}
