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
	first, session, err := login("first", strings.ToUpper(prefix)+".One+first@EXAMPLE.TEST")
	if err != nil {
		t.Fatal(err)
	}
	canonical := prefix + ".one@example.test"
	if first.PrimaryEmail != canonical {
		t.Fatalf("display/contact address is not canonical")
	}
	for i, email := range []string{canonical, prefix + ".one+second@example.test", prefix + ".ONE+second+third@EXAMPLE.TEST"} {
		user, aliasSession, err := login(fmt.Sprintf("alias%d", i), email)
		if err != nil || user.ID != first.ID || aliasSession.UserID != first.ID || aliasSession.Token == "" {
			t.Fatalf("alias did not sign into canonical account: %v", err)
		}
		r := httptest.NewRequest(http.MethodGet, "https://login.example.test/v1/me", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: aliasSession.Token})
		if got, _, err := s.AuthenticateRequest(ctx, r); err != nil || got.ID != first.ID {
			t.Fatalf("alias session unusable: %v", err)
		}
	}
	if session.UserID != first.ID {
		t.Fatal("initial alias session has wrong account")
	}
	for i, email := range []string{prefix + "one@example.test", prefix + ".one@other.test"} {
		user, _, err := login(fmt.Sprintf("distinct%d", i), email)
		if err != nil || user.ID == first.ID {
			t.Fatalf("dots or domains were incorrectly merged: %v", err)
		}
	}
	// Refresh must verify the subject that created this alias session, rather
	// than the original subject retained on the canonical account.
	before := WorkOSProfile{Subject: prefix + "refresh", Email: prefix + ".one+refresh@example.test", EmailVerified: true, SessionID: "session_alias123", RefreshToken: "before-refresh", AccessExpiresAt: time.Now().Add(9 * time.Second)}
	after := before
	after.RefreshToken, after.AccessExpiresAt = "after-refresh", time.Now().Add(time.Hour)
	refresher := NewService(store, nil, verifiedEmailRefreshVerifier{before: before, after: after}, []string{"alias-refresh-key"}, true)
	refresher.RequireWorkOSSessions("alias-refresh-encryption")
	alias, refreshSession, err := refresher.VerifyCallback(ctx, CallbackInput{Code: "alias-refresh"})
	if err != nil || alias.ID != first.ID {
		t.Fatalf("refresh alias did not bind canonical user: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://login.example.test/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: refreshSession.Token})
	if got, _, err := refresher.AuthenticateRequest(ctx, request); err != nil || got.ID != first.ID || got.PrimaryEmail != canonical {
		t.Fatalf("alias refresh used account's original provider subject: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.sessions SET workos_access_expires_at=now() WHERE id=$1`, refreshSession.ID); err != nil {
		t.Fatal(err)
	}
	after.Subject = prefix + "wrong-refresh-subject"
	refresher.verifier = verifiedEmailRefreshVerifier{before: before, after: after}
	if _, _, err := refresher.AuthenticateRequest(ctx, request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("changed provider subject survived refresh: %v", err)
	}
	legacy := prefix + "legacy"
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, legacy, prefix+".old+tag@example.test"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := login("legacy-alias", prefix+".old+other@example.test"); err != nil || got.ID != legacy {
		t.Fatalf("legacy canonical identity duplicated: %v", err)
	}
	var wg sync.WaitGroup
	type result struct {
		user User
		err  error
	}
	results := make(chan result, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, _, err := login(fmt.Sprintf("race%d", i), fmt.Sprintf("%srace+%d@example.test", prefix, i))
			results <- result{u, err}
		}(i)
	}
	wg.Wait()
	close(results)
	var raceID string
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if raceID != "" && raceID != result.user.ID {
			t.Fatal("concurrent aliases created separate accounts")
		}
		raceID = result.user.ID
	}
	unverified := *s
	unverified.verifier = sessionLogoutVerifier{profile: WorkOSProfile{Subject: prefix + "unverified", Email: canonical, EmailVerified: false}}
	if _, _, err := unverified.VerifyCallback(ctx, CallbackInput{Code: "unverified-provider-code"}); err == nil {
		t.Fatal("unverified new subject took over canonical account")
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
