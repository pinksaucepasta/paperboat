package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestWorkspaceListIsUnscopedAndMachineListIsScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/workspaces":
			if r.URL.RawQuery != "" {
				t.Fatalf("workspace list query = %q, want unscoped", r.URL.RawQuery)
			}
			writeData(w, http.StatusOK, WorkspacePage{Items: []Workspace{
				{ID: "personal", Name: "Personal", Kind: "personal", Role: "owner"},
				{ID: "team-a", Name: "Team A", Kind: "team", Role: "member"},
			}})
		case "/v1/machines":
			if got := r.URL.Query().Get("workspace"); got != "team-a" {
				t.Fatalf("machine workspace = %q, want team-a", got)
			}
			writeData(w, http.StatusOK, UserMachinePage{Items: []UserMachine{}, Pagination: Pagination{Limit: 200, Offset: 0, Total: 0}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	client := New(server.URL, config.Credential{AccessToken: "session"}, server.Client())
	if err := client.SetWorkspace("team-a"); err != nil {
		t.Fatal(err)
	}
	page, err := client.ListWorkspaces(context.Background())
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("workspaces = %#v, err=%v", page, err)
	}
	if _, err := client.ListUserMachines(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSelectorMatchesCanonicalTeamSlug(t *testing.T) {
	valid := []string{"personal", "a", "0", "research-team", strings.Repeat("a", 63)}
	for _, selector := range valid {
		if err := ValidateWorkspaceSelector(selector); err != nil {
			t.Errorf("ValidateWorkspaceSelector(%q) = %v, want nil", selector, err)
		}
	}
	invalid := []string{"", "Personal", "-team", "team-", "team.slug", "team_name", "Team", strings.Repeat("a", 64), "research-team!"}
	for _, selector := range invalid {
		if err := ValidateWorkspaceSelector(selector); err == nil {
			t.Errorf("ValidateWorkspaceSelector(%q) = nil, want an error", selector)
		}
	}
}

func TestScopedPreviewCreateBindsWorkspaceIntoQueryAndProof(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/previews" || r.URL.Query().Get("workspace") != "team-a" {
			t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var input PreviewLeaseCreateRequest
		if err := json.Unmarshal(body, &input); err != nil {
			t.Fatal(err)
		}
		if input.Workspace != "team-a" {
			t.Fatalf("body workspace = %q, want team-a", input.Workspace)
		}
		wantProof := "preview-operation|POST|" + r.URL.Path + "|" + string(body)
		gotProof, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || string(gotProof) != wantProof {
			t.Fatalf("proof = %q, want %q, err=%v", gotProof, wantProof, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"workspace_unavailable","message":"not available"}}`)
	}))
	defer server.Close()

	client := New(server.URL, config.Credential{AccessToken: "session"}, server.Client())
	if err := client.SetWorkspace("team-a"); err != nil {
		t.Fatal(err)
	}
	client.SetMachineAuth(machineAuthTestSource{})
	_, err := client.CreatePreviewLease(context.Background(), PreviewLeaseCreateRequest{
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"},
	}, "preview-operation")
	var workspaceErr *WorkspaceAccessError
	if err == nil || !strings.Contains(err.Error(), "workspace \"team-a\"") || !errors.As(err, &workspaceErr) {
		t.Fatalf("error = %v, want actionable workspace access error", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one create request", requests)
	}
}

func TestScopedTunnelCreateBindsWorkspaceIntoQueryAndProof(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tunnels" || r.URL.Query().Get("workspace") != "team-a" {
			t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var input TunnelCreateInput
		if err := json.Unmarshal(body, &input); err != nil {
			t.Fatal(err)
		}
		if input.Workspace != "team-a" {
			t.Fatalf("body workspace = %q, want team-a", input.Workspace)
		}
		wantProof := "tunnel-operation|POST|" + r.URL.Path + "|" + string(body)
		gotProof, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || string(gotProof) != wantProof {
			t.Fatalf("proof = %q, want %q, err=%v", gotProof, wantProof, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"workspace_unavailable","message":"not available"}}`)
	}))
	defer server.Close()

	client := New(server.URL, config.Credential{AccessToken: "session"}, server.Client())
	if err := client.SetWorkspace("team-a"); err != nil {
		t.Fatal(err)
	}
	client.SetMachineAuth(machineAuthTestSource{})
	_, err := client.CreateTunnelV1(context.Background(), TunnelCreateInput{
		Name: "docs", AccessMode: "private", Origin: TunnelOriginInput{Scheme: "http", Address: "127.0.0.1:8080"},
	}, "tunnel-operation")
	var workspaceErr *WorkspaceAccessError
	if err == nil || !strings.Contains(err.Error(), "workspace \"team-a\"") || !errors.As(err, &workspaceErr) {
		t.Fatalf("error = %v, want actionable workspace access error", err)
	}
}

func TestScopedPermissionDeniedIsNotMisreportedAsWorkspaceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/machines" || r.URL.Query().Get("workspace") != "team-a" {
			t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"machine_access_denied","message":"permission denied"}}`)
	}))
	defer server.Close()

	client := New(server.URL, config.Credential{AccessToken: "session"}, server.Client())
	if err := client.SetWorkspace("team-a"); err != nil {
		t.Fatal(err)
	}
	_, err := client.ListUserMachines(context.Background())
	var workspaceErr *WorkspaceAccessError
	if err == nil || errors.As(err, &workspaceErr) {
		t.Fatalf("error = %v, want ordinary machine permission denial", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "machine_access_denied" {
		t.Fatalf("error = %v, want machine_access_denied API error", err)
	}
}

func TestWorkspaceAccessErrorUsesOnlyCanonicalSelectorErrorCodes(t *testing.T) {
	client := New("https://api.example.test", config.Credential{}, nil)
	if err := client.SetWorkspace("team-a"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		code   string
		status int
		want   bool
	}{
		{code: "workspace_unavailable", status: http.StatusForbidden, want: true},
		{code: "invalid_workspace", status: http.StatusBadRequest, want: true},
		{code: "machine_access_denied", status: http.StatusForbidden, want: false},
		{code: "preview_workspace_policy_denied", status: http.StatusForbidden, want: false},
	} {
		err := client.workspaceRequestError("/v1/machines", &APIError{Status: test.status, Code: test.code})
		var workspaceErr *WorkspaceAccessError
		if got := errors.As(err, &workspaceErr); got != test.want {
			t.Errorf("workspaceRequestError(%q) classified unavailable=%t, want %t", test.code, got, test.want)
		}
	}
}
