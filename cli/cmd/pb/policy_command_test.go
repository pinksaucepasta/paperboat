package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// policyFake serves the shared/policy/README.md contract for workspace "acme": each route
// answers with the next queued status and body, and records the request bodies.
type policyFake struct {
	mu       sync.Mutex
	replies  map[string][]policyReply
	requests map[string][]map[string]any
}

type policyReply struct {
	status int
	body   string
}

func newPolicyFake(t *testing.T) (*policyFake, string) {
	t.Helper()
	fake := &policyFake{replies: map[string][]policyReply{}, requests: map[string][]map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" && !strings.HasPrefix(r.URL.RawQuery, "workspace=") {
			key += "?" + r.URL.RawQuery
		}
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				t.Errorf("%s: request body is not JSON: %v", key, err)
			}
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.requests[key] = append(fake.requests[key], body)
		queue := fake.replies[key]
		if len(queue) == 0 {
			t.Errorf("unexpected request %s", key)
			http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
			return
		}
		reply := queue[0]
		if len(queue) > 1 {
			fake.replies[key] = queue[1:]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (f *policyFake) on(key string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[key] = append(f.replies[key], policyReply{status, body})
}

func (f *policyFake) sent(key string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[key]
}

func data(value string) string { return `{"data":` + value + `}` }

func apiFailure(code, details string) string {
	return `{"error":{"code":"` + code + `","message":"server text","details":` + details + `}}`
}

const (
	policyBase      = "/v1/workspaces/acme/policy"
	testPolicyText  = "{\n  // keep me\n  \"rules\": []\n}\n"
	testPolicyJSON  = `{"workspace":"acme","version":7,"policy":"{\n  // keep me\n  \"rules\": []\n}\n","digest":"sha256:ab","updated_at":"2026-10-20T10:00:00Z","updated_by":"user:ana@example.test","message":"m","can_edit":"all","names":{"users":["ana@example.test"],"machines":["db"],"previews":[],"tunnels":[],"tags":[],"groups":[]},"future_field":1}`
	savedPolicyJSON = `{"workspace":"acme","version":8,"policy":"x","digest":"sha256:cd","updated_at":"2026-10-20T10:00:00Z","updated_by":"user:ana@example.test","message":"m","can_edit":"all","names":{}}`
	changesJSON     = `[{"change":"gained","subject":"user:bo@example.test","target":"machine:db","action":"exec","rules":["/rules/0"]},{"change":"lost","subject":"user:cy@example.test","target":"machine:db","action":"private_access","ports":[5432,"8000-8080"]}]`
)

func runPolicyCLI(t *testing.T, serverURL string, args ...string) (int, string, string) {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, serverURL)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"--config", configPath, "--workspace", "acme"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writePolicyDraft(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.hujson")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPolicyGetPrintsFileTextAndJSON(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+policyBase, 200, data(testPolicyJSON))
	fake.on("GET "+policyBase+"?version=3", 200, data(testPolicyJSON))
	if code, out, errOut := runPolicyCLI(t, url, "policy", "get"); code != 0 || out != testPolicyText {
		t.Fatalf("code=%d out=%q err=%s", code, out, errOut)
	}
	code, out, errOut := runPolicyCLI(t, url, "policy", "get", "--version", "3", "--json")
	var got struct {
		Version int    `json:"version"`
		CanEdit string `json:"can_edit"`
		Policy  string `json:"policy"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || got.Version != 7 || got.CanEdit != "all" || got.Policy != testPolicyText {
		t.Fatalf("code=%d out=%q err=%s decode=%v", code, out, errOut, err)
	}
}

func TestPolicySetRejectsLocalErrorsBeforeCallingServer(t *testing.T) {
	_, url := newPolicyFake(t)
	path := writePolicyDraft(t, `{"rules": [{"src": 1}], "bogus": true}`)
	code, _, errOut := runPolicyCLI(t, url, "policy", "set", path, "--yes")
	if code == 0 || !strings.Contains(errOut, "/bogus: ") || !strings.Contains(errOut, "error(s)") {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
}

func TestPolicySetDryRunPrintsChangesAndDoesNotSave(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+policyBase, 200, data(testPolicyJSON))
	fake.on("POST "+policyBase+"/validate", 200, data(`{"valid":true,"errors":[],"tests":{"passed":2,"failed":0},"access_changes":`+changesJSON+`,"confirm":"sha256:c1"}`))
	code, out, errOut := runPolicyCLI(t, url, "policy", "set", writePolicyDraft(t, testPolicyText), "--dry-run")
	for _, want := range []string{"Tests: 2 passed, 0 failed", "+ gained user:bo@example.test machine:db exec (/rules/0)", "- lost user:cy@example.test machine:db private_access ports 5432,8000-8080"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if code != 0 || len(fake.sent("PUT "+policyBase)) != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	if got := fake.sent("POST " + policyBase + "/validate"); len(got) != 1 || got[0]["policy"] != testPolicyText {
		t.Fatalf("validate body=%v", got)
	}
}

func TestPolicySetConfirmsChangesAndRetriesAfter412(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+policyBase, 200, data(testPolicyJSON))
	fake.on("POST "+policyBase+"/validate", 200, data(`{"valid":true,"errors":[],"tests":{"passed":0,"failed":0},"access_changes":`+changesJSON+`,"confirm":"sha256:c1"}`))
	fake.on("PUT "+policyBase, 412, apiFailure("policy_changes_unconfirmed", `{"access_changes":`+changesJSON+`,"confirm":"sha256:c2"}`))
	fake.on("PUT "+policyBase, 200, data(savedPolicyJSON))
	code, out, errOut := runPolicyCLI(t, url, "policy", "set", writePolicyDraft(t, testPolicyText), "--yes", "--message", "why")
	if code != 0 || !strings.Contains(out, "Saved policy version 8") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	puts := fake.sent("PUT " + policyBase)
	if len(puts) != 2 || puts[0]["confirm"] != "sha256:c1" || puts[1]["confirm"] != "sha256:c2" || puts[1]["expected_version"] != float64(7) || puts[1]["message"] != "why" || puts[1]["policy"] != testPolicyText {
		t.Fatalf("puts=%v", puts)
	}
}

func TestPolicySetWithoutYesNeedsTerminalConfirmation(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+policyBase, 200, data(testPolicyJSON))
	fake.on("POST "+policyBase+"/validate", 200, data(`{"valid":true,"errors":[],"tests":{"passed":0,"failed":0},"access_changes":`+changesJSON+`,"confirm":"sha256:c1"}`))
	code, _, errOut := runPolicyCLI(t, url, "policy", "set", writePolicyDraft(t, testPolicyText))
	if code == 0 || !strings.Contains(errOut, "--yes") || len(fake.sent("PUT "+policyBase)) != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
}

func TestPolicySetWithoutAccessChangesSavesWithoutConfirm(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+policyBase, 200, data(testPolicyJSON))
	fake.on("POST "+policyBase+"/validate", 200, data(`{"valid":true,"errors":[],"tests":{"passed":0,"failed":0},"access_changes":[],"confirm":"sha256:e"}`))
	fake.on("PUT "+policyBase, 200, data(savedPolicyJSON))
	code, out, errOut := runPolicyCLI(t, url, "policy", "set", writePolicyDraft(t, testPolicyText))
	if puts := fake.sent("PUT " + policyBase); code != 0 || len(puts) != 1 || puts[0]["confirm"] != nil || !strings.Contains(out, "No access changes.") {
		t.Fatalf("code=%d out=%s err=%s puts=%v", code, out, errOut, puts)
	}
}

func TestPolicySaveErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		status    int
		code      string
		details   string
		wantError string
	}{
		{409, "policy_version_conflict", `{}`, "changed by someone else"},
		{422, "invalid_policy", `{"errors":[{"path":"/rules/0/dst","code":"unknown_name","message":"no machine db2"}]}`, "/rules/0/dst: unknown_name: no machine db2"},
		{403, "policy_edit_forbidden", `{}`, "Owners and admins edit the policy"},
		{412, "policy_changes_unconfirmed", `{"access_changes":` + changesJSON + `,"confirm":"sha256:c2"}`, "was not confirmed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fake, url := newPolicyFake(t)
			fake.on("GET "+policyBase, 200, data(testPolicyJSON))
			fake.on("POST "+policyBase+"/validate", 200, data(`{"valid":true,"errors":[],"tests":{"passed":0,"failed":0},"access_changes":[],"confirm":""}`))
			fake.on("PUT "+policyBase, tc.status, apiFailure(tc.code, tc.details))
			code, out, errOut := runPolicyCLI(t, url, "policy", "set", writePolicyDraft(t, testPolicyText), "--yes")
			if code == 0 || !strings.Contains(out+errOut, tc.wantError) || strings.Contains(out+errOut, "server text") {
				t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
			}
			if tc.status == 412 && len(fake.sent("PUT "+policyBase)) != 2 {
				t.Fatalf("412 is retried exactly once, got %d", len(fake.sent("PUT "+policyBase)))
			}
		})
	}
}

func TestPolicyTestExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name, validation string
		ok               bool
	}{
		{"valid", `{"valid":true,"errors":[],"tests":{"passed":3,"failed":0},"access_changes":[]}`, true},
		{"failed test", `{"valid":true,"errors":[],"tests":{"passed":2,"failed":1},"access_changes":[]}`, false},
		{"invalid", `{"valid":false,"errors":[{"path":"/rules","code":"bad","message":"m"}],"tests":{"passed":0,"failed":0},"access_changes":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, url := newPolicyFake(t)
			fake.on("GET "+policyBase, 200, data(testPolicyJSON))
			fake.on("POST "+policyBase+"/validate", 200, data(tc.validation))
			code, out, errOut := runPolicyCLI(t, url, "policy", "test")
			if (code == 0) != tc.ok {
				t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
			}
			if got := fake.sent("POST " + policyBase + "/validate"); len(got) != 1 || got[0]["policy"] != testPolicyText {
				t.Fatalf("the saved policy is tested by default: %v", got)
			}
		})
	}
}

