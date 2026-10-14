package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestSupportConnectedReferenceAndRoleRevocation(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	ref := supportreport.NewReference()
	owner := "usr_" + ref[3:]
	token := supportreport.NewReference()
	hash := sha256.Sum256([]byte(token))
	if _, err = database.Pool().Exec(ctx, `INSERT INTO users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$2,'active','support')`, owner, owner+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Pool().Exec(ctx, `INSERT INTO sessions(id,user_id,session_hash,csrf_hash,expires_at,operator_authenticated_at) VALUES($1,$1,$2,'test',now()+interval '7 days',now())`, owner, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	defer func() {
		database.Pool().Exec(ctx, `DELETE FROM support_reports WHERE support_reference=$1`, ref)
		database.Pool().Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id=$1`, owner)
		database.Pool().Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, owner)
		database.Pool().Exec(ctx, `DELETE FROM users WHERE id=$1`, owner)
	}()
	store, _ := supportreport.NewSQLStore(database)
	service, _ := supportreport.New(store, supportreport.Config{})
	runctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { service.Run(runctx); close(done) }()
	defer func() { cancel(); <-done }()
	accounts := auth.NewService(database, audit.NewWriter(database), nil, []string{"test-key"}, false)
	cfg := config.Default()
	cfg.HTTP.RequestTimeout = time.Second
	failure := NewRouter(Options{Config: cfg, SupportReports: service, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OverrideHandler: requireAuth(accounts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, 500, "synthetic_failure", "The operation could not finish. Contact support with this reference.")
	}))})
	request := httptest.NewRequest("GET", "/v1/test", nil)
	request.Header.Set("Support-Reference", ref)
	request.AddCookie(&http.Cookie{Name: auth.DevSessionCookieName, Value: token})
	response := httptest.NewRecorder()
	failure.ServeHTTP(response, request)
	if response.Code != 500 || response.Header().Get("Support-Reference") != ref || !strings.Contains(response.Body.String(), ref) {
		t.Fatal("operation reference lost")
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == auth.DevSessionCookieName {
			token = cookie.Value
		}
	}
	lookup := NewRouter(Options{Config: cfg, SupportReports: service, Auth: accounts, OperatorAudit: audit.NewWriter(database), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	deadline := time.Now().Add(3 * time.Second)
	for {
		request = httptest.NewRequest("GET", "/v1/admin/support/"+ref, nil)
		request.AddCookie(&http.Cookie{Name: auth.DevSessionCookieName, Value: token})
		response = httptest.NewRecorder()
		lookup.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("lookup failed: %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "synthetic_failure") {
			if !strings.Contains(response.Body.String(), "support_integration_test.go") {
				t.Fatal("initiating code location missing", response.Body.String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failure not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = database.Pool().Exec(ctx, `UPDATE users SET role='user' WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	lookup.ServeHTTP(response, request)
	if response.Code != 403 || strings.Contains(response.Body.String(), "synthetic_failure") {
		t.Fatal("revoked operator retained access")
	}
}
