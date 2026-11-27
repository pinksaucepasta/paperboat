package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/posthogcapture"
	"net/http"
	"net/http/httptest"
)

func TestWorkOSSignupEmailPolicyPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("emailpolicy%d", time.Now().UnixNano())
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE workos_subject LIKE $1`, prefix+"%")
	s := NewService(store, nil, FakeWorkOSVerifier{}, []string{"test-signup-key"}, true)
	login := func(subject, email string) (User, Session, error) {
		service := *s
		service.verifier = sessionLogoutVerifier{profile: WorkOSProfile{Subject: prefix + subject, Email: email, EmailVerified: true}}
		return service.VerifyCallback(ctx, CallbackInput{Code: "verified-provider-code"})
	}
	refused := func(t *testing.T, err error, canonical string) {
		t.Helper()
		var taken *EmailAlreadyRegisteredError
		if !errors.As(err, &taken) || taken.Canonical != canonical || taken.Error() != "An account for "+canonical+" already exists. Sign in with that address." {
			t.Fatalf("want refusal for %s, got %v", canonical, err)
		}
	}
	countUsers := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.users WHERE workos_subject LIKE $1`, prefix+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Order 1: x+tag first, then x and other aliases are refused, never merged.
	first, session, err := login("first", strings.ToUpper(prefix)+".One+first@EXAMPLE.TEST")
	if err != nil {
		t.Fatal(err)
	}
	canonical := prefix + ".one@example.test"
	if session.UserID != first.ID {
		t.Fatal("initial session has wrong account")
	}
	before := countUsers(t)
	for i, email := range []string{canonical, prefix + ".one+second@example.test", prefix + ".ONE+second+third@EXAMPLE.TEST"} {
		user, aliasSession, err := login(fmt.Sprintf("alias%d", i), email)
		refused(t, err, canonical)
		if user.ID != "" || aliasSession.Token != "" {
			t.Fatal("refused sign-up returned an account or session")
		}
	}
	if countUsers(t) != before {
		t.Fatal("a refused sign-up created an account")
	}
	// The existing account is unchanged and still signs in with its own identity.
	again, againSession, err := login("first", strings.ToUpper(prefix)+".One+first@EXAMPLE.TEST")
	if err != nil || again.ID != first.ID || again.PrimaryEmail != first.PrimaryEmail || againSession.UserID != first.ID {
		t.Fatalf("existing account changed or cannot sign in: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "https://login.example.test/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: againSession.Token})
	if got, _, err := s.AuthenticateRequest(ctx, r); err != nil || got.ID != first.ID {
		t.Fatalf("existing session unusable: %v", err)
	}
	// Order 2: x first, then x+tag is refused.
	plain := prefix + "two@example.test"
	owner, _, err := login("owner", plain)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = login("owner-alias", prefix+"two+tag@example.test")
	refused(t, err, plain)
	if got, _, err := login("owner", plain); err != nil || got.ID != owner.ID {
		t.Fatalf("owner cannot sign in after a refused alias: %v", err)
	}
	for i, email := range []string{prefix + "one@example.test", prefix + ".one@other.test"} {
		user, _, err := login(fmt.Sprintf("distinct%d", i), email)
		if err != nil || user.ID == first.ID {
			t.Fatalf("dots or domains were incorrectly merged or refused: %v", err)
		}
	}
	// Refresh verifies the subject that created the session.
	beforeRefresh := WorkOSProfile{Subject: prefix + "refresh", Email: prefix + "refresh@example.test", EmailVerified: true, SessionID: "session_refresh123", RefreshToken: "before-refresh", AccessExpiresAt: time.Now().Add(9 * time.Second)}
	after := beforeRefresh
	after.RefreshToken, after.AccessExpiresAt = "after-refresh", time.Now().Add(time.Hour)
	refresher := NewService(store, nil, verifiedEmailRefreshVerifier{before: beforeRefresh, after: after}, []string{"alias-refresh-key"}, true)
	refresher.RequireWorkOSSessions("alias-refresh-encryption")
	refreshUser, refreshSession, err := refresher.VerifyCallback(ctx, CallbackInput{Code: "alias-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://login.example.test/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: refreshSession.Token})
	if got, _, err := refresher.AuthenticateRequest(ctx, request); err != nil || got.ID != refreshUser.ID {
		t.Fatalf("refresh failed: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.sessions SET workos_access_expires_at=now() WHERE id=$1`, refreshSession.ID); err != nil {
		t.Fatal(err)
	}
	after.Subject = prefix + "wrong-refresh-subject"
	refresher.verifier = verifiedEmailRefreshVerifier{before: beforeRefresh, after: after}
	if _, _, err := refresher.AuthenticateRequest(ctx, request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("changed provider subject survived refresh: %v", err)
	}
	// A legacy row whose stored address is a +tag is left as is; a new alias is refused.
	legacy := prefix + "legacy"
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, legacy, prefix+".old+tag@example.test"); err != nil {
		t.Fatal(err)
	}
	_, _, err = login("legacy-alias", prefix+".old+other@example.test")
	refused(t, err, prefix+".old@example.test")
	var legacyEmail string
	if err = store.SQL().QueryRowContext(ctx, `SELECT primary_email FROM paperboat.users WHERE id=$1`, legacy).Scan(&legacyEmail); err != nil || legacyEmail != prefix+".old+tag@example.test" {
		t.Fatalf("legacy account changed: %q %v", legacyEmail, err)
	}
	// Concurrent first-use aliases: exactly one account, the other refused.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := login(fmt.Sprintf("race%d", i), fmt.Sprintf("%srace+%d@example.test", prefix, i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else {
			refused(t, err, prefix+"race@example.test")
		}
	}
	if ok != 1 {
		t.Fatalf("concurrent aliases: %d succeeded, want 1", ok)
	}
	unverified := *s
	unverified.verifier = sessionLogoutVerifier{profile: WorkOSProfile{Subject: prefix + "unverified", Email: canonical, EmailVerified: false}}
	var taken *EmailAlreadyRegisteredError
	var verification *WorkOSVerificationError
	if _, _, err := unverified.VerifyCallback(ctx, CallbackInput{Code: "unverified-provider-code"}); errors.As(err, &taken) || !errors.As(err, &verification) || verification.Reason != "email_not_verified" {
		t.Fatalf("unverified new subject: want email_not_verified and no account hint, got %v", err)
	}
}

// signed_up is sent once, when VerifyCallback creates the account, and never for a returning sign-in.
func TestWorkOSSignupReportsSignedUpOncePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("signedup%d", time.Now().UnixNano())
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE workos_subject LIKE $1`, prefix+"%")
	var mu sync.Mutex
	var bodies []string
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
	}))
	defer capture.Close()
	client := posthogcapture.New(posthogcapture.Config{Host: capture.URL, Key: "phc_test"})
	runCtx, stop := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() { _ = client.Run(runCtx); close(finished) }()
	s := NewService(store, nil, FakeWorkOSVerifier{}, []string{"test-signup-key"}, true)
	s.SetAnalytics(client)
	service := *s
	service.verifier = sessionLogoutVerifier{profile: WorkOSProfile{Subject: prefix + "a", Email: prefix + "@example.test", EmailVerified: true}}
	first, _, err := service.VerifyCallback(ctx, CallbackInput{Code: "verified-provider-code"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.VerifyCallback(ctx, CallbackInput{Code: "verified-provider-code"}); err != nil {
		t.Fatal(err)
	}
	stop()
	<-finished
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(bodies, "\n")
	if strings.Count(joined, `"event":"signed_up"`) != 1 || !strings.Contains(joined, `"distinct_id":"`+first.ID+`"`) || strings.Contains(joined, prefix) {
		t.Fatalf("want one signed_up for %s and no email or subject in the payload, got %s", first.ID, joined)
	}
}
