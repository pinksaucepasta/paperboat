package httpapi

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOpenAPIFreezesCLIContractSchemas(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("openapi json is invalid: %v", err)
	}

	client := doc.Components.Schemas["CLIClientSession"]
	assertExactCLIScopes(t, doc.Components.Schemas["DeviceAuthorizationRequest"], "DeviceAuthorizationRequest")
	assertExactCLIScopes(t, doc.Components.Schemas["DeviceRequest"], "DeviceRequest")
	assertExactCLIScopes(t, client, "CLIClientSession")
	tokenSetProperties := objectValue(t, doc.Components.Schemas["TokenSet"]["properties"], "TokenSet.properties")
	scope := objectValue(t, tokenSetProperties["scope"], "TokenSet.scope")
	if scope["const"] != "account:read clients:revoke diagnostics:upload operations:read operations:write previews:read previews:write machines:connect machines:read session:refresh tunnels:read tunnels:write" {
		t.Fatalf("TokenSet.scope const = %v", scope["const"])
	}
	descriptor := doc.Components.Schemas["EnvironmentConnectionDescriptor"]
	descriptorRequired := stringSet(t, descriptor["required"], "EnvironmentConnectionDescriptor.required")
	for _, field := range []string{"schema", "issuer", "connectable", "expires_at", "environment", "status", "reason", "retry_after_seconds"} {
		if !descriptorRequired[field] {
			t.Fatalf("EnvironmentConnectionDescriptor does not require %q", field)
		}
	}
	descriptorProperties := objectValue(t, descriptor["properties"], "EnvironmentConnectionDescriptor.properties")
	schema := objectValue(t, descriptorProperties["schema"], "EnvironmentConnectionDescriptor.schema")
	if schema["const"] != "paperboat.environment-connection/v1" {
		t.Fatalf("EnvironmentConnectionDescriptor.schema is not canonical v1")
	}
	for _, field := range []string{"project_id", "project_state"} {
		if _, present := descriptorProperties[field]; present {
			t.Fatalf("EnvironmentConnectionDescriptor retains legacy field %q", field)
		}
	}
	environment := objectValue(t, descriptorProperties["environment"], "EnvironmentConnectionDescriptor.environment")
	environmentProperties := objectValue(t, environment["properties"], "EnvironmentConnectionDescriptor.environment.properties")
	if _, present := environmentProperties["display_name"]; present {
		t.Fatal("environment descriptor retains obsolete machine display_name")
	}
	for _, field := range []string{"id", "kind", "resource_id", "alias", "state"} {
		if _, present := environmentProperties[field]; !present {
			t.Fatalf("EnvironmentConnectionDescriptor.environment lacks canonical field %q", field)
		}
	}
	for _, field := range []string{"environment_id", "project_id", "machine_id", "project_root"} {
		if _, present := environmentProperties[field]; present {
			t.Fatalf("EnvironmentConnectionDescriptor.environment retains legacy field %q", field)
		}
	}
	terminal := objectValue(t, descriptorProperties["terminal"], "EnvironmentConnectionDescriptor.terminal")
	terminalProperties := objectValue(t, terminal["properties"], "EnvironmentConnectionDescriptor.terminal.properties")
	for _, field := range []string{"endpoint", "auth", "session_id", "cwd"} {
		if _, present := terminalProperties[field]; !present {
			t.Fatalf("EnvironmentConnectionDescriptor.terminal lacks canonical field %q", field)
		}
	}
	for _, field := range []string{"kind", "websocket_base_url", "http_base_url"} {
		if _, present := terminalProperties[field]; present {
			t.Fatalf("EnvironmentConnectionDescriptor.terminal retains legacy field %q", field)
		}
	}
	terminalAuthVariants, ok := doc.Components.Schemas["TerminalAuth"]["oneOf"].([]any)
	if !ok || len(terminalAuthVariants) != 2 {
		t.Fatalf("TerminalAuth.oneOf = %#v, want ticket and bearer variants", doc.Components.Schemas["TerminalAuth"]["oneOf"])
	}
	wantTerminalMethods := map[string]bool{"websocket_ticket": false, "bearer": false}
	for index, raw := range terminalAuthVariants {
		variant := objectValue(t, raw, fmt.Sprintf("TerminalAuth.oneOf[%d]", index))
		properties := objectValue(t, variant["properties"], fmt.Sprintf("TerminalAuth.oneOf[%d].properties", index))
		method := objectValue(t, properties["method"], "TerminalAuth.method")["const"]
		methodString, valid := method.(string)
		_, expected := wantTerminalMethods[methodString]
		if !valid || !expected {
			t.Fatalf("TerminalAuth method = %#v", method)
		}
		wantTerminalMethods[methodString] = true
		assertSingletonConstScope(t, properties["scopes"], "terminal:operate", "TerminalAuth.scopes")
	}
	for method, present := range wantTerminalMethods {
		if !present {
			t.Fatalf("TerminalAuth lacks %q variant", method)
		}
	}
	transferProperties := objectValue(t, doc.Components.Schemas["FileTransfer"]["properties"], "FileTransfer.properties")
	transferAuth := objectValue(t, transferProperties["auth"], "FileTransfer.auth")
	transferAuthProperties := objectValue(t, transferAuth["properties"], "FileTransfer.auth.properties")
	assertSingletonConstScope(t, transferAuthProperties["scopes"], "file:transfer", "FileTransfer.auth.scopes")
	for _, legacy := range []string{"max_bytes", "allowed_mime_types", "mime_type"} {
		if _, present := transferProperties[legacy]; present {
			t.Fatalf("FileTransfer retains legacy field %q", legacy)
		}
	}
	required := stringSet(t, client["required"], "CLIClientSession.required")
	for _, field := range []string{
		"cli_client_session_id", "client_id", "client_label", "device_type", "os", "scopes",
		"state", "created_at", "approved_at", "last_used_at", "revoked_at",
		"revocation_reason", "current",
	} {
		if !required[field] {
			t.Fatalf("CLIClientSession does not require %q", field)
		}
	}

	list := doc.Components.Schemas["CLIClientSessionList"]
	listProperties := objectValue(t, list["properties"], "CLIClientSessionList.properties")
	items := objectValue(t, listProperties["items"], "CLIClientSessionList.items")
	itemSchema := objectValue(t, items["items"], "CLIClientSessionList.items.items")
	if itemSchema["$ref"] != "#/components/schemas/CLIClientSession" {
		t.Fatalf("authorized-client item ref = %v", itemSchema["$ref"])
	}
	pagination := objectValue(t, listProperties["pagination"], "CLIClientSessionList.pagination")
	paginationRequired := stringSet(t, pagination["required"], "CLIClientSessionList.pagination.required")
	if !reflect.DeepEqual(paginationRequired, map[string]bool{
		"limit": true, "offset": true, "total": true, "next_offset": true,
	}) {
		t.Fatalf("pagination required fields = %#v", paginationRequired)
	}

	get := objectValue(t, doc.Paths["/v1/auth/cli-client-sessions"]["get"], "GET /v1/auth/cli-client-sessions")
	assertRequiredBearerScope(t, get, "account:read", "GET /v1/auth/cli-client-sessions")
	configSync := objectValue(t, doc.Paths["/v1/config-sync/status"]["get"], "GET /v1/config-sync/status")
	assertRequiredBearerScope(t, configSync, "account:read", "GET /v1/config-sync/status")
	operation := doc.Paths["/v1/operations/{operation_id}"]
	assertRequiredBearerScope(t, objectValue(t, operation["get"], "GET operation"), "operations:read", "GET operation")
	assertRequiredBearerScope(t, objectValue(t, operation["delete"], "DELETE operation"), "operations:write", "DELETE operation")
	responses := objectValue(t, get["responses"], "authorized-client responses")
	okResponse := objectValue(t, responses["200"], "authorized-client 200")
	content := objectValue(t, okResponse["content"], "authorized-client content")
	jsonContent := objectValue(t, content["application/json"], "authorized-client JSON")
	responseSchema := objectValue(t, jsonContent["schema"], "authorized-client response schema")
	properties := objectValue(t, responseSchema["properties"], "authorized-client response properties")
	data := objectValue(t, properties["data"], "authorized-client response data")
	if data["$ref"] != "#/components/schemas/CLIClientSessionList" {
		t.Fatalf("authorized-client response ref = %v", data["$ref"])
	}
	deleteClient := objectValue(t, doc.Paths["/v1/auth/cli-client-sessions/{cli_client_session_id}"]["delete"], "DELETE /v1/auth/cli-client-sessions/{cli_client_session_id}")
	assertRequiredBearerScope(t, deleteClient, "clients:revoke", "DELETE /v1/auth/cli-client-sessions/{cli_client_session_id}")
	githubRepositories := objectValue(t, doc.Paths["/v1/github/repositories"]["get"], "GET /v1/github/repositories")
	assertRequiredBearerScope(t, githubRepositories, "machines:read", "GET /v1/github/repositories")
	configRepositories := objectValue(t, doc.Paths["/v1/config-repositories"]["get"], "GET /v1/config-repositories")
	assertRequiredBearerScope(t, configRepositories, "machines:read", "GET /v1/config-repositories")
	for _, path := range []string{"/v1/config-repositories", "/v1/machines/{machine_id}/config-assignment/consent"} {
		assertRequiredBearerScope(t, objectValue(t, doc.Paths[path]["post"], "POST "+path), "machines:connect", "POST "+path)
	}
	for _, path := range []string{"/v1/config-sync/environments/{environment_id}/conflict-resolutions", "/v1/config-sync/environments/{environment_id}/force"} {
		operation := objectValue(t, doc.Paths[path]["post"], "POST "+path)
		assertRequiredBearerScope(t, operation, "machines:connect", "POST "+path)
		requirements, ok := operation["security"].([]any)
		if !ok {
			t.Fatalf("%s missing auth requirements", path)
		}
		browserProtected := false
		for _, requirement := range requirements {
			values := objectValue(t, requirement, path+" security")
			if values["cookieSession"] != nil {
				browserProtected = values["csrfHeader"] != nil
			}
		}
		if !browserProtected {
			t.Fatalf("%s browser session lost CSRF requirement", path)
		}
	}
	for path, methods := range map[string][]string{"/v1/github/native-links": {"post"}, "/v1/github/native-links/{link_id}": {"get", "delete"}} {
		for _, method := range methods {
			assertRequiredBearerScope(t, objectValue(t, doc.Paths[path][method], method+" "+path), "machines:connect", method+" "+path)
		}
	}
	configAssignment := doc.Paths["/v1/machines/{machine_id}/config-assignment"]
	assertRequiredBearerScope(t, objectValue(t, configAssignment["get"], "GET config assignment"), "machines:read", "GET config assignment")
	assertRequiredBearerScope(t, objectValue(t, configAssignment["put"], "PUT config assignment"), "machines:connect", "PUT config assignment")
	assertRequiredBearerScope(t, objectValue(t, configAssignment["delete"], "DELETE config assignment"), "machines:connect", "DELETE config assignment")
	disconnectMachine := objectValue(t, doc.Paths["/v1/machines/{machine_id}/disconnect"]["post"], "POST machine disconnect")
	assertRequiredBearerScope(t, disconnectMachine, "machines:connect", "POST machine disconnect")
	deleteMachine := objectValue(t, doc.Paths["/v1/machines/{machine_id}"]["delete"], "DELETE machine")
	assertRequiredBearerScope(t, deleteMachine, "machines:connect", "DELETE machine")
}

