package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestIssueNativePrivateGrantValidatesBoundTarget(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/native-private-access/grants" || r.Header.Get("Authorization") != "Bearer device-token" {
			t.Fatalf("request=%s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var request NativePrivateGrantRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.OperationID != "operation_native_1" || request.ResourceID != "tun_1" || request.RouteID != "route_1" || request.Protocol != "tcp" {
			t.Fatalf("request=%+v err=%v", request, err)
		}
		writeData(w, http.StatusOK, map[string]any{
			"target":     map[string]any{"account_id": "usr_1", "user_id": "usr_1", "environment_id": "env_1", "machine_id": "mch_1", "access_session_id": "umas_1", "resource_kind": "tunnel", "resource_id": "tun_1", "resource_generation": 2, "route_id": "route_1", "route_generation": 3, "target_generation": 4, "protocol": "tcp", "target_scheme": "tcp", "target_address": "127.0.0.1:5432"},
			"credential": "signed-native-private-credential", "expires_at": expiresAt,
		})
	}))
	defer server.Close()
	grant, err := New(server.URL, config.Credential{AccessToken: "device-token"}, nil).IssueNativePrivateGrant(context.Background(), NativePrivateGrantRequest{OperationID: "operation_native_1", ResourceKind: "tunnel", ResourceID: "tun_1", RouteID: "route_1", Protocol: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := grant.Binding()
	if err != nil || len(binding) == 0 {
		t.Fatalf("binding=%q err=%v", binding, err)
	}
}

func TestIssueNativePrivateGrantRejectsUnsafeServerTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeData(w, http.StatusOK, map[string]any{
			"target":     map[string]any{"account_id": "usr_1", "user_id": "usr_1", "environment_id": "env_1", "machine_id": "mch_1", "access_session_id": "umas_1", "resource_kind": "preview", "resource_id": "prv_1", "resource_generation": 1, "route_id": "prv_1", "route_generation": 1, "target_generation": 1, "protocol": "http", "target_scheme": "http", "target_address": "192.0.2.1:3000"},
			"credential": "credential", "expires_at": time.Now().UTC().Add(time.Minute),
		})
	}))
	defer server.Close()
	if _, err := New(server.URL, config.Credential{AccessToken: "token"}, nil).IssueNativePrivateGrant(context.Background(), NativePrivateGrantRequest{}); err == nil {
		t.Fatal("unsafe non-loopback target accepted")
	}
}
