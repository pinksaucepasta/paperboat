package httpapi

import (
	"context"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"net/http"
)

type environmentVaultLayersAPI interface {
	environmentVaultLayerObservationsAPI
	VaultLayerRecipients(context.Context, string, environment.VaultLayerCoordinate, string, int, int) ([]environment.VaultLayerRecipient, int, error)
	PutVaultLayer(context.Context, string, string, string, environment.VaultLayerPut) (environment.VaultLayerDelivery, error)
	VaultLayerContext(context.Context, string, string, string, string) (environment.VaultLayerContext, error)
}

func vaultLayerWorkspace(w http.ResponseWriter, r *http.Request) (string, bool) {
	selected, _, err := requestWorkspace(r)
	if err != nil {
		writeWorkspaceError(w, r, err)
		return "", false
	}
	id := "personal"
	if selected.TeamID != "" {
		id = selected.TeamID
	}
	if explicit := r.URL.Query().Get("workspace_id"); explicit != "" && explicit != id {
		writeWorkspaceError(w, r, workspace.ErrForbidden)
		return "", false
	}
	return id, true
}
func registerEnvironmentVaultLayerRoutes(mux *http.ServeMux, service environmentVaultLayersAPI, verifier machineEndpointProofVerifier, read, write func(http.Handler) http.Handler) {
	registerEnvironmentVaultLayerObservationRoute(mux, service, verifier)
	mux.Handle("GET /v1/environment/layers", read(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		ws, ok := vaultLayerWorkspace(w, r)
		if !ok {
			return
		}
		for key, values := range r.URL.Query() {
			if len(values) != 1 || key != "workspace" && key != "workspace_id" && key != "owner_kind" && key != "owner_id" && key != "machine_id" && key != "state" && key != "limit" && key != "offset" {
				writeError(w, r, 400, "invalid_inventory_query", "Use supported ENV recipient filters once.")
				return
			}
		}
		// The shared pagination reader validates only inventory fields. Source
		// coordinates have already been allowlisted above and belong to this route.
		pageRequest := r.Clone(r.Context())
		pageURL := *r.URL
		pageRequest.URL = &pageURL
		pageValues := r.URL.Query()
		for _, key := range []string{"workspace_id", "owner_kind", "owner_id", "machine_id"} {
			pageValues.Del(key)
		}
		pageRequest.URL.RawQuery = pageValues.Encode()
		query, ok := readInventoryQuery(w, pageRequest, []string{"pending", "ready"}, false)
		if !ok {
			return
		}
		if query.State == "" {
			query.State = "pending"
		}
		source := environment.VaultLayerCoordinate{WorkspaceID: ws, OwnerKind: r.URL.Query().Get("owner_kind"), OwnerID: r.URL.Query().Get("owner_id"), MachineID: r.URL.Query().Get("machine_id")}
		items, total, err := service.VaultLayerRecipients(r.Context(), account, source, query.State, query.Limit, query.Offset)
		if err != nil {
			writeVaultScopeResult(w, r, nil, err)
			return
		}
		noStore(w)
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"items": items, "pagination": inventoryPagination(query.Limit, query.Offset, total, len(items))}})
	})))
	mux.Handle("PUT /v1/environment/layers/{machine_id}", write(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		ws, ok := vaultLayerWorkspace(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 512<<10)
		var in environment.VaultLayerPut
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		out, err := service.PutVaultLayer(r.Context(), account, ws, r.PathValue("machine_id"), in)
		writeVaultScopeResult(w, r, out, err)
	})))
	mux.Handle("GET /v1/environment/layers/{machine_id}", read(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		ws, ok := vaultLayerWorkspace(w, r)
		if !ok {
			return
		}
		for key, values := range r.URL.Query() {
			if len(values) != 1 || key != "workspace" && key != "workspace_id" {
				writeError(w, r, 400, "invalid_layer_context", "Use your authenticated account and selected workspace.")
				return
			}
		}
		out, err := service.VaultLayerContext(r.Context(), account, r.PathValue("machine_id"), account, ws)
		writeVaultScopeResult(w, r, out, err)
	})))
	denied := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, 401, "machine_identity_required", "A signed machine identity request is required.")
	})
	machine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := r.Context().Value(environmentMachinePrincipalKey{}).(environmentMachinePrincipal)
		if !ok || p.MachineID != r.PathValue("machine_id") {
			writeError(w, r, 403, "forbidden", "The machine identity does not authorize this resource.")
			return
		}
		limitEnvironmentJSON(w, r, 2048)
		var in struct {
			OperationID    string `json:"operation_id"`
			WorkspaceID    string `json:"workspace_id"`
			ActorAccountID string `json:"actor_account_id"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		if in.OperationID != p.OperationID {
			writeEnvironmentError(w, r, environment.ErrProtocolInvalid)
			return
		}
		out, err := service.VaultLayerContext(r.Context(), p.AccountID, p.MachineID, in.ActorAccountID, in.WorkspaceID)
		writeVaultScopeResult(w, r, out, err)
	})
	mux.Handle("POST /v1/environment/hosts/{machine_id}/layers", environmentEnrollmentMachineOrHuman(verifier, denied, machine))
}
