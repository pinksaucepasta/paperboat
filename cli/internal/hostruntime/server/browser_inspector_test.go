package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

func browserInspectorFixture(t *testing.T, principal string) (*Dispatcher, Authorization) {
	t.Helper()
	authorize := func(_ context.Context, grant, kind, resource, route, action string) (inspectorapi.Decision, error) {
		if grant != "grant_ok" || kind != "preview" || resource != "prv_1" || route != "prv_1" {
			return inspectorapi.Decision{}, inspectorapi.ErrDenied
		}
		now := time.Now().UTC()
		return inspectorapi.Decision{Principal: principal, Owner: "usr_owner", ResourceKind: kind, ResourceID: resource, RouteID: route, ResourceGeneration: 1, RouteGeneration: 1, TargetGeneration: 1, CredentialID: "iac_1", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
	}
	service, err := inspectorapi.New(inspectorapi.Config{Store: inspector.NewStore(), Registry: inspector.NewRegistry(), Authorize: authorize})
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{config: DispatcherConfig{Inspector: service}}
	a := Authorization{BrowserInspector: true, UserID: "usr_owner", AccountID: "usr_owner", MachineID: "machine_1", BrowserAttachmentID: "att_1", ClientID: "att_1"}
	return d, a
}

func browserInspectorCall(d *Dispatcher, a Authorization, payload string) (int, map[string]any, string) {
	outcome := d.browserInspector(context.Background(), a, json.RawMessage(payload))
	if outcome.ErrorCode != "" {
		return 0, nil, outcome.ErrorCode
	}
	var body struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	}
	if json.Unmarshal(outcome.Result, &body) != nil {
		return 0, nil, "bad_result"
	}
	return body.Status, body.Body, ""
}

const browserInspectorList = `{"grant":"grant_ok","method":"GET","path":"/v1/inspector/records","query":{"kind":"preview","resource":"prv_1","route":"prv_1"}}`

func TestBrowserInspectorListsRecordsForTheGrantPrincipal(t *testing.T) {
	d, a := browserInspectorFixture(t, "usr_owner")
	status, _, code := browserInspectorCall(d, a, `{"grant":"grant_ok","method":"PUT","path":"/v1/inspector/policy","body":{"resource_kind":"preview","resource_id":"prv_1","route_id":"prv_1","enabled":true}}`)
	if code != "" || status != 200 {
		t.Fatalf("enable status=%d code=%q", status, code)
	}
	status, body, code := browserInspectorCall(d, a, browserInspectorList)
	if code != "" || status != 200 || body["kind"] == nil {
		t.Fatalf("status=%d body=%v code=%q", status, body, code)
	}
}

func TestBrowserInspectorRefusesAnotherPrincipalsGrant(t *testing.T) {
	d, a := browserInspectorFixture(t, "usr_someone_else")
	if _, _, code := browserInspectorCall(d, a, browserInspectorList); code != "not_found_or_forbidden" {
		t.Fatalf("code=%q", code)
	}
}

func TestBrowserInspectorRefusesTerminalAuthorization(t *testing.T) {
	d, a := browserInspectorFixture(t, "usr_owner")
	a.BrowserInspector, a.BrowserTerminal = false, true
	if _, _, code := browserInspectorCall(d, a, browserInspectorList); code != "not_found_or_forbidden" {
		t.Fatalf("code=%q", code)
	}
}

func TestBrowserInspectorRejectsBadRequests(t *testing.T) {
	d, a := browserInspectorFixture(t, "usr_owner")
	for name, payload := range map[string]string{
		"unknown_path":   `{"grant":"grant_ok","method":"GET","path":"/v1/runtime"}`,
		"traversal":      `{"grant":"grant_ok","method":"GET","path":"/v1/inspector/records/../policy"}`,
		"wrong_method":   `{"grant":"grant_ok","method":"PATCH","path":"/v1/inspector/records"}`,
		"body_on_get":    `{"grant":"grant_ok","method":"GET","path":"/v1/inspector/records","body":{"a":1}}`,
		"missing_grant":  `{"method":"GET","path":"/v1/inspector/records"}`,
		"unknown_field":  `{"grant":"grant_ok","method":"GET","path":"/v1/inspector/records","extra":1}`,
		"policy_no_body": `{"grant":"grant_ok","method":"PUT","path":"/v1/inspector/policy"}`,
	} {
		if _, _, code := browserInspectorCall(d, a, payload); code != "invalid_request" {
			t.Fatalf("%s: code=%q", name, code)
		}
	}
}

func TestBrowserInspectorIsAdvertisedOnlyWhenConfigured(t *testing.T) {
	d, _ := browserInspectorFixture(t, "usr_owner")
	if !containsString(d.Capabilities(), "inspector.v1") || containsString((&Dispatcher{}).Capabilities(), "inspector.v1") {
		t.Fatal("inspector.v1 advertisement does not follow configuration")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
