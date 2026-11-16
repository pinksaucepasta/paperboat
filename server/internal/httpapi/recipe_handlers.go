package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/recipes"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// Recipes (PLAN F26). Byte fields (auth, wrapped, sealed, envelope) are standard base64 in JSON.

type recipeWorkspaceKey struct{}

func recipeWorkspace(c *auth.ClientPrincipal) workspace.Scope {
	if c.RecipeTeamID == "" {
		return workspace.Scope{ID: "personal"}
	}
	return workspace.Scope{ID: c.RecipeTeamID, TeamID: c.RecipeTeamID}
}

// recipeRoute is what a recipe machine's pb session may call: every read (as the creator, in the
// recipe's workspace), the machine actions whose authorization checks the reach
// (usermachines.getMachineForCapability), its own machine's routes, pairing and the connection
// plumbing pb needs. Anything else (recipes, policy, team and account changes) is refused.
func recipeRoute(r *http.Request, own string) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	p := r.URL.Path
	if own != "" && (strings.HasPrefix(p, "/v1/machines/"+own+"/") || p == "/v1/machines/"+own || strings.HasPrefix(p, "/v1/environment/hosts/"+own+"/")) {
		return true
	}
	switch {
	case strings.HasPrefix(p, "/v1/workspaces/") && (strings.HasSuffix(p, "/policy/explain") || strings.HasSuffix(p, "/policy/validate")):
		return true
	case p == "/v1/machines/setup", p == "/v1/peer-network/config", p == "/v1/peer-network/register", p == "/v1/e2ee/bootstrap":
		return true
	case strings.HasPrefix(p, "/v1/recipe-machine"):
		return true
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/v1/ssh/client-keys/"):
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	if p == "/v1/native-private-access/grants" {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(p, "/v1/"), "/")
	switch {
	case len(parts) == 3 && parts[0] == "machines" && (parts[2] == "exec-descriptor" || parts[2] == "file-transfer-descriptor" ||
		parts[2] == "connection-descriptor" || parts[2] == "terminal-sessions" || parts[2] == "events"):
		return true
	case len(parts) == 4 && parts[0] == "machines" && parts[2] == "events" && parts[3] == "wait":
		return true
	case len(parts) == 3 && parts[0] == "terminal-sessions" && parts[2] == "connection-descriptor":
		return true
	case len(parts) == 5 && parts[0] == "machines" && parts[2] == "terminal-sessions" && parts[4] == "close":
		return true
	}
	return false
}

func writeRecipeRouteRefusal(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusForbidden, "outside_recipe_reach", "A machine paired from a recipe cannot do this. Sign in with pb login to manage your workspace.")
}

type recipeJSON struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Workspace   string        `json:"workspace"`
	Revision    int           `json:"revision"`
	Token       string        `json:"token"`
	Source      string        `json:"source"`
	Format      string        `json:"format"`
	Recipe      recipe.Recipe `json:"recipe"`
	CreatedBy   string        `json:"created_by"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	ExpiresAt   time.Time     `json:"expires_at"`
	LastUsedAt  *time.Time    `json:"last_used_at"`
	RotatedAt   *time.Time    `json:"rotated_at"`
	Machines    int           `json:"machines"`
	MaxMachines int           `json:"max_machines,omitempty"`
}

func recipeView(r recipes.Recipe) recipeJSON {
	return recipeJSON{ID: r.ID, Name: r.Name, Workspace: r.Workspace.ID, Revision: r.Revision, Token: recipe.MaskedID(r.TokenID), Source: r.Source,
		Format: r.Format, Recipe: r.Recipe, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ExpiresAt: r.ExpiresAt,
		LastUsedAt: r.LastUsedAt, RotatedAt: r.RotatedAt, Machines: r.Machines, MaxMachines: r.MaxMachines}
}

// recipeRequest resolves {ws} and the acting account; a recipe session never manages recipes
// (recipeRoute refuses its writes).
func recipeRequest(w http.ResponseWriter, r *http.Request) (string, workspace.Scope, bool) {
	noStore(w)
	p, ok := principalFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		return "", workspace.Scope{}, false
	}
	ws, err := workspace.Parse(r.PathValue("ws"))
	if err != nil {
		writeWorkspaceError(w, r, workspace.ErrInvalid)
		return "", workspace.Scope{}, false
	}
	if p.Client != nil && p.Client.RecipeID != "" && recipeWorkspace(p.Client) != ws {
		writeWorkspaceError(w, r, workspace.ErrForbidden)
		return "", workspace.Scope{}, false
	}
	return p.User.ID, ws, true
}

type recipeFileBody struct {
	Name    string              `json:"name"`
	Source  string              `json:"source"`
	Format  string              `json:"format"`
	TokenID string              `json:"token_id"`
	Auth    []byte              `json:"auth"`
	Wraps   []recipes.VaultWrap `json:"vault_wraps"`
}

func recipeCreate(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 256<<10)
		var in recipeFileBody
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		out, err := service.Create(r.Context(), account, ws, recipes.Input{Name: in.Name, Source: in.Source, Format: in.Format, TokenID: in.TokenID, Auth: in.Auth, Wraps: in.Wraps})
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: map[string]any{"recipe": recipeView(out)}})
	}
}

func recipeUpdate(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 256<<10)
		var in struct {
			Source string              `json:"source"`
			Format string              `json:"format"`
			Wraps  []recipes.VaultWrap `json:"vault_wraps"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		before, after, err := service.Update(r.Context(), account, ws, recipes.Input{Name: r.PathValue("name"), Source: in.Source, Format: in.Format, Wraps: in.Wraps})
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		_, machines, err := service.Get(r.Context(), account, ws, after.Name)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		online := 0
		for _, m := range machines {
			if m.Online {
				online++
			}
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"recipe": recipeView(after), "previous": recipeView(before), "online_machines": online}})
	}
}

