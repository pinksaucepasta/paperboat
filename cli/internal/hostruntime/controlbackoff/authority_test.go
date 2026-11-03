package controlbackoff

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func TestHeldAPIRequestPreservesTheServersAuthenticationDenial(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"success":false,"error":{"code":"unauthenticated","message":"revoked"}}`))
	}))
	defer server.Close()
	address, _ := url.Parse(server.URL)
	transport := &Transport{Base: server.Client().Transport, Host: address.Host}
	client := api.New(server.URL, config.Credential{AccessToken: "synthetic-authority-test"}, &http.Client{Transport: transport})
	for range 2 {
		_, err := client.ListUserMachines(t.Context())
		if !errors.Is(err, api.ErrUnauthenticated) {
			t.Fatal("API client lost the server's authentication denial behind the breaker")
		}
		fault := errorreport.ProjectFault(t.Context(), "paperboat-daemon", "machine_events", "delivery", "control_request_failed", err)
		if fault.HTTPStatus != http.StatusUnauthorized {
			t.Fatal("held denial cannot wake authorization consumers")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("preserving denial semantics bypassed the network breaker")
	}
}