func TestPolicyExplainDecisionAndTables(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("POST "+policyBase+"/explain", 200, data(`{"decision":{"allowed":false,"reason":"port_not_allowed","rules":["/rules/1"],"guardrail":"team_suspended"}}`))
	fake.on("POST "+policyBase+"/explain", 200, data(`{"who_can":[{"subject":"user:ana@example.test","target":"machine:db","action":"exec","reason":"owner"}]}`))
	code, out, errOut := runPolicyCLI(t, url, "policy", "explain", "--src", "user:ana@example.test", "--dst", "machine:db", "--action", "private_access", "--port", "22", "--file", writePolicyDraft(t, testPolicyText), "--api-token", "ci")
	if code != 0 || !strings.Contains(out, "DENIED: port_not_allowed") || !strings.Contains(out, "Rules: /rules/1") || !strings.Contains(out, "Guardrail: team_suspended") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	sent := fake.sent("POST " + policyBase + "/explain")[0]
	if sent["port"] != float64(22) || sent["api_token"] != "ci" || sent["policy"] != testPolicyText || sent["src"] != "user:ana@example.test" {
		t.Fatalf("explain body=%v", sent)
	}
	code, out, errOut = runPolicyCLI(t, url, "policy", "explain", "--dst", "machine:db", "--action", "exec", "--json")
	var got struct {
		WhoCan []struct{ Subject string } `json:"who_can"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || len(got.WhoCan) != 1 {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	if code, _, _ := runPolicyCLI(t, url, "policy", "explain", "--dst", "machine:db"); code == 0 {
		t.Fatal("--dst without --action must be rejected")
	}
}

func TestPolicyExplainForbidden(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("POST "+policyBase+"/explain", 403, apiFailure("policy_explain_forbidden", `{}`))
	code, out, errOut := runPolicyCLI(t, url, "policy", "explain", "--src", "user:bo@example.test")
	if code == 0 || !strings.Contains(out+errOut, "owners and admins can explain") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
}

func TestPolicyVersionsAndRestoreConfirmFlow(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+policyBase+"/versions", 200, data(`{"versions":[{"version":7,"digest":"sha256:ab","created_at":"2026-10-20T10:00:00Z","author":"user:ana@example.test","api_token":"ci","message":"tidy","gained":1,"lost":2,"kind":"saved"}]}`))
	code, out, errOut := runPolicyCLI(t, url, "policy", "versions")
	if code != 0 || !strings.Contains(out, "+1 -2") || !strings.Contains(out, "(token ci)") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	code, out, _ = runPolicyCLI(t, url, "policy", "versions", "--json")
	var versions struct{ Versions []struct{ Version int } }
	if err := json.Unmarshal([]byte(out), &versions); code != 0 || err != nil || versions.Versions[0].Version != 7 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	fake.on("GET "+policyBase, 200, data(testPolicyJSON))
	fake.on("POST "+policyBase+"/restore", 412, apiFailure("policy_changes_unconfirmed", `{"access_changes":`+changesJSON+`,"confirm":"sha256:c9"}`))
	fake.on("POST "+policyBase+"/restore", 200, data(savedPolicyJSON))
	code, out, errOut = runPolicyCLI(t, url, "policy", "restore", "3", "--yes")
	restores := fake.sent("POST " + policyBase + "/restore")
	if code != 0 || !strings.Contains(out, "+ gained") || len(restores) != 2 || restores[0]["confirm"] != nil || restores[1]["confirm"] != "sha256:c9" || restores[1]["version"] != float64(3) || restores[1]["expected_version"] != float64(7) {
		t.Fatalf("code=%d out=%s err=%s restores=%v", code, out, errOut, restores)
	}
}

func TestPolicyRestoreUnconfirmedWithoutYes(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+policyBase, 200, data(testPolicyJSON))
	fake.on("POST "+policyBase+"/restore", 412, apiFailure("policy_changes_unconfirmed", `{"access_changes":`+changesJSON+`,"confirm":"sha256:c9"}`))
	code, _, errOut := runPolicyCLI(t, url, "policy", "restore", "3")
	if code == 0 || !strings.Contains(errOut, "--yes") || len(fake.sent("POST "+policyBase+"/restore")) != 1 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
}

func TestPolicySchemaIsOffline(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"policy", "schema"}, &stdout, &stderr); code != 0 || !bytes.Equal(stdout.Bytes(), policy.Schema) {
		t.Fatalf("code=%d err=%s", code, stderr.String())
	}
}