func TestOpenAPIOriginTLSIsReferenceOnlyAndModeBound(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	tlsSchema := doc.Components.Schemas["TunnelRouteTLS"]
	properties := objectValue(t, tlsSchema["properties"], "TunnelRouteTLS.properties")
	verification := objectValue(t, properties["verification"], "TunnelRouteTLS.verification")
	wantModes := map[string]bool{"system": true, "custom_ca": true, "insecure_development": true}
	gotModes := stringSet(t, verification["enum"], "TunnelRouteTLS.verification.enum")
	if !reflect.DeepEqual(gotModes, wantModes) {
		t.Fatalf("origin TLS modes = %#v, want %#v", gotModes, wantModes)
	}
	if _, exists := gotModes["mutual_tls"]; exists {
		t.Fatal("mTLS must be an orthogonal credential reference, not a verification mode")
	}
	reference := doc.Components.Schemas["OriginCredentialReference"]
	pattern, _ := reference["pattern"].(string)
	if reference["maxLength"] != float64(512) || !strings.Contains(pattern, "://paperboat/") {
		t.Fatalf("origin reference is not bounded and Paperboat-scoped: %#v", reference)
	}
	encoded, err := json.Marshal(tlsSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"BEGIN CERTIFICATE", "BEGIN PRIVATE KEY", "bearer_token", "private_key_pem"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("origin TLS schema contains secret material field %q", forbidden)
		}
	}
	if len(arrayValue(t, tlsSchema["allOf"], "TunnelRouteTLS.allOf")) != 2 {
		t.Fatalf("origin TLS mode/reference conditions missing: %#v", tlsSchema)
	}
}

