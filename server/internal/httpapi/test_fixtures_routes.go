package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/testfixtures"
)

// registerTestFixtureRoutes mounts the final re-check runner's scoped surface (package testfixtures) behind its one
// dedicated bearer token. Nothing is mounted unless a token is configured.
func registerTestFixtureRoutes(mux *http.ServeMux, opts Options) {
	service := opts.TestFixtures
	if service == nil {
		return
	}
	guard := func(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if !service.Authenticate(nodeauth.Bearer(r)) {
				writeError(w, r, http.StatusUnauthorized, "test_fixtures_unauthorized", "A dedicated test fixtures credential is required.")
				return
			}
			if allowed, wait := service.Allow(); !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Test fixtures are requested too often. Retry after the delay.")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			next(w, r)
		}
	}
	fail := func(w http.ResponseWriter, r *http.Request, err error) {
		switch {
		case errors.Is(err, testfixtures.ErrInvalid):
			writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error())
		case errors.Is(err, testfixtures.ErrAccountNotFound):
			writeError(w, r, http.StatusNotFound, "account_not_found", "The account has not signed in yet; sign in once, then retry.")
		case errors.Is(err, testfixtures.ErrRecordNotFound):
			writeError(w, r, http.StatusNotFound, "record_not_found", "No such test DNS record.")
		case errors.Is(err, testfixtures.ErrNotConfigured):
			writeError(w, r, http.StatusServiceUnavailable, "test_dns_not_configured", "Test DNS is not configured on this server.")
		default:
			writeError(w, r, http.StatusServiceUnavailable, "test_fixtures_unavailable", "The test fixture operation failed; retry shortly.", err)
		}
	}
	mux.HandleFunc("POST /v1/test-fixtures/team", guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email   string   `json:"email"`
			Members []string `json:"members"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		team, err := service.CreateTeam(r.Context(), in.Email, in.Members)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"env": map[string]string{"PAPERBOAT_SMOKE_TEAM": team}}})
	}))
	mux.HandleFunc("DELETE /v1/test-fixtures/team", guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email string `json:"email"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		active, err := service.RemoveTeam(r.Context(), in.Email)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"team": testfixtures.TeamName(in.Email), "active_members": active}})
	}))
	mux.HandleFunc("GET /v1/test-fixtures/zone", guard(func(w http.ResponseWriter, r *http.Request) {
		zone, err := service.Zone()
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]string{"zone": zone}})
	}))
	mux.HandleFunc("POST /v1/test-fixtures/dns", guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		record, err := service.CreateRecord(r.Context(), in.Type, in.Name, in.Content)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]string{"id": record.ID, "name": record.Name}})
	}))
	mux.HandleFunc("GET /v1/test-fixtures/dns/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		record, err := service.Record(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]string{"id": record.ID, "name": record.Name, "type": record.Type, "content": record.Content}})
	}))
	mux.HandleFunc("DELETE /v1/test-fixtures/dns/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		if err := service.DeleteRecord(r.Context(), r.PathValue("id")); err != nil && !errors.Is(err, testfixtures.ErrRecordNotFound) {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
