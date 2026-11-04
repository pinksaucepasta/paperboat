package clientapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallInjectedIdentityAndSafeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer session-secret" || r.Header.Get("X-Paperboat-Protocol") != "test-protocol" {
			t.Error("injected identity missing")
		}
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"data":{"name":"present","future":true}}`))
		case "/large":
			_, _ = w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"secret-from-server","message":"private server message"}}`))
		}
	}))
	defer server.Close()
	options := CallOptions{HTTPClient: server.Client(), Headers: http.Header{"X-Paperboat-Protocol": {"test-protocol"}}}
	var out struct {
		Name string `json:"name"`
	}
	if err := Call(context.Background(), server.URL, http.MethodGet, "/ok", nil, "session-secret", &out, options); err != nil || out.Name != "present" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if err := Call(context.Background(), server.URL, http.MethodGet, "/large", nil, "session-secret", &out, options); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("large response error: %v", err)
	}
	err := Call(context.Background(), server.URL, http.MethodGet, "/denied", nil, "session-secret", &out, options)
	var refused *ResponseError
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden || refused.Code != "secret-from-server" {
		t.Fatalf("typed refusal: %v", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal("server content exposed through safe error")
	}
	if err := Call(context.Background(), server.URL, http.MethodGet, "/ok", nil, "", &out, CallOptions{}); err == nil {
		t.Fatal("missing HTTP owner accepted")
	}
}
