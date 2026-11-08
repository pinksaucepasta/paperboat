package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
)

// An unknown or malformed email must not answer differently from an operator's: no account oracle.
func TestOperatorEmailDoesNotRevealWhetherTheAccountExists(t *testing.T) {
	mux := http.NewServeMux()
	registerOperatorAuthentication(mux, Options{OperatorAuth: &operatorauth.Service{}})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/operator/email", strings.NewReader(`{"email":"nobody"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next":"password"`) {
		t.Fatalf("unknown email: %d %s", rec.Code, rec.Body.String())
	}
}

// Every session cookie is Secure whatever the public URL scheme (the guest terminal cookie too).
func TestOperatorCookiesAreAlwaysSecure(t *testing.T) {
	rec := httptest.NewRecorder()
	setOperatorCookie(rec, operatorauth.Session{Token: "t", ExpiresAt: time.Now().Add(time.Hour)})
	clearOperatorCookie(rec)
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 || !cookies[0].Secure || !cookies[1].Secure {
		t.Fatalf("cookies: %+v", cookies)
	}
}
