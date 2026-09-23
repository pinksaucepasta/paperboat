package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestEnrollmentLoginDoesNotRedirectToken(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := New(server.URL, config.Credential{}, nil).LoginWithEnrollmentToken(context.Background(), "0123456789ABCDEFGHIJKLMNOP", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "test", "linux")
	if err == nil || calls != 0 {
		t.Fatalf("redirect err=%v forwarded=%d", err, calls)
	}
}
