package httpapi

import (
	"context"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

type environmentVaultScopesAPI interface {
	StagePersonalVaultScope(context.Context, string, string, environment.VaultPersonalScopeStage) (environment.VaultScopeDocument, error)
	AbortPersonalVaultRotation(context.Context, string, string) error
	RotatePersonalVault(context.Context, string, environment.VaultPersonalRotate) (environment.PasswordVaultHead, error)
	GetVaultPersonalScopes(context.Context, string) (environment.VaultPersonalScopes, error)
	GetVaultScope(context.Context, string, string, string, string, string) (environment.VaultScopeState, error)
	PutVaultScope(context.Context, string, string, string, string, string, environment.VaultScopePut) (environment.VaultScopeState, error)
	GetVaultSharing(context.Context, string, string) (environment.VaultSharingState, error)
	CreateVaultTeam(context.Context, string, environment.VaultTeamCreate) (environment.VaultTeamState, error)
	GetVaultTeam(context.Context, string, string) (environment.VaultTeamState, error)
	GrantVaultTeamMember(context.Context, string, string, environment.VaultMemberGrant) (environment.VaultTeamState, error)
	RotateVaultTeam(context.Context, string, string, environment.VaultTeamRotate) (environment.VaultTeamState, error)
	ListVaultGrants(context.Context, string) ([]environment.VaultGrantState, error)
	AcknowledgeVaultGrant(context.Context, string, string, string) error
	ResetPersonalVault(context.Context, string, environment.VaultPersonalReset) (environment.PasswordVaultHead, error)
}

func registerEnvironmentVaultScopeRoutes(mux *http.ServeMux, service environmentVaultScopesAPI, read, write func(http.Handler) http.Handler) {
	mux.Handle("PUT /v1/environment/rotate-personal/{operation_id}/scopes", write(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		limitEnvironmentJSON(w, r, 1<<20)
		var req environment.VaultPersonalScopeStage
		if !decodeStrictJSON(w, r, &req) {
			return
		}
		out, err := service.StagePersonalVaultScope(r.Context(), account, r.PathValue("operation_id"), req)
		writeVaultScopeResult(w, r, out, err)
	})))
	mux.Handle("DELETE /v1/environment/rotate-personal/{operation_id}", write(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		err := service.AbortPersonalVaultRotation(r.Context(), account, r.PathValue("operation_id"))
		writeVaultScopeResult(w, r, map[string]bool{"aborted": true}, err)
	})))

	mux.Handle("GET /v1/environment/scopes", read(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		out, err := service.GetVaultPersonalScopes(r.Context(), account)
		writeVaultScopeResult(w, r, out, err)
	})))
	mux.Handle("GET /v1/environment/scopes/{kind}/{owner_id}", read(environmentVaultScopeHandler(service, false)))
	mux.Handle("PUT /v1/environment/scopes/{kind}/{owner_id}", write(environmentVaultScopeHandler(service, true)))
	mux.Handle("GET /v1/environment/users/{account_id}/sharing", read(environmentVaultSharingHandler(service)))
	mux.Handle("POST /v1/environment/teams", write(environmentVaultTeamHandler(service, "create")))
	mux.Handle("GET /v1/environment/teams/{team_id}", read(environmentVaultTeamHandler(service, "get")))
	mux.Handle("POST /v1/environment/teams/{team_id}/members", write(environmentVaultTeamHandler(service, "grant")))
	mux.Handle("POST /v1/environment/teams/{team_id}/rotate", write(environmentVaultTeamHandler(service, "rotate")))
	mux.Handle("GET /v1/environment/grants", read(environmentVaultGrantsHandler(service, false)))
	mux.Handle("POST /v1/environment/grants/{document_id}/ack", write(environmentVaultGrantsHandler(service, true)))
	mux.Handle("POST /v1/environment-vault/reset", write(environmentVaultResetHandler(service)))
	mux.Handle("POST /v1/environment/rotate-personal", write(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 2<<20)
		var req environment.VaultPersonalRotate
		if !decodeStrictJSON(w, r, &req) {
			return
		}
		out, err := service.RotatePersonalVault(r.Context(), account, req)
		writeVaultScopeResult(w, r, out, err)
	})))
}
func vaultScopePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	noStore(w)
	p, ok := principalFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		return "", false
	}
	return p.User.ID, true
}
func writeVaultScopeResult(w http.ResponseWriter, r *http.Request, result any, err error) {
	if expectedHTTPErrorOnly(err, environment.ErrTeamEntitlementRequired) {
		writeError(w, r, http.StatusForbidden, "team_entitlement_required", "Activate this team's subscription in Billing before using team ENV scopes. The encrypted initialization remains saved.")
		return
	}
	if expectedHTTPErrorOnly(err, environment.ErrVaultRotationRequired) {
		writeError(w, r, http.StatusConflict, "rotation_required", "Rotate the affected personal or team ENV keys on an unlocked authorized machine before writing or provisioning hosts.")
		return
	}
	if expectedHTTPErrorOnly(err, environment.ErrKeyAuthorizationRequired) {
		writeError(w, r, http.StatusForbidden, "forbidden", "Current ENV ownership or team membership is required.")
		return
	}
	if err != nil {
		writeEnvironmentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: result})
}
func environmentVaultScopeHandler(service environmentVaultScopesAPI, mutation bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		kind, owner, machine := r.PathValue("kind"), r.PathValue("owner_id"), r.URL.Query().Get("machine_id")
		selected, _, err := requestWorkspace(r)
		if err != nil {
			writeWorkspaceError(w, r, err)
			return
		}
		workspaceID := "personal"
		if selected.TeamID != "" {
			workspaceID = selected.TeamID
		}
		if explicit := r.URL.Query().Get("workspace_id"); explicit != "" && explicit != workspaceID {
			writeWorkspaceError(w, r, workspace.ErrForbidden)
			return
		}
		if kind == "team" && (workspaceID != owner || owner == "personal") {
			writeWorkspaceError(w, r, workspace.ErrForbidden)
			return
		}
		if !mutation {
			out, err := service.GetVaultScope(r.Context(), account, workspaceID, kind, owner, machine)
			writeVaultScopeResult(w, r, out, err)
			return
		}
		limitEnvironmentJSON(w, r, 1<<20)
		var req environment.VaultScopePut
		if !decodeStrictJSON(w, r, &req) {
			return
		}
		out, err := service.PutVaultScope(r.Context(), account, workspaceID, kind, owner, machine, req)
		writeVaultScopeResult(w, r, out, err)
	}
}
func environmentVaultSharingHandler(service environmentVaultScopesAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		out, err := service.GetVaultSharing(r.Context(), account, r.PathValue("account_id"))
		writeVaultScopeResult(w, r, out, err)
	}
}
func environmentVaultTeamHandler(service environmentVaultScopesAPI, operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		team := r.PathValue("team_id")
		var out environment.VaultTeamState
		var err error
		switch operation {
		case "get":
			if !requireTeamWorkspaceTarget(w, r, team) {
				return
			}
			out, err = service.GetVaultTeam(r.Context(), account, team)
		case "create":
			limitEnvironmentJSON(w, r, 730<<10)
			var req environment.VaultTeamCreate
			if !decodeStrictJSON(w, r, &req) {
				return
			}
			if !requireTeamWorkspaceTarget(w, r, req.TeamID) {
				return
			}
			out, err = service.CreateVaultTeam(r.Context(), account, req)
		case "grant":
			if !requireTeamWorkspaceTarget(w, r, team) {
				return
			}
			limitEnvironmentJSON(w, r, 8<<10)
			var req environment.VaultMemberGrant
			if !decodeStrictJSON(w, r, &req) {
				return
			}
			out, err = service.GrantVaultTeamMember(r.Context(), account, team, req)
		case "rotate":
			if !requireTeamWorkspaceTarget(w, r, team) {
				return
			}
			limitEnvironmentJSON(w, r, 1500<<10)
			var req environment.VaultTeamRotate
			if !decodeStrictJSON(w, r, &req) {
				return
			}
			out, err = service.RotateVaultTeam(r.Context(), account, team, req)
		}
		writeVaultScopeResult(w, r, out, err)
	}
}
func environmentVaultGrantsHandler(service environmentVaultScopesAPI, ack bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		if ack {
			if !requirePersonalWorkspaceTarget(w, r) {
				return
			}
			limitEnvironmentJSON(w, r, 1024)
			var req struct {
				VaultDocumentID string `json:"vault_document_id"`
			}
			if !decodeStrictJSON(w, r, &req) {
				return
			}
			err := service.AcknowledgeVaultGrant(r.Context(), account, r.PathValue("document_id"), req.VaultDocumentID)
			writeVaultScopeResult(w, r, map[string]bool{"acknowledged": true}, err)
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		grants, err := service.ListVaultGrants(r.Context(), account)
		writeVaultScopeResult(w, r, struct {
			Grants []environment.VaultGrantState `json:"grants"`
		}{grants}, err)
	}
}
func environmentVaultResetHandler(service environmentVaultScopesAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		limitEnvironmentJSON(w, r, 2<<20)
		var req environment.VaultPersonalReset
		if !decodeStrictJSON(w, r, &req) {
			return
		}
		out, err := service.ResetPersonalVault(r.Context(), account, req)
		writeVaultScopeResult(w, r, out, err)
	}
}
