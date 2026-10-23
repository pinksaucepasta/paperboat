package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestConfiguredLoginSessionPostgres(t *testing.T) {
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
	s := NewService(store, audit.NewWriter(store), FakeWorkOSVerifier{}, []string{"session-policy-test-key"}, true)
	p := config.DefaultBrowserSessions()
	p.LoginLifetime = 2 * time.Hour
	p.LoginRotateBefore = 10 * time.Minute
	if err = s.ConfigureSessions(p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s.now = func() time.Time { return now }
	user, session, err := s.VerifyCallback(ctx, CallbackInput{Code: fmt.Sprintf("config_session_%d:session-%d@test.invalid", now.UnixNano(), now.UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user.ID)
	if !session.ExpiresAt.Equal(now.Add(p.LoginLifetime)) {
		t.Fatal("login lifetime ignored")
	}
	w := httptest.NewRecorder()
	s.SetSessionCookies(w, session)
	for _, c := range w.Result().Cookies() {
		if !c.Expires.Equal(session.ExpiresAt.Truncate(time.Second)) {
			t.Fatal("cookie deadline disagrees with session")
		}
	}
	request := func(token string) *http.Request {
		r := httptest.NewRequest("GET", "https://login.example.test/v1/me", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
		return r
	}
	_, stored, err := s.AuthenticateRequest(ctx, request(session.Token))
	if err != nil || !stored.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("persisted login deadline err=%v", err)
	}
	now = now.Add(time.Hour)
	rotated, err := s.RotateSession(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.ExpiresAt.Equal(now.Add(p.LoginLifetime)) {
		t.Fatal("rotation lifetime ignored")
	}
	if _, _, err = s.AuthenticateRequest(ctx, request(session.Token)); err == nil {
		t.Fatal("old credential survived rotation")
	}
	if _, got, err := s.AuthenticateRequest(ctx, request(rotated.Token)); err != nil || !got.ExpiresAt.Equal(rotated.ExpiresAt) {
		t.Fatalf("rotated session unavailable: %v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.sessions SET expires_at=now()-interval '1 second' WHERE id=$1`, rotated.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AuthenticateRequest(ctx, request(rotated.Token)); err == nil {
		t.Fatal("expired credential authorized")
	}
}

func TestDevelopmentLogoutRevokesBrowserSessionPostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, audit.NewWriter(store), FakeWorkOSVerifier{}, []string{"development-logout-test-key"}, false)
	user, session, err := s.VerifyCallback(ctx, CallbackInput{Code: fmt.Sprintf("logout_%d:logout-%d@test.invalid", time.Now().UnixNano(), time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user.ID)
	request := httptest.NewRequest(http.MethodPost, "http://dashboard.example.test/v1/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: DevSessionCookieName, Value: session.Token})
	if _, err := s.Logout(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateRequest(ctx, request); err == nil {
		t.Fatal("development session remains authenticated after logout")
	}
}

type sessionLogoutVerifier struct{ profile WorkOSProfile }

func (v sessionLogoutVerifier) VerifyCallback(context.Context, CallbackInput) (WorkOSProfile, error) {
	return v.profile, nil
}

func (sessionLogoutVerifier) LogoutURL(sessionID string) string {
	return (HTTPWorkOSVerifier{BaseURL: "https://api.workos.com"}).LogoutURL(sessionID)
}

type verifiedEmailRefreshVerifier struct {
	before WorkOSProfile
	after  WorkOSProfile
}

func (v verifiedEmailRefreshVerifier) VerifyCallback(context.Context, CallbackInput) (WorkOSProfile, error) {
	return v.before, nil
}

func (v verifiedEmailRefreshVerifier) RefreshSession(context.Context, string) (WorkOSProfile, error) {
	return v.after, nil
}

func TestWorkOSRefreshRechecksVerifiedEmailPostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	before := WorkOSProfile{Subject: fmt.Sprintf("workos_refresh_email_%d", now.UnixNano()), Email: fmt.Sprintf("allowed-%d@test.invalid", now.UnixNano()), EmailVerified: true, SessionID: "session_123456", RefreshToken: "before-refresh", AccessExpiresAt: now.Add(9 * time.Second)}
	after := before
	after.Email, after.EmailVerified, after.RefreshToken, after.AccessExpiresAt = fmt.Sprintf("removed-%d@test.invalid", now.UnixNano()), false, "after-refresh", now.Add(time.Hour)
	s := NewService(store, audit.NewWriter(store), verifiedEmailRefreshVerifier{before: before, after: after}, []string{"refresh-email-test-key"}, false)
	s.RequireWorkOSSessions("refresh-email-encryption-key")
	user, session, err := s.VerifyCallback(ctx, CallbackInput{Code: "first"})
	if err != nil || !user.EmailVerified {
		t.Fatalf("initial login: user=%+v err=%v", user, err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user.ID)
	request := httptest.NewRequest(http.MethodGet, "http://dashboard.example.test/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: DevSessionCookieName, Value: session.Token})
	current, _, err := s.AuthenticateRequest(ctx, request)
	if err != nil || current.EmailVerified || current.PrimaryEmail != after.Email {
		t.Fatalf("pre-refresh email authorized after refresh: user=%+v err=%v", current, err)
	}
}

func TestWorkOSVerifiedEmailFollowsProviderProfilePostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	profile := WorkOSProfile{Subject: fmt.Sprintf("workos_verified_%d", stamp), Email: fmt.Sprintf("first-%d@test.invalid", stamp), EmailVerified: true}
	s := NewService(store, audit.NewWriter(store), sessionLogoutVerifier{profile: profile}, []string{"verified-email-test-key"}, false)
	user, session, err := s.VerifyCallback(ctx, CallbackInput{Code: "first"})
	if err != nil || !user.EmailVerified {
		t.Fatalf("verified callback: user=%+v err=%v", user, err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user.ID)
	profile.Email, profile.EmailVerified = fmt.Sprintf("second-%d@test.invalid", stamp), false
	s.verifier = sessionLogoutVerifier{profile: profile}
	if _, _, err := s.VerifyCallback(ctx, CallbackInput{Code: "second"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://dashboard.example.test/v1/me", nil)
	request.AddCookie(&http.Cookie{Name: DevSessionCookieName, Value: session.Token})
	current, _, err := s.AuthenticateRequest(ctx, request)
	if err != nil || current.EmailVerified || current.PrimaryEmail != profile.Email {
		t.Fatalf("old session retained stale verified email: user=%+v err=%v", current, err)
	}
}

func TestWorkOSSessionLogoutPostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	verifier := sessionLogoutVerifier{profile: WorkOSProfile{
		Subject: fmt.Sprintf("workos_logout_%d", time.Now().UnixNano()),
		Email:   fmt.Sprintf("workos-logout-%d@test.invalid", time.Now().UnixNano()), SessionID: "session_123",
	}}
	s := NewService(store, audit.NewWriter(store), verifier, []string{"workos-logout-test-key"}, false)
	user, session, err := s.VerifyCallback(ctx, CallbackInput{Code: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user.ID)
	request := httptest.NewRequest(http.MethodPost, "http://dashboard.example.test/v1/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: DevSessionCookieName, Value: session.Token})
	_, persisted, err := s.AuthenticateRequest(ctx, request)
	if err != nil || persisted.WorkOSSessionID != "session_123" {
		t.Fatalf("WorkOS session not persisted: %v", err)
	}
	rotated, err := s.RotateSession(ctx, persisted)
	if err != nil || rotated.WorkOSSessionID != "session_123" {
		t.Fatalf("WorkOS session not preserved after rotation: %v", err)
	}
	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example.test/v1/auth/logout", nil)
	request.AddCookie(&http.Cookie{Name: DevSessionCookieName, Value: rotated.Token})
	logoutURL, err := s.Logout(ctx, request)
	if err != nil || logoutURL != "https://api.workos.com/user_management/sessions/logout?session_id=session_123" {
		t.Fatalf("provider logout URL = %q, err = %v", logoutURL, err)
	}
	if _, _, err := s.AuthenticateRequest(ctx, request); err == nil {
		t.Fatal("revoked session remained authenticated")
	}
}
