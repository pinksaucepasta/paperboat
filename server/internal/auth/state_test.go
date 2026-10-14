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

func TestOAuthStateRejectsTamperingAndCookieMismatch(t *testing.T) {
	service := NewService(nil, nil, nil, []string{"test-session-signing-key"}, false)
	state, err := service.NewOAuthState()
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.NewOAuthState()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{state + "changed", other, ""} {
		request, err := http.NewRequest(http.MethodPost, "/v1/auth/workos/callback", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: DevOAuthStateCookieName, Value: state})
		if err := service.ValidateOAuthState(request, candidate); err == nil {
			t.Fatal("unbound OAuth state accepted")
		}
	}
}
