package httpapi

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOpenAPIConsoleIdentityAndMachineResponses(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]json.RawMessage
		Components struct{ Schemas map[string]map[string]any }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, contract := range []struct{ path, method, schema string }{
		{"/v1/me", "get", "MeResponse"},
		{"/v1/machines", "get", "MachineListResponse"},
		{"/v1/machines/{machine_id}", "get", "MachineResponse"},
		{"/v1/machines/{machine_id}", "patch", "MachineResponse"},
	} {
		var operation struct {
			Responses map[string]struct {
				Content map[string]struct{ Schema map[string]any }
			}
		}
		if err := json.Unmarshal(doc.Paths[contract.path][contract.method], &operation); err != nil {
			t.Fatal(err)
		}
		got := operation.Responses["200"].Content["application/json"].Schema["$ref"]
		if got != "#/components/schemas/"+contract.schema {
			t.Errorf("%s %s lacks %s response", contract.method, contract.path, contract.schema)
		}
	}
	for schema, fields := range map[string][]string{
		"Me":      {"id", "email", "display_name", "status", "role", "workos_subject"},
		"Machine": {"id", "alias", "platform", "ownership", "permissions", "can_manage", "machine_capabilities", "availability"},
	} {
		properties, _ := doc.Components.Schemas[schema]["properties"].(map[string]any)
		for _, field := range fields {
			if properties[field] == nil {
				t.Errorf("%s lacks public field %s", schema, field)
			}
		}
	}
	machineProperties := doc.Components.Schemas["Machine"]["properties"].(map[string]any)
	versions := machineProperties["runtime_versions"].(map[string]any)
	values, _ := versions["additionalProperties"].(map[string]any)
	if versions["type"] != "object" || values["type"] != "string" {
		t.Fatal("machine runtime versions must expose component-to-version strings")
	}

}
