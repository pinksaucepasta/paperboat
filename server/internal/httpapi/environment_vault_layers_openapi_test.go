package httpapi

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/environment"
)

func TestOpenAPIEnvironmentLayerContracts(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]any `json:"paths"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	// Every public DTO field is documented, including zero delivery registration,
	// nullable observations and cross-workspace rotation source coordinates.
	for name, value := range map[string]any{
		"EnvironmentVaultLayerCoordinate":      environment.VaultLayerCoordinate{},
		"EnvironmentVaultLayerSource":          environment.VaultLayerSource{},
		"EnvironmentVaultLayerRecipient":       environment.VaultLayerRecipient{},
		"EnvironmentVaultLayerDelivery":        environment.VaultLayerDelivery{},
		"EnvironmentVaultLayerPut":             environment.VaultLayerPut{},
		"EnvironmentVaultLayerContext":         environment.VaultLayerContext{},
		"EnvironmentVaultLayerObservation":     environment.VaultLayerObservation{},
		"EnvironmentVaultLayerObservationsPut": environment.VaultLayerObservationsPut{},
		"EnvironmentVaultLayerObservationsAck": environment.VaultLayerObservationsAck{},
		"EnvironmentVaultGrant":                environment.VaultGrantState{},
		"EnvironmentVaultScopeDocument":        environment.VaultScopeDocument{},
		"EnvironmentVaultPersonalScopeStage":   environment.VaultPersonalScopeStage{},
	} {
		schema := doc.Components.Schemas[name]
		if schema == nil {
			t.Fatalf("missing %s", name)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("missing %s properties", name)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err = json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != len(props) {
			t.Fatalf("%s documented fields differ from public DTO", name)
		}
		required := map[string]bool{}
		for _, key := range schema["required"].([]any) {
			required[key.(string)] = true
		}
		for key := range fields {
			if _, ok = props[key]; !ok || !required[key] {
				t.Fatalf("%s field %s must be documented and required", name, key)
			}
		}
	}
	for name := range map[string]bool{"EnvironmentVaultScope": true, "EnvironmentVaultPersonalScopeHead": true} {
		required := doc.Components.Schemas[name]["required"].([]any)
		found := false
		for _, key := range required {
			found = found || key == "workspace_id"
		}
		if !found {
			t.Fatalf("%s omits signed workspace", name)
		}
	}
	for path, methods := range map[string][]string{
		"/v1/environment/layers":                                {"get"},
		"/v1/environment/layers/{machine_id}":                   {"get", "put"},
		"/v1/environment/hosts/{machine_id}/layers":             {"post"},
		"/v1/environment/hosts/{machine_id}/layer-observations": {"post"},
	} {
		for _, method := range methods {
			op, _ := doc.Paths[path][method].(map[string]any)
			if op == nil {
				t.Fatalf("missing %s %s", method, path)
			}
			if method == "get" && path == "/v1/environment/layers/{machine_id}" {
				for _, p := range op["parameters"].([]any) {
					if p.(map[string]any)["name"] == "actor_account_id" {
						t.Fatal("human layer context cannot override actor")
					}
				}
			}
		}
	}
	registration := doc.Paths["/v1/environment/hosts/{machine_id}/key"]["post"].(map[string]any)
	response := registration["responses"].(map[string]any)["200"].(map[string]any)
	schema := response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	data := schema["properties"].(map[string]any)["data"].(map[string]any)
	if data["$ref"] != "#/components/schemas/EnvironmentVaultLayerRecipient" {
		t.Fatal("registration must return independent layer recipient")
	}
}
