package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
)

type ClientUpdatePolicyAPI interface {
	Get(context.Context) (clientupdate.Policy, error)
	Put(context.Context, string, string, string, clientupdate.Input) (clientupdate.Policy, error)
}

func clientUpdatePolicyGet(s ClientUpdatePolicyAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, e := s.Get(r.Context())
		if e != nil {
			writeError(w, r, 503, "client_update_policy_unavailable", "Client update policy is temporarily unavailable.", e)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: p})
	}
}
func clientUpdatePolicyPut(s ClientUpdatePolicyAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input clientupdate.Input
		var wire struct {
			ExpectedRevision *int64     `json:"expected_revision"`
			MinimumVersion   *string    `json:"minimum_version"`
			Reason           string     `json:"reason"`
			EnforceAt        *time.Time `json:"enforce_at"`
		}
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 4097))
		if readErr != nil {
			if expectedHTTPErrorOnly(readErr, context.Canceled) {
				return
			}
			writeError(w, r, 503, "client_update_policy_unavailable", "The request body could not be read. Retry with the same Idempotency-Key.", readErr)
			return
		}
		if len(body) > 4096 {
			writeError(w, r, 400, "validation_failed", "Request body must match the client update policy schema.")
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&wire) != nil || decoder.Decode(&extra) != io.EOF || wire.ExpectedRevision == nil || wire.MinimumVersion == nil {
			writeError(w, r, 400, "validation_failed", "Request body must match the client update policy schema.")
			return
		}
		input = clientupdate.Input{ExpectedRevision: *wire.ExpectedRevision, MinimumVersion: *wire.MinimumVersion, Reason: wire.Reason, EnforceAt: wire.EnforceAt}
		p, _ := principalFromContext(r.Context())
		out, e := s.Put(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"), r.Header.Get("Operator-Reason"), input)
		switch {
		case expectedHTTPErrorOnly(e, clientupdate.ErrFloorUnavailable):
			writeError(w, r, 400, "validation_failed", "The requested global floor is not available on all published platforms. Publish a candidate at or above this version for every supported platform before setting the floor.")
			return
		case expectedHTTPErrorOnly(e, clientupdate.ErrInvalid):
			writeError(w, r, 400, "validation_failed", "Use a valid date version available on every published platform, a reason of at most 256 bytes, and an expected revision. Clearing requires an empty reason and no enforcement date.")
			return
		case expectedHTTPErrorOnly(e, clientupdate.ErrConflict):
			writeError(w, r, 409, "revision_conflict", "The client update policy changed. Read it again before applying a new change.")
			return
		case expectedHTTPErrorOnly(e, clientupdate.ErrIdempotency):
			writeError(w, r, 409, "idempotency_conflict", "This idempotency key was already used for a different change.")
			return
		case expectedHTTPErrorOnly(e, clientupdate.ErrReleaseUnavailable):
			writeError(w, r, 503, "release_unavailable", "The published release is unavailable. Clearing the floor is still available.", e)
			return
		case e != nil:
			writeError(w, r, 503, "client_update_policy_unavailable", "The policy could not be updated. Retry with the same Idempotency-Key.", e)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	}
}

// Escape routes preserve authentication, updates, diagnosis, machine runtime,
// and cleanup. A floor affects new user feature requests, never live channels.
func clientUpdatePolicyEscape(r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/v1/") || r.Method == http.MethodDelete || r.Method == http.MethodOptions {
		return true
	}
	for _, prefix := range []string{"/v1/auth/", "/v1/operator/", "/v1/admin/", "/v1/releases/", "/v1/updates/", "/v1/diagnostic-upload-intents", "/v1/support/", "/v1/runtime/", "/v1/edge/", "/v1/helper-"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	switch p {
	case "/v1/client/update-policy", "/v1/client-configuration", "/v1/runtime-observations":
		return true
	}
	last := p[strings.LastIndex(p, "/")+1:]
	if r.Method == http.MethodPost {
		switch last {
		case "cancel", "stop", "revoke", "disconnect", "release", "heartbeat":
			return true
		}
	}
	if r.Method == http.MethodGet && (last == "update-status" || last == "update-summary") {
		return true
	}
	return false
}
func clientUpdatePolicyGuard(s ClientUpdatePolicyAPI, now func() time.Time, next http.Handler) http.Handler {
	if s == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// These self-reported headers identify the CLI distribution; they are
		// compatibility hints, not authentication. Only official builds receive
		// the minimum-version gate. Custom builds must not read policy at all.
		if r.Header.Get("X-Paperboat-Client") != "paperboat" ||
			r.Header.Get("X-Paperboat-Distribution") != "official" ||
			clientUpdatePolicyEscape(r) {
			next.ServeHTTP(w, r)
			return
		}
		p, e := s.Get(r.Context())
		if e != nil {
			writeError(w, r, 503, "client_update_policy_unavailable", "Client update policy is temporarily unavailable. Authentication, updates, diagnostics and cleanup remain available.", e)
			return
		}
		version, ok := strings.CutPrefix(r.UserAgent(), "paperboat/")
		if !ok {
			version = ""
		}
		if p.RequiresUpdate(version, now()) {
			w.Header().Set("Cache-Control", "no-store")
			writeErrorDetails(w, r, http.StatusUpgradeRequired, "update_required", "This Paperboat CLI version is no longer supported. Run pb update before using this feature.", map[string]any{"minimum_version": p.MinimumVersion, "reason": p.Reason})
			return
		}
		next.ServeHTTP(w, r)
	})
}