func recipeList(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		list, err := service.List(r.Context(), account, ws)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		out := make([]recipeJSON, 0, len(list))
		for _, x := range list {
			out = append(out, recipeView(x))
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"recipes": out}})
	}
}

func recipeGet(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		out, machines, err := service.Get(r.Context(), account, ws, r.PathValue("name"))
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		if machines == nil {
			machines = []recipes.MachineState{}
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"recipe": recipeView(out), "machines": machines}})
	}
}

func recipeRotate(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 1<<20)
		var in struct {
			Auth        []byte `json:"auth"`
			Confirm     string `json:"confirm"`
			MachineKeys []struct {
				MachineID string `json:"machine_id"`
				Sealed    []byte `json:"sealed"`
			} `json:"machine_keys"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		keys := make([]recipes.MachineKey, 0, len(in.MachineKeys))
		for _, k := range in.MachineKeys {
			keys = append(keys, recipes.MachineKey{MachineID: k.MachineID, Sealed: k.Sealed})
		}
		out, err := service.Rotate(r.Context(), account, ws, r.PathValue("name"), in.Auth, keys, in.Confirm)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"recipe": recipeView(out)}})
	}
}

func recipeRevoke(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		var in struct {
			Confirm string `json:"confirm"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		out, impact, err := service.Revoke(r.Context(), account, ws, r.PathValue("name"), in.Confirm)
		if err != nil && out.ID == "" {
			writeRecipeError(w, r, err)
			return
		}
		data := map[string]any{"recipe": recipeView(out), "impact": impact}
		if err != nil {
			// Revoked; a machine removal failed and is retried by the collector.
			data["removal_error"] = "Some machines are still being removed; pb recipe show lists them."
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: data})
	}
}

