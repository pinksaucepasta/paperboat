package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorauth"
)

const operatorSessionCookie = "pb-operator-session"

func operatorCookie(r *http.Request) string {
	cookie, err := r.Cookie(operatorSessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}
func setOperatorCookie(w http.ResponseWriter, secure bool, session operatorauth.Session) {
	http.SetCookie(w, &http.Cookie{Name: operatorSessionCookie, Value: session.Token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, Expires: session.ExpiresAt})
}
func clearOperatorCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: operatorSessionCookie, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
func operatorOriginAllowed(r *http.Request, opts Options) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	} // non-browser API clients supply CSRF for authenticated writes
	for _, allowed := range opts.Config.HTTP.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return origin == opts.Config.HTTP.BrowserLoginOrigin
}
func operatorCredentialRequest(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || strings.TrimSpace(body.Email) == "" {
		writeError(w, r, 400, "invalid_request", "Valid email and password are required.")
		return "", "", false
	}
	return body.Email, body.Password, true
}
func registerOperatorAuthentication(mux *http.ServeMux, opts Options) {
	service := opts.OperatorAuth
	if service == nil {
		return
	}
	mux.HandleFunc("POST /v1/operator/email", func(w http.ResponseWriter, r *http.Request) {
		if !operatorOriginAllowed(r, opts) {
			writeError(w, r, 403, "origin_failed", "Origin is not allowed.")
			return
		}
		email, _, ok := operatorCredentialRequest(w, r)
		if !ok {
			return
		}
		step, err := service.Discover(r.Context(), email)
		if expectedHTTPErrorOnly(err, operatorauth.ErrInvalid) {
			writeError(w, r, 404, "operator_not_found", "Operator email was not found.")
			return
		}
		if err != nil {
			writeError(w, r, 503, "operator_unavailable", "Operator authentication is unavailable.", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"next": step}})
	})
	finish := func(w http.ResponseWriter, r *http.Request, session operatorauth.Session) {
		setOperatorCookie(w, strings.HasPrefix(opts.Config.HTTP.PublicBaseURL, "https://"), session)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"email": session.Account.Email, "role": session.Account.Role, "csrf_token": session.CSRF}})
	}
	mux.HandleFunc("POST /v1/operator/setup", func(w http.ResponseWriter, r *http.Request) {
		if !operatorOriginAllowed(r, opts) {
			writeError(w, r, 403, "origin_failed", "Origin is not allowed.")
			return
		}
		email, password, ok := operatorCredentialRequest(w, r)
		if !ok {
			return
		}
		session, err := service.Setup(r.Context(), email, password)
		if expectedHTTPErrorOnly(err, operatorauth.ErrBootstrapConsumed) {
			writeError(w, r, 409, "setup_complete", "The first operator is already set up.")
			return
		}
		if expectedHTTPErrorOnly(err, operatorauth.ErrInvalid) {
			writeError(w, r, 400, "invalid_request", "Operator setup was rejected.")
			return
		}
		if err != nil {
			writeError(w, r, 503, "operator_unavailable", "Operator setup is unavailable.", err)
			return
		}
		finish(w, r, session)
	})
	mux.HandleFunc("POST /v1/operator/login", func(w http.ResponseWriter, r *http.Request) {
		if !operatorOriginAllowed(r, opts) {
			writeError(w, r, 403, "origin_failed", "Origin is not allowed.")
			return
		}
		email, password, ok := operatorCredentialRequest(w, r)
		if !ok {
			return
		}
		session, err := service.Login(r.Context(), email, password)
		if expectedHTTPErrorOnly(err, operatorauth.ErrInvalid) || expectedHTTPErrorOnly(err, operatorauth.ErrLocked) {
			writeError(w, r, 401, "invalid_credentials", "Operator sign-in failed.")
			return
		}
		if err != nil {
			writeError(w, r, 503, "operator_unavailable", "Operator sign-in is unavailable.", err)
			return
		}
		finish(w, r, session)
	})
	mux.HandleFunc("GET /v1/operator/me", func(w http.ResponseWriter, r *http.Request) {
		account, err := service.Authenticate(r.Context(), operatorCookie(r), "", false)
		if expectedHTTPErrorOnly(err, operatorauth.ErrInvalid) {
			writeError(w, r, 401, "unauthenticated", "Operator sign-in is required.")
			return
		}
		if err != nil {
			writeError(w, r, 503, "operator_unavailable", "Operator authentication is unavailable.", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"email": account.Email, "role": account.Role}})
	})
	mux.HandleFunc("POST /v1/operator/logout", func(w http.ResponseWriter, r *http.Request) {
		if !operatorOriginAllowed(r, opts) {
			writeError(w, r, 403, "origin_failed", "Origin is not allowed.")
			return
		}
		if _, err := service.Authenticate(r.Context(), operatorCookie(r), r.Header.Get("X-CSRF-Token"), true); expectedHTTPErrorOnly(err, operatorauth.ErrInvalid) {
			writeError(w, r, 403, "forbidden", "Operator session or CSRF token is invalid.")
			return
		} else if err != nil {
			writeError(w, r, 503, "operator_unavailable", "Operator authentication is unavailable.", err)
			return
		}
		if err := service.Logout(r.Context(), operatorCookie(r)); err != nil {
			writeError(w, r, 503, "operator_unavailable", "Operator sign-out is unavailable.", err)
			return
		}
		clearOperatorCookie(w, strings.HasPrefix(opts.Config.HTTP.PublicBaseURL, "https://"))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"status": "signed_out"}})
	})
}
func requireOperator(opts Options, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.OperatorAuth == nil {
			writeError(w, r, 503, "operator_unavailable", "Operator authentication is unavailable.")
			return
		}
		if !operatorOriginAllowed(r, opts) {
			writeError(w, r, 403, "origin_failed", "Origin is not allowed.")
			return
		}
		unsafe := unsafeMethod(r.Method)
		account, err := opts.OperatorAuth.Authenticate(r.Context(), operatorCookie(r), r.Header.Get("X-CSRF-Token"), unsafe)
		if expectedHTTPErrorOnly(err, operatorauth.ErrInvalid) {
			writeError(w, r, 403, "forbidden", "Operator sign-in is required.")
			return
		}
		if err != nil {
			writeError(w, r, 503, "operator_unavailable", "Operator authentication is unavailable.", err)
			return
		}
		p := principal{User: auth.User{ID: account.ID, PrimaryEmail: account.Email, Role: auth.Role(account.Role), Status: "active"}, Session: auth.Session{ID: account.ID, UserID: account.ID, OperatorAuthenticatedAt: account.AuthenticatedAt, ExpiresAt: time.Now().Add(time.Hour)}}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, p)))
	})
}
