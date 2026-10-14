package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
)

type authContextKey struct{}

type principal struct {
	User    auth.User
	Session auth.Session
	Client  *auth.ClientPrincipal
}

func workOSState(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := service.NewOAuthState(r.URL.Query().Get("return_to"))
		if err != nil {
			if errors.Is(err, auth.ErrOAuthState) {
				writeError(w, r, http.StatusBadRequest, "invalid_return_to", "Return destination must be a dashboard path.")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		service.SetOAuthStateCookie(w, state)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"state": state}})
	}
}

func workOSCallback(service *auth.Service) http.HandlerFunc {
	type request struct {
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
		State       string `json:"state"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if err := service.ValidateBrowserRequest(r); err != nil {
			writeError(w, r, http.StatusForbidden, "origin_failed", "Browser origin validation failed.")
			return
		}
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		if err := service.ValidateOAuthState(r, body.State); err != nil {
			writeError(w, r, http.StatusForbidden, "oauth_state_failed", "OAuth state validation failed.")
			return
		}
		returnTo, err := service.OAuthReturnTo(body.State)
		if err != nil {
			writeError(w, r, http.StatusForbidden, "oauth_state_failed", "OAuth state validation failed.")
			return
		}
		user, session, err := service.VerifyCallback(r.Context(), auth.CallbackInput{
			Code:        body.Code,
			RedirectURI: body.RedirectURI,
			State:       body.State,
		})
		if err != nil {
			writeWorkOSCallbackFailure(w, r, err)
			return
		}
		service.ClearOAuthStateCookie(w)
		service.SetSessionCookies(w, session)
		payload := mePayload(user)
		payload["return_to"] = returnTo
		writeJSON(w, http.StatusOK, SuccessResponse{Data: payload})
	}
}

func writeWorkOSCallbackFailure(w http.ResponseWriter, r *http.Request, err error) {
	var verification *auth.WorkOSVerificationError
	if errors.Is(err, auth.ErrWorkOSImpersonation) || errors.As(err, &verification) && verification.HTTPStatus == http.StatusBadRequest {
		logHTTPRejection(r, http.StatusUnauthorized, "auth_callback_failed", err)
		writeError(w, r, http.StatusUnauthorized, "auth_callback_failed", "Authentication callback could not be verified.", err)
		return
	}
	if errors.As(err, &verification) {
		writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Authentication provider is temporarily unavailable. Please retry.", err)
		return
	}
	writeError(w, r, http.StatusInternalServerError, "auth_callback_failed", "Authentication callback could not be completed.", err)
}

func logout(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := service.ValidateBrowserRequest(r); err != nil {
			writeError(w, r, http.StatusForbidden, "origin_failed", "Browser origin validation failed.")
			return
		}
		if unsafeMethod(r.Method) {
			if err := service.ValidateCSRF(r.Context(), r); err != nil {
				writeCSRFValidationError(w, r, err)
				return
			}
		}
		logoutURL, err := service.Logout(r.Context(), r)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		service.ClearSessionCookies(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"status": "logged_out", "logout_url": logoutURL}})
	}
}

func csrf(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := service.CSRFToken(r)
		if !ok {
			p, principalOK := principalFromContext(r.Context())
			if !principalOK {
				writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
				return
			}
			refreshed, err := service.RefreshCSRF(r.Context(), p.Session)
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
				return
			}
			service.SetCSRFCookie(w, refreshed, p.Session.ExpiresAt)
			token = refreshed
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"csrf_token": token}})
	}
}

func me(_ *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: mePayload(p.User)})
	}
}

func mePayload(user auth.User) map[string]any {
	return map[string]any{
		"id":             user.ID,
		"email":          user.PrimaryEmail,
		"display_name":   user.DisplayName,
		"status":         user.Status,
		"role":           string(user.Role),
		"workos_subject": user.WorkOSSubject,
	}
}

func requireAuth(service *auth.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unsafeMethod(r.Method) {
			if err := service.ValidateBrowserRequest(r); err != nil {
				writeError(w, r, http.StatusForbidden, "origin_failed", "Browser origin validation failed.")
				return
			}
		}
		user, session, err := service.AuthenticateRequest(r.Context(), r)
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if errors.Is(err, auth.ErrWorkOSTemporarilyUnavailable) {
			writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Authentication provider is temporarily unavailable. Please retry.", err)
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		if shouldRotateForRequest(r) && service.ShouldRotate(session) {
			rotated, err := service.RotateSession(r.Context(), session)
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
				return
			}
			session = rotated
			service.SetSessionCookies(w, session)
		}
		bindSupportOwner(r.Context(), user.ID)
		ctx := context.WithValue(r.Context(), authContextKey{}, principal{User: user, Session: session})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requireAnyAuth(service *auth.Service, machines *auth.DeviceService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r); ok {
			client, err := machines.Authenticate(r.Context(), token)
			if err != nil {
				writeDeviceAuthenticationError(w, r, err)
				return
			}
			bindSupportOwner(r.Context(), client.User.ID)
			ctx := context.WithValue(r.Context(), authContextKey{}, principal{User: client.User, Client: &client})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		requireAuth(service, next).ServeHTTP(w, r)
	})
}

func requireBearerAuth(machines *auth.DeviceService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		client, err := machines.Authenticate(r.Context(), token)
		if err != nil {
			writeDeviceAuthenticationError(w, r, err)
			return
		}
		bindSupportOwner(r.Context(), client.User.ID)
		ctx := context.WithValue(r.Context(), authContextKey{}, principal{User: client.User, Client: &client})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeDeviceAuthenticationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "Client authentication is temporarily unavailable. Please retry.", err)
}

func requireScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if p.Client != nil && !p.Client.HasScope(scope) {
			writeError(w, r, http.StatusForbidden, "insufficient_scope", "The client is not authorized for this operation.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func shouldRotateForRequest(r *http.Request) bool {
	return !unsafeMethod(r.Method) && r.URL.Path != "/v1/auth/csrf"
}

func requireCSRF(service *auth.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principalFromContext(r.Context()); ok && p.Client != nil {
			next.ServeHTTP(w, r)
			return
		}
		if unsafeMethod(r.Method) {
			if err := service.ValidateCSRF(r.Context(), r); err != nil {
				writeCSRFValidationError(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeCSRFValidationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrCSRF) {
		writeError(w, r, http.StatusForbidden, "csrf_failed", "CSRF validation failed.")
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "CSRF verification is temporarily unavailable. Please retry.", err)
}

func bearerToken(r *http.Request) (string, bool) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func requireEntitlement(service *auth.Service, next http.Handler) http.Handler {
	return requireCSRF(service, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		ok, err := service.HasActiveEntitlement(r.Context(), p.User.ID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		if !ok {
			paymentRequired(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func paymentRequired(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusPaymentRequired, "payment_required", "An active subscription or trial is required to use this feature.")
}

func principalFromContext(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(authContextKey{}).(principal)
	return p, ok
}

func unsafeMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