func writeRecipeError(w http.ResponseWriter, r *http.Request, err error) {
	var file *recipes.FileError
	var reach *recipes.ReachError
	var name *recipes.NameError
	var confirm *recipes.ConfirmError
	var wrap *recipes.WrapMissingError
	switch {
	case errors.As(err, &file):
		details := make([]map[string]any, 0, len(file.Errors))
		for _, e := range file.Errors {
			details = append(details, map[string]any{"line": e.Line, "column": e.Col, "path": e.Path, "message": e.Message, "text": e.Error()})
		}
		writeErrorDetails(w, r, http.StatusUnprocessableEntity, "invalid_recipe", file.Error(), map[string]any{"errors": details})
	case errors.As(err, &reach):
		writeErrorDetails(w, r, http.StatusUnprocessableEntity, "invalid_recipe_reach", "The recipe's [[reach]] names something this workspace does not have.", map[string]any{"errors": reach.Errors})
	case errors.As(err, &name):
		writeErrorDetails(w, r, http.StatusUnprocessableEntity, "unknown_"+name.Kind, name.Error(), map[string]any{"kind": name.Kind, "name": name.Name, "known": name.Known, "suggestion": name.Suggestion})
	case errors.As(err, &confirm):
		writeErrorDetails(w, r, http.StatusConflict, "confirmation_required", "Run the command again with the confirmation code.", map[string]any{"confirm": confirm.Code, "impact": confirm.Impact})
	case errors.As(err, &wrap):
		writeErrorDetails(w, r, http.StatusUnprocessableEntity, "vault_key_missing", wrap.Error(), map[string]any{"vault": wrap.Vault})
	case expectedHTTPErrorOnly(err, recipes.ErrNameTaken):
		writeError(w, r, http.StatusConflict, "recipe_name_taken", "A recipe with this name already exists. Change it with pb recipe update -f <file>.")
	case expectedHTTPErrorOnly(err, recipes.ErrTokenTaken):
		writeError(w, r, http.StatusConflict, "recipe_token_taken", "This token id is taken; make a new token and retry.")
	case expectedHTTPErrorOnly(err, recipes.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "Recipes in a team are created by owners and admins (a recipe tags machines, which edits the access policy).")
	case expectedHTTPErrorOnly(err, recipes.ErrInvalidInput):
		writeError(w, r, http.StatusBadRequest, "validation_failed", "The request is not valid: name (lowercase letters, digits, -), format toml or json, token_id and a 32-byte auth.")
	case expectedHTTPErrorOnly(err, recipes.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "No such recipe in this workspace.")
	case expectedHTTPErrorOnly(err, recipes.ErrVaultNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "No such vault in this workspace.")
	case expectedHTTPErrorOnly(err, recipes.ErrVaultTaken):
		writeError(w, r, http.StatusConflict, "vault_name_taken", "A vault with this name already exists in this workspace.")
	case expectedHTTPErrorOnly(err, recipes.ErrVaultStale):
		writeError(w, r, http.StatusConflict, "version_conflict", "The vault changed since you read it. Read it again and retry.")
	case expectedHTTPErrorOnly(err, recipes.ErrVaultRekeyRequired):
		writeError(w, r, http.StatusConflict, "rotation_required", "A recipe that could read this vault was revoked. Give the vault a new key (pb env vault rekey) before writing values.")
	case errors.Is(err, recipes.ErrVaultInUse):
		writeError(w, r, http.StatusConflict, "vault_in_use", "A recipe reads this vault. Remove it from the recipe (pb recipe update) first.")
	case expectedHTTPErrorOnly(err, workspace.ErrInvalid), expectedHTTPErrorOnly(err, workspace.ErrForbidden):
		writeWorkspaceError(w, r, err)
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Recipes are temporarily unavailable. Please retry.", err)
	}
}

