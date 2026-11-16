package httpapi

import (
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/recipes"
)

// Named ENV vaults (PLAN F26): keys are made and wrapped on members' computers; the server keeps
// ciphertext and wraps only.

func vaultList(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		out, err := service.ListVaults(r.Context(), account, ws)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		if out == nil {
			out = []recipes.Vault{}
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"vaults": out}})
	}
}

func vaultCreate(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 64<<10)
		var in struct {
			Name  string         `json:"name"`
			Wraps []recipes.Wrap `json:"wraps"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		out, err := service.CreateVault(r.Context(), account, ws, in.Name, in.Wraps)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: map[string]any{"vault": out}})
	}
}

func vaultGet(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		out, err := service.GetVault(r.Context(), account, ws, r.PathValue("name"))
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"vault": out}})
	}
}

func vaultPutValues(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 2<<20)
		var in struct {
			ExpectedRevision int64  `json:"expected_revision"`
			KeyEpoch         int    `json:"key_epoch"`
			Envelope         []byte `json:"envelope"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		out, err := service.PutValues(r.Context(), account, ws, r.PathValue("name"), in.ExpectedRevision, in.KeyEpoch, in.Envelope)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"vault": out}})
	}
}

func vaultRekey(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 2<<20)
		var in struct {
			ExpectedKeyEpoch int                  `json:"expected_key_epoch"`
			Envelope         []byte               `json:"envelope"`
			Wraps            []recipes.Wrap       `json:"wraps"`
			RecipeWraps      []recipes.RecipeWrap `json:"recipe_wraps"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		out, err := service.Rekey(r.Context(), account, ws, r.PathValue("name"), in.ExpectedKeyEpoch, in.Envelope, in.Wraps, in.RecipeWraps)
		if err != nil {
			writeRecipeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"vault": out}})
	}
}

func vaultDelete(service *recipes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := recipeRequest(w, r)
		if !ok {
			return
		}
		if err := service.DeleteVault(r.Context(), account, ws, r.PathValue("name")); err != nil {
			writeRecipeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
