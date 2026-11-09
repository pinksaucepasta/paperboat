package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
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

// A well-formed address with no operator account takes the real database lookup and still gets "password".
func TestOperatorEmailWellFormedUnknownAddressAnswersPassword(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	service := &operatorauth.Service{Store: store}
	if _, err = service.Discover(context.Background(), "unknown@example.test"); err == nil {
		t.Fatal("the service must refuse the unknown address; the handler hides that")
	}
	mux := http.NewServeMux()
	registerOperatorAuthentication(mux, Options{OperatorAuth: service})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/operator/email", strings.NewReader(`{"email":"unknown@example.test"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next":"password"`) {
		t.Fatalf("well-formed unknown email: %d %s", rec.Code, rec.Body.String())
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