// recipeSessionCreate exchanges a recipe token's id and auth for a CLI session of the recipe's
// creator carrying the recipe: the first step of pairing a machine from a recipe.
func recipeSessionCreate(machines *auth.DeviceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		var in struct {
			TokenID     string `json:"token_id"`
			Auth        []byte `json:"auth"`
			ClientLabel string `json:"client_label"`
			DeviceType  string `json:"device_type"`
			OS          string `json:"os"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		out, err := machines.StartRecipeSession(r.Context(), auth.RecipeSessionRequest{TokenID: in.TokenID, Auth: in.Auth, Label: in.ClientLabel, DeviceType: in.DeviceType, OS: in.OS})
		if writeSDKLimitError(w, r, err) {
			return
		}
		var refusal *auth.RecipeRefusal
		if errors.As(err, &refusal) {
			details := map[string]any{"recipe": refusal.Name}
			if !refusal.At.IsZero() {
				details["at"] = refusal.At
			}
			if refusal.Max > 0 {
				details["max_machines"] = refusal.Max
			}
			status := http.StatusUnauthorized
			if refusal.Code == "recipe_full" {
				status = http.StatusConflict
			}
			writeErrorDetails(w, r, status, refusal.Code, recipeRefusalText(refusal), details)
			return
		}
		if writeDeviceError(w, r, err) {
			return
		}
		t := out.Tokens
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"access_token": t.AccessToken, "refresh_token": t.RefreshToken, "token_type": t.TokenType,
			"expires_in": t.ExpiresIn, "scope": t.Scope, "cli_client_session_id": t.CLIClientSessionID,
			"recipe": map[string]any{"id": out.RecipeID, "name": out.Name, "revision": out.Revision, "workspace": out.Workspace.ID, "recipe": out.Recipe}}})
	}
}

func recipeRefusalText(e *auth.RecipeRefusal) string {
	switch e.Code {
	case "recipe_expired":
		return "This recipe token expired. Get a new token with pb recipe rotate " + e.Name + " (its machines keep working), then update PB_RECIPE."
	case "recipe_replaced":
		return "This recipe token was replaced (pb recipe rotate " + e.Name + "). Use the new token; update PB_RECIPE where this one is stored."
	case "recipe_full":
		return "Recipe " + e.Name + " has reached its max_machines. A machine leaves the recipe when it is removed; to allow more, raise max_machines and run pb recipe update."
	default:
		return "This recipe token is not valid: the recipe was revoked or the token was mistyped."
	}
}

// recipeMachine returns what a recipe machine applies: the recipe, its vaults (key wrapped to the
// recipe) and, after a rotate, the recipe's new key sealed to this machine.
func recipeMachine(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil || p.Client.RecipeID == "" {
			writeError(w, r, http.StatusForbidden, "recipe_session_required", "Only a machine paired from a recipe reads this.")
			return
		}
		state, err := service.MachineRecipe(r.Context(), p.Client.RecipeID, p.Client.MachineID)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: state})
	}
}

// recipeMachineApply records what the machine applied, per section, for pb recipe show and doctor.
func recipeMachineApply(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil || p.Client.RecipeID == "" || p.Client.MachineID == "" {
			writeError(w, r, http.StatusForbidden, "recipe_session_required", "Only a paired machine from a recipe reports this.")
			return
		}
		limitEnvironmentJSON(w, r, 64<<10)
		var in struct {
			Revision int            `json:"revision"`
			Sections map[string]any `json:"sections"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		if err := service.RecordApply(r.Context(), p.Client.RecipeID, p.Client.MachineID, in.Revision, in.Sections); err != nil {
			writeRecipeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func registerRecipeRoutes(mux *http.ServeMux, service *recipes.Service, account func(http.Handler) http.Handler, csrf func(http.Handler) http.Handler, device *auth.DeviceService) {
	mux.Handle("POST /v1/workspaces/{ws}/recipes", account(csrf(recipeCreate(service))))
	mux.Handle("GET /v1/workspaces/{ws}/recipes", account(recipeList(service)))
	mux.Handle("GET /v1/workspaces/{ws}/recipes/{name}", account(recipeGet(service)))
	mux.Handle("PUT /v1/workspaces/{ws}/recipes/{name}", account(csrf(recipeUpdate(service))))
	mux.Handle("POST /v1/workspaces/{ws}/recipes/{name}/rotate", account(csrf(recipeRotate(service))))
	mux.Handle("POST /v1/workspaces/{ws}/recipes/{name}/revoke", account(csrf(recipeRevoke(service))))
	mux.Handle("GET /v1/workspaces/{ws}/env-vaults", account(vaultList(service)))
	mux.Handle("POST /v1/workspaces/{ws}/env-vaults", account(csrf(vaultCreate(service))))
	mux.Handle("GET /v1/workspaces/{ws}/env-vaults/{name}", account(vaultGet(service)))
	mux.Handle("PUT /v1/workspaces/{ws}/env-vaults/{name}/values", account(csrf(vaultPutValues(service))))
	mux.Handle("POST /v1/workspaces/{ws}/env-vaults/{name}/rekey", account(csrf(vaultRekey(service))))
	mux.Handle("DELETE /v1/workspaces/{ws}/env-vaults/{name}", account(csrf(vaultDelete(service))))
	if device != nil {
		mux.Handle("POST /v1/auth/recipe-sessions", recipeSessionCreate(device))
		mux.Handle("GET /v1/recipe-machine", requireBearerAuth(device, recipeMachine(service)))
		mux.Handle("PUT /v1/recipe-machine/apply", requireBearerAuth(device, recipeMachineApply(service)))
	}
}
