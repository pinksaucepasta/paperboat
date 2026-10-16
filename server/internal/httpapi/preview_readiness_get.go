package httpapi

import (
	"context"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat-server/internal/previewv1"
	"net/http"
	"strings"
)

type PreviewReadinessReadAPI interface {
	GetMachineReadiness(context.Context, previewtunnelapi.RequestContext, string) (previewv1.PreviewResult, error)
}

func previewLeaseReadinessGet(service PreviewReadinessReadAPI, identities machineRequestVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("X-Paperboat-Machine-Identity")) == "" || strings.TrimSpace(r.Header.Get("X-Paperboat-Machine-Proof")) == "" {
			writePreviewTunnelError(w, r, http.StatusUnauthorized, "machine_identity_required", "A signed host identity is required.", "unchanged", false, "run_on_authorized_host")
			return
		}
		if r.ContentLength != 0 {
			writePreviewLeaseError(w, r, fmt.Errorf("%w: readiness GET requires an empty body", previewv1.ErrInvalidInput))
			return
		}
		request, ok := previewLeaseRequestContext(w, r, nil, "", identities)
		if !ok {
			return
		}
		result, err := service.GetMachineReadiness(r.Context(), request, r.PathValue("preview_id"))
		if err != nil {
			writePreviewLeaseError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store, no-transform")
		w.Header().Set("ETag", result.ETag)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: result.Preview})
	}
}
