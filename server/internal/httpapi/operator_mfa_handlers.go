package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
)

// registerOperatorMFA serves the second-factor routes. Each needs the session cookie and its CSRF token, and an
// allowed origin; none needs a complete session, since they are how a pending one becomes complete.
func registerOperatorMFA(mux *http.ServeMux, opts Options) {
	service := opts.OperatorAuth
	// step runs one MFA call with the common origin, session and error handling.
	step := func(pattern string, withCode bool, run func(w http.ResponseWriter, r *http.Request, token, csrf, code string) (any, time.Time, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !operatorOriginAllowed(r, opts) {
				writeError(w, r, 403, "origin_failed", "Origin is not allowed.")
				return
			}
			code := ""
			if withCode {
				var body struct {
					Code string `json:"code"`
				}
				r.Body = http.MaxBytesReader(w, r.Body, 1024)
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if decoder.Decode(&body) != nil || body.Code == "" || len(body.Code) > 64 {
					writeError(w, r, 400, "invalid_request", "A code is required.")
					return
				}
				code = body.Code
			}
			token := operatorCookie(r)
			out, expires, err := run(w, r, token, r.Header.Get("X-CSRF-Token"), code)
			switch {
			case expectedHTTPErrorOnly(err, operatorauth.ErrInvalid):
				writeError(w, r, 403, "forbidden", "Operator session or CSRF token is invalid.")
			case expectedHTTPErrorOnly(err, operatorauth.ErrMFAInvalid):
				writeError(w, r, 401, "invalid_code", "That code was not accepted. Use the current code from your authenticator, or a recovery code.")
			case expectedHTTPErrorOnly(err, operatorauth.ErrMFAState):
				writeError(w, r, 409, "mfa_state", "That step does not apply to this session.")
			case expectedHTTPErrorOnly(err, operatorauth.ErrMFAUnavailable):
				writeError(w, r, 503, "mfa_unavailable", "Operator second factor is not available on this server.", err)
			case err != nil:
				writeError(w, r, 503, "operator_unavailable", "Operator authentication is unavailable.", err)
			default:
				if !expires.IsZero() {
					// The pending cookie expired in fifteen minutes; the complete session lasts eight hours.
					setOperatorCookie(w, operatorauth.Session{Token: token, ExpiresAt: expires})
				}
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, 200, SuccessResponse{Data: out})
			}
		})
	}
	step("POST /v1/operator/mfa/enroll", false, func(w http.ResponseWriter, r *http.Request, token, csrf, _ string) (any, time.Time, error) {
		out, err := service.EnrollStart(r.Context(), token, csrf)
		return out, time.Time{}, err
	})
	step("POST /v1/operator/mfa/enroll/confirm", true, func(w http.ResponseWriter, r *http.Request, token, csrf, code string) (any, time.Time, error) {
		codes, expires, err := service.EnrollConfirm(r.Context(), token, csrf, code)
		return map[string]any{"mfa": operatorauth.MFAVerified, "recovery_codes": codes}, expires, err
	})
	step("POST /v1/operator/mfa/verify", true, func(w http.ResponseWriter, r *http.Request, token, csrf, code string) (any, time.Time, error) {
		expires, err := service.Verify(r.Context(), token, csrf, code)
		return map[string]any{"mfa": operatorauth.MFAVerified}, expires, err
	})
	step("POST /v1/operator/mfa/recovery-codes", true, func(w http.ResponseWriter, r *http.Request, token, csrf, code string) (any, time.Time, error) {
		codes, err := service.RegenerateRecoveryCodes(r.Context(), token, csrf, code)
		return map[string]any{"recovery_codes": codes}, time.Time{}, err
	})
}
