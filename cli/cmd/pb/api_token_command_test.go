package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	tokensPath    = "/v1/workspaces/acme/api-tokens"
	testTokenJSON = `{"id":"apitok_1","name":"ci","workspace":"acme","created_by":"user:ana@example.test","created_at":"2026-10-20T10:00:00Z","expires_at":"2099-01-01T00:00:00Z","scope":[{"dst":["tag:ci"],"allow":["exec"],"ports":[22]}]}`
)

func TestAPITokenCreatePrintsSecretOnce(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("POST "+tokensPath, 200, data(`{"api_token":`+testTokenJSON+`,"secret":"pbat_secret"}`))
	fake.on("POST "+tokensPath, 200, data(`{"api_token":`+testTokenJSON+`,"secret":"pbat_secret"}`))
	code, out, errOut := runPolicyCLI(t, url, "api-token", "create", "ci", "--expires", "30d", "--scope", `[{"dst":["tag:ci"],"allow":["exec"]}]`)
	if code != 0 || out != "pbat_secret\n" || !strings.Contains(errOut, "shown once") {
		t.Fatalf("code=%d out=%q err=%s", code, out, errOut)
	}
	sent := fake.sent("POST " + tokensPath)[0]
	if sent["name"] != "ci" || sent["expires_in_days"] != float64(30) || sent["scope"] == nil {
		t.Fatalf("create body=%v", sent)
	}
	code, out, _ = runPolicyCLI(t, url, "api-token", "create", "ci", "--json")
	var got struct {
		Secret   string              `json:"secret"`
		APIToken struct{ ID string } `json:"api_token"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || got.Secret != "pbat_secret" || got.APIToken.ID != "apitok_1" {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if body := fake.sent("POST " + tokensPath)[1]; body["expires_in_days"] != float64(90) || body["scope"] != nil {
		t.Fatalf("defaults body=%v", body)
	}
}

func TestAPITokenCreateRejectsBadInputLocally(t *testing.T) {
	_, url := newPolicyFake(t)
	for _, args := range [][]string{
		{"--expires", "soon"},
		{"--scope", `{"dst":"x"}`},
		{"--scope", `[{"dst":["tag:ci"],"allow":["exec"],"typo":1}]`},
		{"--scope", `[]`, "--scope-file", "x.json"},
	} {
		if code, _, _ := runPolicyCLI(t, url, append([]string{"api-token", "create", "ci"}, args...)...); code == 0 {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestAPITokenCreateServerErrors(t *testing.T) {
	for _, tc := range []struct {
		status              int
		code, details, want string
	}{
		{409, "api_token_name_taken", `{}`, "already have an API token with this name"},
		{422, "invalid_api_token_scope", `{"errors":[{"path":"/0/dst/0","code":"unknown_name","message":"no tag ci"}]}`, "/0/dst/0: unknown_name: no tag ci"},
	} {
		fake, url := newPolicyFake(t)
		fake.on("POST "+tokensPath, tc.status, apiFailure(tc.code, tc.details))
		if code, out, errOut := runPolicyCLI(t, url, "api-token", "create", "ci"); code == 0 || !strings.Contains(out+errOut, tc.want) {
			t.Errorf("%s: code=%d out=%s err=%s", tc.code, code, out, errOut)
		}
	}
}

func TestAPITokenListAndRevoke(t *testing.T) {
	fake, url := newPolicyFake(t)
	list := data(`{"api_tokens":[` + testTokenJSON + `,{"id":"apitok_2","name":"old","workspace":"acme","created_by":"user:ana@example.test","created_at":"2026-10-01T00:00:00Z","expires_at":"2026-10-02T00:00:00Z","revoked_at":"2026-10-01T01:00:00Z"}]}`)
	for range 4 {
		fake.on("GET "+tokensPath, 200, list)
	}
	code, out, errOut := runPolicyCLI(t, url, "api-token", "list")
	if code != 0 || !strings.Contains(out, "apitok_1") || !strings.Contains(out, "revoked") || strings.Contains(out, "pbat_") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	code, out, _ = runPolicyCLI(t, url, "api-token", "list", "--json")
	var got struct {
		APITokens []struct{ ID string } `json:"api_tokens"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || len(got.APITokens) != 2 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if code, _, errOut := runPolicyCLI(t, url, "api-token", "revoke", "old", "--yes"); code == 0 || !strings.Contains(errOut, "no active API token") {
		t.Fatalf("a revoked token's name must not match: code=%d err=%s", code, errOut)
	}
	fake.on("DELETE "+tokensPath+"/apitok_1", 200, data(`{"api_token":`+testTokenJSON+`}`))
	if code, out, errOut := runPolicyCLI(t, url, "api-token", "revoke", "ci", "--yes"); code != 0 || !strings.Contains(out, "Revoked API token ci (apitok_1)") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
}
