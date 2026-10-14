package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestOAuthStateExpiresServerSide(t *testing.T) {
	now := time.Unix(1000, 0)
	service := NewService(nil, nil, nil, []string{"test-session-signing-key"}, false)
	service.now = func() time.Time { return now }
	state, err := service.NewOAuthState()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/auth/workos/callback", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: DevOAuthStateCookieName, Value: state})
	if err := service.ValidateOAuthState(req, state); err != nil {
		t.Fatalf("fresh state rejected: %v", err)
	}
	service.now = func() time.Time { return now.Add(11 * time.Minute) }
	if err := service.ValidateOAuthState(req, state); err == nil {
		t.Fatal("expired state was accepted")
	}
}

func TestOAuthStateBindsSafeDashboardReturnPath(t *testing.T) {
	service := NewService(nil, nil, nil, []string{"test-session-signing-key"}, false)
	for _, path := range []string{"/dashboard", "/dashboard/terminals/term_123/console"} {
		state, err := service.NewOAuthState(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := service.OAuthReturnTo(state)
		if err != nil || got != path {
			t.Fatalf("return path = %q, err=%v, want %q", got, err, path)
		}
		if _, err := service.OAuthReturnTo(state + "changed"); err == nil {
			t.Fatal("tampered state retained return destination")
		}
	}
	for _, path := range []string{"https://attacker.example/", "//attacker.example", "/dashboard//evil", "/dashboard/../admin", "/dashboard/%2f%2fattacker.example", "/dashboard/term\\evil", "/dashboard?next=//attacker.example"} {
		if _, err := service.NewOAuthState(path); err == nil {
			t.Fatalf("unsafe return destination %q accepted", path)
		}
	}
}
