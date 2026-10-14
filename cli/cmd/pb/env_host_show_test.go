package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

func TestENVHostShowSeparatesPublicationFromObservedApplication(t *testing.T) {
	for _, scenario := range []string{"absent", "stale", "applied"} {
		t.Run(scenario, func(t *testing.T) {
			layer := api.VaultLayerDelivery{Source: api.VaultLayerSource{VaultLayerCoordinate: api.VaultLayerCoordinate{WorkspaceID: "team-one", OwnerKind: "team", OwnerID: "team-one"}, Revision: 3}, Recipient: api.VaultLayerRecipient{DeliveryGeneration: 3, DocumentID: "current_doc", FenceGeneration: 4, HostPublic: "PRIVATE_KEY_PAYLOAD"}, State: "ready", Envelope: "PRIVATE_CIPHERTEXT"}
			if scenario != "absent" {
				layer.Observation = &api.VaultLayerObservation{State: "applied", DeliveryGeneration: 2, DocumentID: "stale_doc", FenceGeneration: 3, ObservedAt: time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)}
			}
			if scenario == "applied" {
				layer.Applied = true
				layer.Observation.DeliveryGeneration = 3
				layer.Observation.DocumentID = "current_doc"
				layer.Observation.FenceGeneration = 4
			}
			host := api.VaultLayerContext{WorkspaceID: "team-one", ActorAccountID: "account_1", MachineID: "machine_1", Layers: []api.VaultLayerDelivery{layer}}

			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/v1/environment/layers/machine_1" || r.URL.Query().Get("workspace") != "team-one" {
					t.Error("wrong owner host read")
				}
				w.Header().Set("Cache-Control", "no-store")
				json.NewEncoder(w).Encode(map[string]any{"data": host})
			}))
			defer server.Close()
			previousBackend := environmentVariableBackendForCommand
			previousResolver := environmentVariableResolveMachine
			t.Cleanup(func() {
				environmentVariableBackendForCommand = previousBackend
				environmentVariableResolveMachine = previousResolver
			})
			client := api.New(server.URL, config.Credential{}, server.Client())
			if err := client.SetWorkspace("team-one"); err != nil {
				t.Fatal(err)
			}
			environmentVariableBackendForCommand = func(*cobra.Command) (*api.Client, error) { return client, nil }
			environmentVariableResolveMachine = func(context.Context, *api.Client, string) (api.UserMachine, error) {
				return api.UserMachine{ID: "machine_1", Capabilities: api.MachineCapabilities{EnvironmentInjection: api.MachineCapability{Configured: true}}}, nil
			}
			var out bytes.Buffer
			command := newEnvironmentTestCommand(strings.NewReader(""), &out)
			command.Flags().Bool("json", false, "")
			if err := command.Flags().Set("json", "true"); err != nil {
				t.Fatal(err)
			}
			if err := runVaultHostShow(command, "machine_1"); err != nil {
				t.Fatal(err)
			}
			var metadata vaultHostMetadata
			if err := json.Unmarshal(out.Bytes(), &metadata); err != nil {
				t.Fatal(err)
			}
			if !metadata.Published || metadata.Applied != (scenario == "applied") || requests != 1 || strings.Contains(out.String(), "PRIVATE") {
				t.Fatal("host show conflated publication/application or exposed ciphertext")
			}
			out.Reset()
			if err := command.Flags().Set("json", "false"); err != nil {
				t.Fatal(err)
			}
			if err := runVaultHostShow(command, "machine_1"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "PUBLISHED\ttrue") || strings.Contains(out.String(), "PRIVATE") {
				t.Fatal("human host metadata omitted publication or leaked data")
			}
			if scenario != "applied" && !strings.Contains(out.String(), "APPLIED\tfalse") {
				t.Fatal("stale or absent observation was treated as applied")
			}
		})
	}
}

func TestENVRecoveryFromActualTeamEntitlementAPIResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environment/scopes/team/team-one" || r.URL.Query().Get("workspace") != "team-one" {
			t.Error("wrong Team source authorization")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "team_entitlement_required", "message": "PRIVATE_PROVIDER_VALUE"}})
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{}, server.Client())
	if err := client.SetWorkspace("team-one"); err != nil {
		t.Fatal(err)
	}
	_, cause := client.GetVaultScope(context.Background(), "team", "team-one", "")
	if cause == nil {
		t.Fatal("unpaid Team source was accepted")
	}
	err := safeEnvironmentVariableCommandError(errors.Join(cause, nil))
	result := classifyCLIJSONError(err)
	if !strings.Contains(result.Message, "Team Billing") || strings.Contains(result.Message, "PRIVATE") || result.Code != "team_entitlement_required" || result.Retryable {
		t.Fatalf("actual API recovery=%#v", result)
	}
	mixed := safeEnvironmentVariableCommandError(errors.Join(cause, syscall.EIO))
	if !errors.Is(mixed, syscall.EIO) || strings.Contains(userFacingError(mixed), "Team Billing") || classifyCommandFailure(mixed).kind != commandUnexpected {
		t.Fatal("mixed IO failure was mislabeled as a simple entitlement rejection")
	}
}
