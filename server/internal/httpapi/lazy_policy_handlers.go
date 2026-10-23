package httpapi

import (
	"context"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/lazyaccess"
)

type LazyPolicyAPI interface {
	Upsert(context.Context, string, lazyaccess.UpsertRequest) (lazyaccess.Policy, error)
	Get(context.Context, string, string) (lazyaccess.Policy, error)
	Delete(context.Context, string, string, int64) error
}

func registerLazyPolicyRoutes(mux *http.ServeMux, service LazyPolicyAPI, read, write func(http.Handler) http.Handler) {
	handler := func(action string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			noStore(w)
			principal, ok := principalFromContext(r.Context())
			if !ok || principal.User.ID == "" {
				writeError(w, r, 401, "authentication_required", "Sign in to manage private service policies.")
				return
			}
			var out any
			var err error
			switch action {
			case "upsert":
				limitEnvironmentJSON(w, r, 16<<10)
				var input lazyaccess.UpsertRequest
				if !decodeLazyPolicy(w, r, &input) {
					return
				}
				out, err = service.Upsert(r.Context(), principal.User.ID, input)
			case "get":
				out, err = service.Get(r.Context(), principal.User.ID, r.PathValue("policy_id"))
			case "delete":
				limitEnvironmentJSON(w, r, 1024)
				var input struct {
					ExpectedGeneration int64 `json:"expected_generation"`
				}
				if !decodeLazyPolicy(w, r, &input) {
					return
				}
				err = service.Delete(r.Context(), principal.User.ID, r.PathValue("policy_id"), input.ExpectedGeneration)
			}
			if err != nil {
				lazyPolicyError(w, r, err)
				return
			}
			if action == "delete" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
		})
	}
	mux.Handle("PUT /v1/lazy-policies", write(handler("upsert")))
	mux.Handle("GET /v1/lazy-policies/{policy_id}", read(handler("get")))
	mux.Handle("DELETE /v1/lazy-policies/{policy_id}", write(handler("delete")))
}

func lazyPolicyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, lazyaccess.ErrInvalid):
		writeError(w, r, 400, "invalid_policy", "Provide an exact loopback HTTP target, private or team access, an expiry and port-based ownership.")
	case expectedHTTPErrorOnly(err, lazyaccess.ErrDenied):
		writeError(w, r, 404, "not_found", "Policy or owned machine not found.")
	case expectedLazyPolicyConflict(err):
		writeError(w, r, 409, "policy_changed", "Policy or machine installation changed. Read the current state and reapprove before retrying.")
	case expectedHTTPErrorOnly(err, lazyaccess.ErrCapacity):
		w.Header().Set("Retry-After", "1")
		writeError(w, r, 429, "policy_limit", "The environment policy target limit is reached. Remove an unused policy before retrying.")
	default:
		writeError(w, r, 503, "policy_unavailable", "Policy storage is unavailable. Read the current policy before retrying a mutation.", err)
	}
}

// A known database conflict retains its native cause. Prove the whole cause
// tree before mapping it to an expected response, including rollback failures.
func expectedLazyPolicyConflict(err error) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	if err == nil {
		return false
	}
	pending := []error{err}
	found := false
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		if current == lazyaccess.ErrConflict {
			found = true
			continue
		}
		if pg, ok := current.(*pgconn.PgError); ok {
			if pg == nil || pg.Code != "23505" && pg.Code != "40001" {
				return false
			}
			continue
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 15-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		default:
			return false
		}
	}
	return found
}

func decodeLazyPolicy(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := browserBody(w, r)
	if err == nil {
		err = browserDecode(raw, v)
	}
	if err != nil {
		writeError(w, r, 400, "invalid_policy", "Policy JSON must be bounded and contain only unique supported fields.")
		return false
	}
	return true
}