func assertExactCLIScopes(t *testing.T, schema map[string]any, name string) {
	t.Helper()
	properties := objectValue(t, schema["properties"], name+".properties")
	scopes := objectValue(t, properties["scopes"], name+".scopes")
	if scopes["minItems"] != float64(12) || scopes["maxItems"] != float64(12) || scopes["uniqueItems"] != true {
		t.Fatalf("%s does not require exactly twelve unique scopes: %#v", name, scopes)
	}
	items := objectValue(t, scopes["items"], name+".scopes.items")
	actual := stringSet(t, items["enum"], name+".scopes.items.enum")
	expected := map[string]bool{
		"account:read": true, "clients:revoke": true, "machines:read": true,
		"machines:connect": true, "session:refresh": true, "diagnostics:upload": true,
		"previews:read": true, "previews:write": true, "tunnels:read": true,
		"tunnels:write": true, "operations:read": true, "operations:write": true,
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s scopes = %#v", name, actual)
	}
}

func assertRequiredBearerScope(t *testing.T, operation map[string]any, expected, name string) {
	t.Helper()
	actual := stringSet(t, operation["x-required-bearer-scopes"], name+".x-required-bearer-scopes")
	if !reflect.DeepEqual(actual, map[string]bool{expected: true}) {
		t.Fatalf("%s bearer scopes = %#v", name, actual)
	}
}

func assertSingletonConstScope(t *testing.T, raw any, expected, name string) {
	t.Helper()
	scopes := objectValue(t, raw, name)
	if scopes["minItems"] != float64(1) || scopes["maxItems"] != float64(1) || scopes["items"] != false {
		t.Fatalf("%s does not require exactly one scope: %#v", name, scopes)
	}
	prefixItems := arrayValue(t, scopes["prefixItems"], name+".prefixItems")
	if len(prefixItems) != 1 || objectValue(t, prefixItems[0], name+".prefixItems[0]")["const"] != expected {
		t.Fatalf("%s does not require %q: %#v", name, expected, scopes)
	}
}

func objectValue(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", label, value)
	}
	return object
}

func arrayValue(t *testing.T, value any, label string) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, want array", label, value)
	}
	return array
}

func stringSet(t *testing.T, value any, label string) map[string]bool {
	t.Helper()
	set := make(map[string]bool)
	for _, item := range arrayValue(t, value, label) {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("%s contains %T, want string", label, item)
		}
		set[text] = true
	}
	return set
}
