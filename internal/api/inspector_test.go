package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func inspectorIssueServer(t *testing.T, status int, body string, seen *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/inspector/credentials" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") == "" {
			t.Error("issuance carried no user authorization")
		}
		var decoded map[string]any
		_ = json.NewDecoder(request.Body).Decode(&decoded)
		*seen = decoded
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
}

func TestIssueInspectorCredential(t *testing.T) {
	var seen map[string]any
	server := inspectorIssueServer(t, http.StatusOK, `{"data":{"credential_id":"iac_01","token":"iat_secret","expires_at":"2026-09-09T01:00:00Z"}}`, &seen)
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "user-token"}, server.Client())
	credential, err := client.IssueInspectorCredential(context.Background(), "tunnel", "tun_01", "rte_01", "replay")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != "iat_secret" || credential.CredentialID != "iac_01" || credential.ExpiresAt.IsZero() {
		t.Fatalf("credential = %+v", credential)
	}
	if seen["resource_kind"] != "tunnel" || seen["resource_id"] != "tun_01" || seen["route_id"] != "rte_01" || seen["action"] != "replay" {
		t.Fatalf("issuance scope = %v", seen)
	}
	if credential.ExpiresAt.Before(time.Now().UTC()) {
		t.Fatalf("credential already expired: %+v", credential)
	}
}

func TestIssueInspectorCredentialDeniesAndRejectsUnsafe(t *testing.T) {
	var seen map[string]any
	server := inspectorIssueServer(t, http.StatusForbidden, `{"error":{"code":"inspector_not_authorized","message":"denied"}}`, &seen)
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "user-token"}, server.Client())
	_, err := client.IssueInspectorCredential(context.Background(), "preview", "prv_01", "prv_01", "replay")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "inspector_not_authorized" || apiErr.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want inspector_not_authorized denial", err)
	}

	empty := inspectorIssueServer(t, http.StatusOK, `{"data":{"credential_id":"","token":"","expires_at":"0001-01-01T00:00:00Z"}}`, &seen)
	defer empty.Close()
	emptyClient := New(empty.URL, config.Credential{AccessToken: "user-token"}, empty.Client())
	if _, err := emptyClient.IssueInspectorCredential(context.Background(), "tunnel", "tun_01", "rte_01", "inspect"); err == nil {
		t.Fatal("unsafe credential accepted")
	}
}
