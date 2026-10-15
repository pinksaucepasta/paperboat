package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	pbgithub "github.com/pinksaucepasta/paperboat-server/internal/github"
)

const nativeGitHubCookie = "__Host-pb-github-native"

var nativeGitHubPage = template.Must(template.New("github-connect").Funcs(template.FuncMap{
	// Only these fixed comments are trusted HTML. Account text remains escaped.
	// Cloudflare must not replace this security-relevant label with script output.
	"emailOff": func() template.HTML { return "<!--email_off-->" },
	"emailOn":  func() template.HTML { return "<!--/email_off-->" },
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Connect GitHub to Paperboat</title></head><body><h1>Connect GitHub</h1><p>You are connecting GitHub to Paperboat account {{emailOff}}<strong>{{.Email}}</strong>{{emailOn}}.</p><p>Continue only if you started this request from your Paperboat CLI. GitHub will ask you to authorize repository access. After connecting, choose an accessible private repository for configuration sync. Selected configuration is ordinary plaintext in private Git, including its history.</p><form method="post" action="{{.Action}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button name="action" value="continue">Continue to GitHub</button><button name="action" value="cancel">Cancel</button></form></body></html>`))

func githubNativeHeaders(w http.ResponseWriter, service *pbgithub.Service) {
	formAction := "'self'"
	if authorize, err := service.OAuthAuthorizeURL("browser-policy", service.DefaultCallbackURL()); err == nil {
		if provider, err := url.Parse(authorize); err == nil && provider.Scheme == "https" {
			formAction += " " + provider.Scheme + "://" + provider.Host
		}
	}
	noStore(w)
	// Preserve Origin on HTML form POST while withholding cross-origin referrers.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action "+formAction+"; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
func githubNativeStart(service *pbgithub.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, 401, "unauthenticated", "Native client authentication is required.")
			return
		}
		item, err := service.StartNativeLink(r.Context(), p.User.ID, p.Client.SessionID)
		if err != nil {
			if expectedHTTPErrorOnly(err, pbgithub.ErrNativeLink) {
				logHTTPRejection(r, http.StatusForbidden, "forbidden", err)
				writeError(w, r, http.StatusForbidden, "forbidden", "Reconnect this Paperboat machine session before starting a GitHub connection.")
				return
			}
			writeError(w, r, 503, "github_connection_unavailable", "GitHub connection could not be started.", err)
			return
		}
		noStore(w)
		writeJSON(w, 201, SuccessResponse{Data: item})
	}
}
func githubNativeStatus(service *pbgithub.Service, cancel bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, 401, "unauthenticated", "Native client authentication is required.")
			return
		}
		item, err := service.NativeLinkStatus(r.Context(), p.User.ID, p.Client.SessionID, r.PathValue("link_id"), cancel)
		if err != nil {
			if expectedHTTPErrorOnly(err, pbgithub.ErrNativeLink) {
				logHTTPRejection(r, 404, "not_found_or_forbidden", err)
				writeError(w, r, 404, "not_found_or_forbidden", "GitHub connection request is unavailable.")
				return
			}
			writeError(w, r, 503, "github_connection_unavailable", "GitHub connection status is temporarily unavailable.", err)
			return
		}
		noStore(w)
		writeJSON(w, 200, SuccessResponse{Data: item})
	}
}
func githubNativeLaunch(service *pbgithub.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		githubNativeHeaders(w, service)
		if r.Method == http.MethodGet {
			var random [32]byte
			if _, err := rand.Read(random[:]); err != nil {
				http.Error(w, "Unable to start GitHub connection.", 500)
				return
			}
			browser := base64.RawURLEncoding.EncodeToString(random[:])
			email, err := service.LaunchNativeLink(r.Context(), r.PathValue("link_id"), r.URL.Query().Get("token"), browser)
			if err != nil {
				writeGitHubNativeBrowserFailure(w, r, err, "This GitHub connection request is expired or unavailable. Start again in Paperboat.", false)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: nativeGitHubCookie, Value: browser, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: int(pbgithub.NativeLinkLifetime / time.Second)})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = nativeGitHubPage.Execute(w, struct{ Email, CSRF, Action string }{email, browser, r.URL.Path})
			return
		}
		origin := service.NativeBrowserOrigin()
		if origin == "" || r.Header.Get("Origin") != origin {
			http.Error(w, "GitHub connection origin was not verified.", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid request.", http.StatusBadRequest)
			return
		}
		cookie, err := nativeGitHubBrowserCookie(r)
		if err != nil || !hmac.Equal([]byte(cookie), []byte(r.PostForm.Get("csrf"))) {
			http.Error(w, "GitHub connection browser was not verified.", http.StatusForbidden)
			return
		}
		action := r.PostForm.Get("action")
		if action != "continue" && action != "cancel" {
			http.Error(w, "Invalid action.", http.StatusBadRequest)
			return
		}
		redirect, err := service.AuthorizeNativeLink(r.Context(), r.PathValue("link_id"), cookie, action == "cancel")
		if err != nil {
			writeGitHubNativeBrowserFailure(w, r, err, "This GitHub connection request is expired or unavailable. Start again in Paperboat.", false)
			return
		}
		if action == "cancel" {
			clearNativeGitHubCookie(w)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("GitHub connection canceled. Return to Paperboat."))
			return
		}
		http.Redirect(w, r, redirect, http.StatusSeeOther)
	}
}
func writeGitHubNativeBrowserFailure(w http.ResponseWriter, r *http.Request, err error, rejectionMessage string, providerCallback bool) {
	rejectionCode := ""
	switch {
	case expectedHTTPErrorOnly(err, pbgithub.ErrNativeLink):
		rejectionCode = "github_connection_unavailable"
	case providerCallback && expectedHTTPErrorOnly(err, pbgithub.ErrMissingScopes):
		rejectionCode = "github_scope_denied"
	case providerCallback && expectedHTTPErrorOnly(err, pbgithub.ErrIdentityLinkedToAnotherUser):
		rejectionCode = "github_identity_conflict"
	case providerCallback && githubOAuthCredentialRejected(err):
		rejectionCode = "github_oauth_failed"
	}
	if rejectionCode != "" {
		logHTTPRejection(r, http.StatusBadRequest, rejectionCode, err)
		http.Error(w, rejectionMessage, http.StatusBadRequest)
		return
	}
	captureSupportFailure(r, http.StatusServiceUnavailable, "github_connection_unavailable", sentry.NewStacktrace(), err)
	http.Error(w, "GitHub connection is temporarily unavailable. Return to Paperboat and retry.", http.StatusServiceUnavailable)
}

func nativeGitHubBrowserCookie(r *http.Request) (string, error) {
	var value string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == nativeGitHubCookie {
			value = c.Value
			count++
		}
	}
	if count != 1 || len(value) != 43 {
		return "", pbgithub.ErrNativeLink
	}
	return value, nil
}
func clearNativeGitHubCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: nativeGitHubCookie, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}
func githubCallbackDispatch(service *pbgithub.Service, browser http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if !strings.HasPrefix(state, "pbnative_") {
			browser.ServeHTTP(w, r)
			return
		}
		githubNativeHeaders(w, service)
		binding, err := nativeGitHubBrowserCookie(r)
		if err != nil {
			http.Error(w, "GitHub connection browser was not verified. Return to Paperboat and retry.", http.StatusForbidden)
			return
		}
		if err := service.CompleteNativeLink(r.Context(), state, binding, r.URL.Query().Get("code")); err != nil {
			writeGitHubNativeBrowserFailure(w, r, err, "GitHub connection did not complete. Return to Paperboat and retry.", true)
			return
		}
		clearNativeGitHubCookie(w)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("GitHub connected. Return to Paperboat to select your configuration repository."))
	}
}
