package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

// A command that names a terminal machine lists the account's machines once, and one that falls back to
// the workspace default chooses that machine from the same listing.
func TestResolvingTheTerminalTargetListsMachinesOnce(t *testing.T) {
	var listings atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		listings.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"items":[{"id":"machine_1","alias":"hp","state":"online","online":true,"capabilities":{"terminal_host":{"configured":true,"observed":true}}}],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":null}}}`))
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}, server.Client())
	target, err := resolveTerminalTargetOrDefault(t.Context(), nil, client, nil, "hp")
	if err != nil || target.id != "machine_1" || target.name != "hp" {
		t.Fatalf("target %+v err %v", target, err)
	}
	if got := listings.Load(); got != 1 {
		t.Fatalf("the machine list was fetched %d times, want 1", got)
	}

	machines, err := client.ListUserMachines(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := defaultEnvironmentFrom(machines, "", "personal")
	if err != nil || chosen != "machine_1" {
		t.Fatalf("default chosen from an existing listing: %q %v", chosen, err)
	}
	if _, err := resolveUserMachineIn(machines, chosen); err != nil {
		t.Fatal(err)
	}
}
