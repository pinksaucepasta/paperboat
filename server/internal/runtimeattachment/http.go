package runtimeattachment

import (
	"bytes"
	"encoding/json"
	"errors"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type Handler struct {
	Service *Service
	Machine previewattachment.MachineProofVerifier
	Edge    previewattachment.PreviewEdgeRequestVerifier
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
	if r.URL.Path == runtimewire.HostPath {
		maximum = 4096
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
		if len(request.Observations) != 0 || len(request.Capabilities) != 1 || request.Capabilities[0] != runtimewire.CapabilityEndpoints {
			writeError(w, 400, "invalid_request")
			return
		}
		admissions, err := h.Service.PullEndpoints(r.Context(), edge)
		if err != nil {
			serviceError(w, r, err)
			return
		}
		_ = json.NewEncoder(w).Encode(runtimewire.Snapshot{Schema: runtimewire.Schema, Complete: true, Admissions: &admissions})
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
