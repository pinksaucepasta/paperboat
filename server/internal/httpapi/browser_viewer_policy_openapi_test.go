package httpapi

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestOpenAPIFreezesBrowserViewerPolicy(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components map[string]any            `json:"components"`
		Paths      map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("openapi json is invalid: %v", err)
	}
	const path = "/v1/browser-access/policies/{resource_kind}/{resource_id}/{route_id}"
	item := doc.Paths[path]
	if item == nil {
		t.Fatalf("missing %s", path)
	}
	get := objectValue(t, item["get"], "browser viewer policy GET")
	put := objectValue(t, item["put"], "browser viewer policy PUT")
	if get["operationId"] != "browser_access_viewer_policy_get" || put["operationId"] != "browser_access_viewer_policy_put" {
		t.Fatalf("unexpected operation IDs: GET=%v PUT=%v", get["operationId"], put["operationId"])
	}
	assertResponseSchema(t, get, "200", "#/components/schemas/BrowserViewerPolicyResponse")
	assertRequestSchema(t, put, "#/components/schemas/BrowserViewerPolicyMutation")
	assertResponseSchema(t, put, "200", "#/components/schemas/BrowserViewerPolicyResponse")
	assertResponseSchema(t, put, "202", "#/components/schemas/BrowserViewerPolicyResponse")
	assertExactResponseStatuses(t, get, "browser viewer policy GET", "200", "400", "401", "404", "503")
	assertExactResponseStatuses(t, put, "browser viewer policy PUT", "200", "202", "400", "401", "404", "409", "503")
	if !hasSecurityPair(get, "cookieSession", "") || !hasSecurityPair(put, "cookieSession", "csrfHeader") {
		t.Fatal("viewer policy operations must use a browser session; PUT must also require CSRF")
	}

	schemas := objectValue(t, doc.Components["schemas"], "components.schemas")
	policy := objectValue(t, schemas["BrowserViewerPolicy"], "BrowserViewerPolicy")
	mutation := objectValue(t, schemas["BrowserViewerPolicyMutation"], "BrowserViewerPolicyMutation")
	wantAudience := []any{"owner", "team", "specific_emails", "email_domain", "public"}
	policyProps := objectValue(t, policy["properties"], "BrowserViewerPolicy.properties")
	if got := arrayValue(t, objectValue(t, policyProps["serving_state"], "serving state")["enum"], "serving state enum"); !reflect.DeepEqual(got, []any{"fencing", "pending", "ready", "not_applicable"}) {
		t.Fatalf("serving states = %#v", got)
	}
	mutationProps := objectValue(t, mutation["properties"], "BrowserViewerPolicyMutation.properties")
	if got := arrayValue(t, objectValue(t, policyProps["audience"], "policy audience")["enum"], "policy audience enum"); !reflect.DeepEqual(got, wantAudience) {
		t.Fatalf("policy audiences = %#v, want %#v", got, wantAudience)
	}
	if got := arrayValue(t, objectValue(t, mutationProps["audience"], "mutation audience")["enum"], "mutation audience enum"); !reflect.DeepEqual(got, wantAudience) {
		t.Fatalf("mutation audiences = %#v, want %#v", got, wantAudience)
	}
	if min, ok := objectValue(t, mutationProps["expected_generation"], "expected_generation")["minimum"].(float64); !ok || min != 0 {
		t.Fatalf("first-write generation minimum = %v, want 0", mutationProps["expected_generation"])
	}
}

func hasSecurityPair(operation map[string]any, first, second string) bool {
	security, ok := operation["security"].([]any)
	if !ok || len(security) != 1 {
		return false
	}
	entry, ok := security[0].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := entry[first].([]any); !ok {
		return false
	}
	if second == "" {
		return len(entry) == 1
	}
	_, ok = entry[second].([]any)
	return ok && len(entry) == 2
}
