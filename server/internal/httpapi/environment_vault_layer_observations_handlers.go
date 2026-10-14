package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

type environmentVaultLayerObservationsAPI interface {
	RecordVaultLayerObservations(context.Context, string, string, environment.VaultLayerObservationsPut) (environment.VaultLayerObservationsAck, error)
}
type environmentLayerMachineVerifier interface {
	VerifyMachineControlRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error)
}

func registerEnvironmentVaultLayerObservationRoute(mux *http.ServeMux, service environmentVaultLayerObservationsAPI, verifier machineEndpointProofVerifier) {
	mux.HandleFunc("POST /v1/environment/hosts/{machine_id}/layer-observations", func(w http.ResponseWriter, r *http.Request) {
		// Each of the <=128 reports is <=4096 bytes. Only machine-control identity
		// is accepted; CLI helper credentials and human bearer tokens cannot report.
		v, ok := verifier.(environmentLayerMachineVerifier)
		proof, proofErr := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(r.Header.Get("X-Paperboat-Machine-Proof")))
		scheme, token, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		if !ok || proofErr != nil || len(proof) == 0 || !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			writeError(w, r, 401, "machine_identity_required", "A signed machine-control request is required.")
			return
		}
		const maxBody = environment.MaximumVaultLayerObservations*4096 + 2048
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			writeError(w, r, 400, "invalid_request", "The ENV observation batch is too large.")
			return
		}
		claims, err := v.VerifyMachineControlRequest(r.Context(), strings.TrimSpace(token), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "machine_identity_invalid", "Machine identity proof was rejected.")
			return
		}
		if claims.UserID == "" || claims.MachineID != r.PathValue("machine_id") || claims.OperationID == "" {
			writeError(w, r, 403, "machine_identity_mismatch", "The signed machine identity must match this recipient.")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var in environment.VaultLayerObservationsPut
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		if in.OperationID != claims.OperationID {
			writeError(w, r, 403, "machine_operation_mismatch", "The operation must match the signed request.")
			return
		}
		out, err := service.RecordVaultLayerObservations(r.Context(), claims.UserID, claims.MachineID, in)
		writeVaultScopeResult(w, r, out, err)
	})
}
