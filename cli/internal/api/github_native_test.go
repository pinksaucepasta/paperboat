package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestGitHubNativeLinkRejectsUnsafeBrowserHandoff(t *testing.T) {
	for _, test := range []struct{ name, suffix string }{
		{"foreign origin", "https://other.invalid/v1/github/native-links/link/launch?token=opaque"},
		{"wrong path", "/auth/callback?token=opaque"},
		{"fragment", "/v1/github/native-links/link/launch?token=opaque#fragment"},
		{"valid", "/v1/github/native-links/link/launch?token=opaque"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var origin string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/github/native-links" || r.Header.Get("Authorization") != "Bearer native" {
					t.Error("incorrect authenticated start")
					http.Error(w, "bad request", 400)
					return
				}
				browser := test.suffix
				if browser[0] == '/' {
					browser = origin + browser
				}
				writeData(w, 201, GitHubNativeLink{ID: "link", State: "pending", BrowserURL: browser, ExpiresAt: time.Now().Add(time.Minute)})
			}))
			defer srv.Close()
			origin = srv.URL
			link, err := New(origin, config.Credential{AccessToken: "native"}, nil).StartGitHubNativeLink(context.Background())
			if (err == nil) != (test.name == "valid") {
				t.Fatalf("handoff accepted=%v", err == nil)
			}
			if link.ID != "link" {
				t.Fatal("created operation unavailable for cancellation")
			}
		})
	}
}

func TestGitHubNativeLinkStatusAndCancelPreserveNativeAuthentication(t *testing.T) {
	methods := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/github/native-links/link" || r.Header.Get("Authorization") != "Bearer native" {
			t.Error("incorrect operation authority")
			http.Error(w, "bad request", 400)
			return
		}
		methods = append(methods, r.Method)
		state := "pending"
		if r.Method == http.MethodDelete {
			state = "canceled"
		}
		writeData(w, 200, GitHubNativeLink{ID: "link", State: state})
	}))
	defer srv.Close()
	client := New(srv.URL, config.Credential{AccessToken: "native"}, nil)
	if state, err := client.GitHubNativeLinkStatus(context.Background(), "link"); err != nil || state.State != "pending" {
		t.Fatal("cannot poll own operation")
	}
	if state, err := client.CancelGitHubNativeLink(context.Background(), "link"); err != nil || state.State != "canceled" {
		t.Fatal("cannot cancel own operation")
	}
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodDelete {
		t.Fatal("incorrect lifecycle methods")
	}
}
