package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppInstalledMatchesTheAccountsOwnActiveInstallation(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/user/installations") || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+`?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"installations":[{"id":1,"account":{"login":"an-org"}},{"id":2,"account":{"login":"paused"},"suspended_at":"2026-01-01T00:00:00Z"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"installations":[{"id":3,"account":{"login":"Second-Page"}}]}`))
	}))
	defer api.Close()
	client := HTTPClient{BaseURL: api.URL, Client: api.Client()}
	for login, want := range map[string]bool{"an-org": true, "second-page": true, "paused": false, "nobody": false} {
		installed, err := client.AppInstalled(context.Background(), "token", login)
		if err != nil || installed != want {
			t.Fatalf("%s: installed = %t, %v; want %t", login, installed, err, want)
		}
	}
}
