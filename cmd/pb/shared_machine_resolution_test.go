package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestSharedMachineSSHAliasRequiresUnambiguousTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":"machine_first","alias":"dev","display_name":"First"},{"id":"machine_second","alias":"dev","display_name":"Second"}],"total":2}}`))
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if _, err := resolveSSHMachine(context.Background(), client, "dev"); !errors.Is(err, resolver.ErrProjectAmbiguous) {
		t.Fatalf("shared alias silently selected a machine: %v", err)
	}
	machine, err := resolveSSHMachine(context.Background(), client, "machine_second")
	if err != nil || machine.ID != "machine_second" {
		t.Fatalf("exact target: %q %v", machine.ID, err)
	}
}
