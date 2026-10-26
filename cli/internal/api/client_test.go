package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func TestConfigSyncStatusPreservesFileRecovery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config-sync/status" || r.Header.Get("Authorization") != "Bearer native" {
			t.Fatal("incorrect authenticated status request")
		}
		writeData(w, http.StatusOK, map[string]any{"state": "pending", "environments": []map[string]any{{"machine_id": "machine", "state": "pending", "error_code": "configuration_changed", "recovery_actions": []string{"apply_configuration"}, "managed_path_count": 2}}})
	}))
	defer server.Close()
	status, err := New(server.URL, config.Credential{AccessToken: "native"}, nil).ConfigSyncStatus(context.Background())
	if err != nil || len(status.Environments) != 1 {
		t.Fatalf("status unavailable: %v", err)
	}
	item := status.Environments[0]
	if item.ErrorCode != "configuration_changed" || len(item.RecoveryActions) != 1 || item.RecoveryActions[0] != "apply_configuration" || item.ManagedPathCount != 2 {
		t.Fatal("file-edit recovery or reconciled counters were lost")
	}
}

func TestClientConfigurationUsesServerOwnedURLWithoutAuthentication(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/client-configuration" {
			http.NotFound(w, r)
			return
		}
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("authorization=%q", authorization)
		}
		writeData(w, http.StatusOK, ClientConfiguration{
			Version:            "1",
			CLIVerificationURL: "https://dashboard.paperboat.test/cli/authorize",
			MachinesURL:        "https://dashboard.paperboat.test/dashboard/machines",
		})
	}))
	defer srv.Close()

	got, err := New(srv.URL, config.Credential{}, nil).ClientConfiguration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.MachinesURL != "https://dashboard.paperboat.test/dashboard/machines" {
		t.Fatalf("machines_url=%q", got.MachinesURL)
	}
}

func TestMachineExecDescriptorBindsSourceAndOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/um_1/exec-descriptor" {
			http.NotFound(w, r)
			return
		}
		var body operationDescriptorRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.SourceMachineID != "um_source" || body.OperationID != "operation_exec_1" || !body.NativeOnlyAccepted {
			t.Fatalf("body=%#v", body)
		}
		expiresAt := time.Now().Add(time.Minute)
		writeData(w, http.StatusOK, ExecDescriptor{OperationID: "operation_exec_1", Environment: &Environment{ID: "env_1", Kind: "machine", ResourceID: "um_1", State: "ready", Root: "/workspace"}, Endpoints: TerminalEndpoints{QUIC: "quic://machine.test:443", WSS: "wss://machine.test/v1/runtime"}, Auth: AuthMaterial{Method: "bearer", Token: "exec-token", ExpiresAt: expiresAt, Scopes: []string{"exec:operate"}}, ExpiresAt: expiresAt})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, nil)
	client.SetSourceMachineID("um_source")
	descriptor, err := client.MachineExecDescriptor(context.Background(), "um_1", "operation_exec_1")
	if err != nil || descriptor.Auth.Token != "exec-token" || descriptor.Environment.Root != "/workspace" {
		t.Fatalf("descriptor=%#v err=%v", descriptor, err)
	}
}

// N3: after a restart or an outage the machine's edge runtime carrier is not current, and the server
// refused exec with machine_not_ready for 30 s to 8 min although exec runs over the native peer
// connection. The client asks for a native-only descriptor and accepts one without edge endpoints;
// half an edge endpoint pair is still refused.
func TestOperationDescriptorAcceptsNativeOnlyAnswers(t *testing.T) {
	// "path ingress" is what a path-based edge ingress produces (staging): a wss endpoint under a prefix, no QUIC.
	refused := map[string]bool{"half an edge pair": true, "quic without wss": true, "path ingress with quic": true, "wss not the runtime": true}
	for name, endpoints := range map[string]TerminalEndpoints{
		"native only": {}, "edge": {QUIC: "quic://machine.test:443", WSS: "wss://machine.test/v1/runtime"},
		"path ingress":           {WSS: "wss://192.168.31.149/v1/runtime/machine-abc.192.168.31.149/v1/runtime"},
		"half an edge pair":      {WSS: "wss://machine.test/v1/runtime"},
		"quic without wss":       {QUIC: "quic://machine.test:443"},
		"path ingress with quic": {QUIC: "quic://machine.test:443", WSS: "wss://machine.test/prefix/v1/runtime"},
		"wss not the runtime":    {WSS: "wss://machine.test/prefix/other"},
	} {
		expiresAt := time.Now().Add(time.Minute)
		descriptor := ExecDescriptor{OperationID: "operation_exec_1", Environment: &Environment{ID: "env_1", Kind: "machine", ResourceID: "um_1", State: "ready", Root: "/workspace"}, Endpoints: endpoints, Auth: AuthMaterial{Method: "bearer", Token: "exec-token", ExpiresAt: expiresAt, Scopes: []string{"exec:operate"}}, ExpiresAt: expiresAt}
		err := validateOperationDescriptor(descriptor, "um_1", "operation_exec_1", "exec:operate", "exec")
		if (err == nil) == refused[name] {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPrepareAuthenticatedMachineInstallationBindsMachineOperationAndGeneration(t *testing.T) {
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/mch_1/machine-installations" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Idempotency-Key") != "machine-installation-operation-1" {
			t.Fatalf("request=%s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		var input struct {
			Verifier                string          `json:"verifier"`
			PublicIdentityKey       string          `json:"public_identity_key"`
			InstallationGeneration  int64           `json:"installation_generation"`
			Artifact                json.RawMessage `json:"artifact"`
			SSHUser                 string          `json:"ssh_user"`
			SSHPort                 uint16          `json:"ssh_port"`
			CanReuseRuntimeIdentity bool            `json:"can_reuse_runtime_identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.InstallationGeneration != 7 || input.PublicIdentityKey != "public-key" || input.Verifier != "verifier-012345678901234567890123" || input.Artifact != nil || input.SSHUser != "pujan" || input.SSHPort != 22 || !input.CanReuseRuntimeIdentity {
			t.Fatalf("input=%+v err=%v", input, err)
		}
		writeData(w, http.StatusCreated, AuthenticatedMachineInstallation{ExpiresAt: expiresAt})
	}))
	defer server.Close()
	result, err := New(server.URL, config.Credential{AccessToken: "token"}, server.Client()).PrepareAuthenticatedMachineInstallation(context.Background(), "mch_1", "machine-installation-operation-1", AuthenticatedMachineInstallationInput{
		Verifier: "verifier-012345678901234567890123", PublicIdentityKey: "public-key",
		InstallationGeneration: 7, SSHUser: "pujan", SSHPort: 22, CanReuseRuntimeIdentity: true,
	})
	if err != nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestManagedSSHAuthorizedKeysReportsSafeContractMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/machine_1/ssh-authorized-keys" {
			http.NotFound(w, r)
			return
		}
		writeData(w, http.StatusOK, ManagedSSHAuthorizedKeys{Type: "PRIVATE_PAYLOAD", Version: 1, MachineID: "PRIVATE_MACHINE", MachineGeneration: 2})
	}))
	defer server.Close()

	_, err := New(server.URL, config.Credential{}, server.Client()).ManagedSSHAuthorizedKeys(t.Context(), "machine_1", "credential_1", 1, []byte("proof"))
	if err == nil || err.Error() != "paperboat-server returned an invalid managed SSH authorized-key set" || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("err=%v", err)
	}
}

func TestClientConfigurationRejectsInvalidURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeData(w, http.StatusOK, ClientConfiguration{Version: "1", MachinesURL: "/dashboard/machines"})
	}))
	defer srv.Close()

	_, err := New(srv.URL, config.Credential{}, nil).ClientConfiguration(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid machines URL") {
		t.Fatalf("err=%v", err)
	}
}

func TestEndpointCertificateUsesExactEscapedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/endpoints/machine_01/certificates/3" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request=%s %s authorization=%q", r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"))
		}
		writeData(w, http.StatusOK, EndpointCertificateDocument{Version: 1, EndpointID: "machine_01", Generation: 3})
	}))
	defer server.Close()
	value, err := New(server.URL, config.Credential{AccessToken: "token"}, server.Client()).EndpointCertificate(context.Background(), "machine_01", 3)
	if err != nil || value.EndpointID != "machine_01" || value.Generation != 3 {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	if _, err := New(server.URL, config.Credential{}, server.Client()).EndpointCertificate(context.Background(), "", 3); err == nil {
		t.Fatal("empty endpoint identity was accepted")
	}
}

func TestRequestCLIEndpointUsesBoundHTTPContract(t *testing.T) {
	input := CLIEndpointRequestInput{
		OperationID: "operation_cli_0123456789", EndpointID: "cli_session_01", Generation: 1,
		QUICPublicKey: "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI",
	}
	expected := PendingEndpointIdentity{
		RequestID: "per_0123456789abcdef", EndpointID: input.EndpointID, Role: "cli", State: "pending", Generation: 1,
		QUICPublicKey: input.QUICPublicKey,
		CreatedAt:     time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 8, 3, 12, 5, 0, 0, time.UTC),
		SafetyCode: "abcde-fghij",
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/e2ee/endpoint-requests" ||
			r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Idempotency-Key") != input.OperationID ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("request=%s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		var body map[string]json.RawMessage
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 4 {
			t.Fatalf("request body fields=%v", body)
		}
		var got CLIEndpointRequestInput
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &got); err != nil || got != input {
			t.Fatalf("request body=%s decoded=%+v err=%v", encoded, got, err)
		}
		writeData(w, http.StatusCreated, expected)
	}))
	defer server.Close()

	got, err := New(server.URL, config.Credential{AccessToken: "token"}, server.Client()).RequestCLIEndpoint(context.Background(), input)
	if err != nil || got != expected || requests != 1 {
		t.Fatalf("response=%+v requests=%d err=%v", got, requests, err)
	}

	for name, invalid := range map[string]CLIEndpointRequestInput{
		"operation":  {EndpointID: input.EndpointID, Generation: 1, QUICPublicKey: input.QUICPublicKey},
		"endpoint":   {OperationID: input.OperationID, Generation: 1, QUICPublicKey: input.QUICPublicKey},
		"generation": {OperationID: input.OperationID, EndpointID: input.EndpointID, QUICPublicKey: input.QUICPublicKey},
		"QUIC key":   {OperationID: input.OperationID, EndpointID: input.EndpointID, Generation: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(server.URL, config.Credential{AccessToken: "token"}, server.Client()).RequestCLIEndpoint(context.Background(), invalid); err == nil {
				t.Fatal("invalid CLI endpoint request was sent")
			}
		})
	}
	if requests != 1 {
		t.Fatalf("invalid inputs reached server: requests=%d", requests)
	}
}

func TestBootstrapE2EEUsesIdempotencyAndStrictResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/e2ee/bootstrap" || request.Header.Get("Idempotency-Key") != "bootstrap-operation-0123456789" {
			t.Fatalf("request=%s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"data":{"root_public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","certificate":{"version":1,"account_id":"account_1","root_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","endpoint_id":"cli_1","role":"cli","generation":1,"serial":1,"issued_at":"2026-08-03T00:00:00Z","expires_at":"2026-09-02T00:00:00Z","certificate":"certificate","certificate_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}`)
	}))
	defer server.Close()
	result, err := New(server.URL, config.Credential{AccessToken: "token"}, nil).BootstrapE2EE(context.Background(), "bootstrap-operation-0123456789", E2EEBootstrapInput{})
	if err != nil || result.Certificate.EndpointID != "cli_1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestConfigAssignmentRequestsUseBearerAndSnakeCase(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/config-repositories":
			writeData(w, http.StatusOK, map[string]any{"items": []map[string]any{{"id": "cfgrepo_1", "provider": "github", "external_ref": "acme/config", "display_name": "Config"}}, "pagination": Pagination{Limit: 200, Total: 1}})
		case "GET /v1/machines/mch_1/config-assignment":
			writeData(w, http.StatusOK, map[string]any{"id": "cfgasn_1", "environment_id": "mch_1", "repository_id": "cfgrepo_1", "consent_state": "pending", "version": 2})
		case "PUT /v1/machines/mch_1/config-assignment":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["push_repository_id"] != "cfgrepo_1" || body["mode"] != "push_only" || body["expected_version"] != float64(2) || body["configuration_revision"] != strings.Repeat("a", 64) {
				t.Fatalf("body=%v", body)
			}
			writeData(w, http.StatusOK, map[string]any{"id": "cfgasn_1", "environment_id": "mch_1", "repository_id": "cfgrepo_1", "consent_state": "pending", "version": 3})
		case "DELETE /v1/machines/mch_1/config-assignment":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, config.Credential{AccessToken: "token"}, srv.Client())
	repos, err := c.ListConfigRepositories(context.Background())
	if err != nil || len(repos) != 1 || repos[0].ID != "cfgrepo_1" {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
	assignment, err := c.ConfigAssignment(context.Background(), "mch_1")
	if err != nil || assignment.Version != 2 || assignment.RepositoryID == nil {
		t.Fatalf("assignment=%+v err=%v", assignment, err)
	}
	if _, err := c.ConfigureConfigProjection(context.Background(), "mch_1", "", "cfgrepo_1", "push_only", false, 2, nil, nil, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := c.UnassignConfig(context.Background(), "mch_1", 3); err != nil {
		t.Fatal(err)
	}
	if got := requests[len(requests)-1]; got != "DELETE /v1/machines/mch_1/config-assignment?expected_version=3" {
		t.Fatalf("last request=%q", got)
	}
}

func TestFavoritesRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/favorites" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["kind"] != "machine" || body["resource_id"] != "mch_1" || body["favorite"] != true {
				t.Fatalf("body=%v", body)
			}
		}
		writeData(w, http.StatusOK, []Favorite{{Kind: "machine", ResourceID: "mch_1"}})
	}))
	defer srv.Close()
	client := New(srv.URL, config.Credential{AccessToken: "token"}, srv.Client())
	if items, err := client.ListFavorites(context.Background()); err != nil || len(items) != 1 || items[0].ResourceID != "mch_1" {
		t.Fatalf("list=%v err=%v", items, err)
	}
	if items, err := client.SetFavorite(context.Background(), "machine", "mch_1", true); err != nil || len(items) != 1 {
		t.Fatalf("set=%v err=%v", items, err)
	}
}

func TestUserMachineRequestsUseScopedRoutes(t *testing.T) {
	var paths []string
	var connectBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/v1/machines":
			writeData(w, http.StatusOK, UserMachinePage{Items: []UserMachine{{ID: "um_1", Alias: "studio-mac", Online: true}}, Pagination: Pagination{Limit: 200, Total: 1}})
		case "/v1/machines/um_1/connection-descriptor":
			body, _ := io.ReadAll(r.Body)
			connectBodies = append(connectBodies, string(body))
			writeData(w, http.StatusOK, ConnectionDescriptor{Schema: ConnectionSchemaV1, UserMachineID: "um_1", Connectable: false})
		case "/v1/machines/um_1/connection-readiness":
			writeData(w, http.StatusOK, ConnectionDescriptor{Schema: ConnectionSchemaV1, UserMachineID: "um_1", Connectable: false})
		case "/v1/machines/um_1/file-transfer-descriptor":
			body, _ := io.ReadAll(r.Body)
			connectBodies = append(connectBodies, string(body))
			writeData(w, http.StatusOK, FileTransfer{Endpoint: "https://machine.test/v1/file-transfers", SourceMachineID: "um_source", DestinationMachineID: "um_1", InitiatingUserID: "usr_1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, config.Credential{AccessToken: "t"}, nil)
	machines, err := c.ListUserMachines(context.Background())
	if err != nil || len(machines) != 1 || machines[0].ID != "um_1" {
		t.Fatalf("machines=%+v err=%v", machines, err)
	}
	if _, err := c.UserMachineConnectionDescriptor(context.Background(), "um_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserMachineConnectionDescriptorForSession(context.Background(), "um_1", "pts_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserMachineConnectionReadiness(context.Background(), "um_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserMachineConnectionReadinessForSession(context.Background(), "um_1", "pts_1"); err != nil {
		t.Fatal(err)
	}
	if descriptor, err := c.MachineFileTransferDescriptor(context.Background(), "um_1", "um_source", "pts_1"); err != nil || descriptor.SourceMachineID != "um_source" {
		t.Fatalf("transfer descriptor=%+v err=%v", descriptor, err)
	}
	if got := strings.Join(paths, ","); !strings.Contains(got, "GET /v1/machines?limit=200&offset=0&sort=alias") || !strings.Contains(got, "POST /v1/machines/um_1/connection-descriptor") || !strings.Contains(got, "GET /v1/machines/um_1/connection-readiness?terminal_session_id=pts_1") {
		t.Fatalf("paths=%q", got)
	}
	if got := strings.Join(connectBodies, ","); got != `,{"terminal_session_id":"pts_1"},{"session_id":"pts_1","source_machine_id":"um_source"}` {
		t.Fatalf("connect bodies=%q", got)
	}
}

func TestUserMachineCapabilitiesDecodeEnvironmentInjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines" {
			t.Fatalf("request path=%q", r.URL.Path)
		}
		writeData(w, http.StatusOK, UserMachinePage{
			Items: []UserMachine{{ID: "um_env", Capabilities: MachineCapabilities{
				EnvironmentInjection: MachineCapability{Configured: true, Observed: true},
			}}},
			Pagination: Pagination{Limit: 200, Total: 1},
		})
	}))
	defer srv.Close()

	machines, err := New(srv.URL, config.Credential{}, srv.Client()).ListUserMachines(context.Background())
	if err != nil || len(machines) != 1 {
		t.Fatalf("machines=%+v err=%v", machines, err)
	}
	got := machines[0].Capabilities.EnvironmentInjection
	if !got.Configured || !got.Observed {
		t.Fatalf("environment management capability=%+v", got)
	}
}

func TestUserMachineRevokeRequestsUseBearer(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		writeData(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer srv.Close()
	c := New(srv.URL, config.Credential{AccessToken: "token"}, nil)
	if err := c.DisconnectUserMachine(context.Background(), "um_1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteUserMachine(context.Background(), "um_1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, ","); got != "POST /v1/machines/um_1/disconnect Bearer token,DELETE /v1/machines/um_1 Bearer token" {
		t.Fatalf("requests=%q", got)
	}
}

func TestManagedSSHReadinessRecordsAreGenerationBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/machines/machine_1/ssh-target":
			writeData(writer, http.StatusOK, ManagedSSHTarget{Type: "machine_target", Version: 1, MachineID: "machine_1", MachineGeneration: 4, OSUser: "deploy", Port: 22, ReconciliationVersion: 2})
		case "/v1/machines/machine_1/ssh-host-keys":
			writeData(writer, http.StatusOK, ManagedSSHHostKeySet{Type: "host_key_set", Version: 1, SetID: "keyset_01234567-89ab-4cde-8fab-0123456789ab", MachineID: "machine_1", MachineGeneration: 4, ObservationGeneration: 3, Keys: []string{"ssh-ed25519 AAAA test"}, Fingerprint: "SHA256:test", State: "active", ReconciliationVersion: 5})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if target, err := client.ManagedSSHTarget(context.Background(), "machine_1", 4); err != nil || target.Port != 22 {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	if keys, err := client.ManagedSSHHostKeys(context.Background(), "machine_1", 4); err != nil || keys.ObservationGeneration != 3 {
		t.Fatalf("keys=%+v err=%v", keys, err)
	}
}

func TestObserveManagedSSHHostKeysAcceptsReusedActiveSet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeData(writer, http.StatusOK, ManagedSSHHostKeySet{Type: "host_key_set", Version: 1, SetID: "keyset_01234567-89ab-4cde-8fab-0123456789ab", MachineID: "machine_1", MachineGeneration: 4, ObservationGeneration: 3, Keys: []string{"ssh-ed25519 AAAA test"}, Fingerprint: "SHA256:test", State: "active", ReconciliationVersion: 5})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	set, err := client.ObserveManagedSSHHostKeys(context.Background(), "machine_1", "identity", "managed-ssh-observe-4-47", 4, 47, []string{"ssh-ed25519 AAAA test"}, []byte("proof"))
	if err != nil || set.ObservationGeneration != 3 {
		t.Fatalf("set=%+v err=%v", set, err)
	}
}

func TestManagedSSHClientKeyRegistrationBindsFingerprintAndIdempotency(t *testing.T) {
	var requestPath, operationID, body string
	fingerprint := [32]byte{1, 2, 3}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestPath = request.URL.Path
		operationID = request.Header.Get("Idempotency-Key")
		value, _ := io.ReadAll(request.Body)
		body = string(value)
		writeData(writer, http.StatusCreated, ManagedSSHClientKey{Type: "client_key", Version: 1, Fingerprint: "SHA256:AQIDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", PublicKey: "ssh-ed25519 AAAA test", State: "active", ReconciliationVersion: 1})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	key, err := client.RegisterManagedSSHClientKey(context.Background(), "ssh-ed25519 AAAA test", fingerprint, "managed-ssh-operation-1")
	if err != nil || key.State != "active" {
		t.Fatalf("key=%+v err=%v", key, err)
	}
	if requestPath != "/v1/ssh/client-keys/SHA256:AQIDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" || operationID != "managed-ssh-operation-1" || body != `{"public_key":"ssh-ed25519 AAAA test"}` {
		t.Fatalf("path=%q operation=%q body=%q", requestPath, operationID, body)
	}
}

func TestSetUserMachineAvailabilityUsesIdempotencyAndVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/machines/um_1/availability-policy" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Idempotency-Key") != "availability-1" {
			t.Fatalf("request=%s %s auth=%q idempotency=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["mode"] != "keep_awake" || body["expected_version"] != float64(4) {
			t.Fatalf("body=%v err=%v", body, err)
		}
		writeData(w, http.StatusOK, AvailabilityPolicy{Schema: "paperboat.availability-policy/v1", DesiredMode: "keep_awake", DesiredVersion: 5, Status: "pending"})
	}))
	defer server.Close()
	result, err := New(server.URL, config.Credential{AccessToken: "token"}, server.Client()).SetUserMachineAvailability(context.Background(), "um_1", "keep_awake", "availability-1", 4)
	if err != nil || result.DesiredVersion != 5 || result.Status != "pending" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRenameUserMachine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v1/machines/um_1" || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request=%s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["alias"] != "new-studio" || body["display_name"] != nil {
			t.Fatalf("body=%v err=%v", body, err)
		}
		writeData(w, http.StatusOK, UserMachine{ID: "um_1", Alias: "new-studio"})
	}))
	defer server.Close()
	result, err := New(server.URL, config.Credential{AccessToken: "token"}, server.Client()).RenameUserMachine(context.Background(), "um_1", " New-Studio ")
	if err != nil || result.ID != "um_1" || result.Alias != "new-studio" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestUserMachineTerminalSessionRequests(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/machines/um_1/terminal-sessions":
			if r.Header.Get("Idempotency-Key") != "key-1" {
				t.Fatalf("missing idempotency key")
			}
			_, _ = w.Write([]byte(`{"data":{"id":"pts_1","name":"api","state":"running","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}`))
		case "GET /v1/machines/um_1/terminal-sessions":
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":"pts_1","name":"api","state":"running","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":null}}}`))
		case "PATCH /v1/machines/um_1/terminal-sessions/pts_1":
			_, _ = w.Write([]byte(`{"data":{"id":"pts_1","name":"renamed","state":"running","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}`))
		case "POST /v1/machines/um_1/terminal-sessions/pts_1/close":
			writeData(w, http.StatusOK, map[string]string{"operation_state": "applied"})
		case "DELETE /v1/machines/um_1/terminal-sessions/pts_1":
			writeData(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, config.Credential{AccessToken: "token"}, nil)
	if session, err := c.CreateUserMachineTerminalSession(context.Background(), "um_1", "api", "key-1"); err != nil || session.ID != "pts_1" {
		t.Fatalf("create session=%+v err=%v", session, err)
	}
	if sessions, err := c.ListUserMachineTerminalSessions(context.Background(), "um_1"); err != nil || len(sessions) != 1 || sessions[0].ID != "pts_1" {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if _, err := c.RenameUserMachineTerminalSession(context.Background(), "um_1", "pts_1", "renamed"); err != nil {
		t.Fatal(err)
	}
	if result, err := c.CloseUserMachineTerminalSession(context.Background(), "um_1", "pts_1"); err != nil || result.OperationState != "applied" {
		t.Fatal(err)
	}
	if err := c.DeleteUserMachineTerminalSession(context.Background(), "um_1", "pts_1"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 5 {
		t.Fatalf("requests=%v", seen)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func TestClientSendsBearer(t *testing.T) {
	var gotToken, gotProtocol string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Authorization")
		gotProtocol = r.Header.Get("X-Paperboat-Protocol")
		writeData(w, http.StatusOK, Me{ID: "usr_1", Email: "a@b.dev"})
	}))
	defer srv.Close()

	c := New(srv.URL, config.Credential{AccessToken: "sess-token"}, nil)
	me, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if gotToken != "Bearer sess-token" {
		t.Fatalf("authorization = %q", gotToken)
	}
	if gotProtocol == "" {
		t.Fatal("missing protocol negotiation header")
	}
	if me.Email != "a@b.dev" {
		t.Fatalf("me.Email = %q", me.Email)
	}
}

func TestClientIncompatibleVersionIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = io.WriteString(w, `{"error":{"code":"incompatible_client_version","message":"upgrade required","details":{"required_protocol":"2"}}}`)
	}))
	defer srv.Close()
	_, err := New(srv.URL, config.Credential{}, nil).Me(context.Background())
	var versionErr *ErrIncompatibleVersion
	if !errors.As(err, &versionErr) || versionErr.Required != "2" || !strings.Contains(versionErr.Error(), "upgrade") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeviceAuthorizeIncompatibleVersionIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = io.WriteString(w, `{"error":{"code":"incompatible_client_version","message":"upgrade pb before signing in","details":{"required_protocol":"2"}}}`)
	}))
	defer srv.Close()
	_, err := DeviceAuthorize(context.Background(), srv.URL, "machine", "desktop", "darwin", DeviceJoin{}, nil)
	var versionErr *ErrIncompatibleVersion
	if !errors.As(err, &versionErr) || versionErr.Required != "2" || !strings.Contains(versionErr.Error(), "upgrade pb") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientUnauthenticated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
	}))
	defer srv.Close()

	c := New(srv.URL, config.Credential{}, nil)
	_, err := c.ListUserMachines(context.Background())
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestClientStructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Request-Id", "req_123")
		writeErr(w, http.StatusConflict, "machine_not_ready", "Machine is not ready.")
	}))
	defer srv.Close()

	c := New(srv.URL, config.Credential{AccessToken: "t"}, nil)
	_, err := c.UserMachineConnectionDescriptor(context.Background(), "um_1")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err type = %T, want *APIError", err)
	}
	if apiErr.Code != "machine_not_ready" || apiErr.Status != http.StatusConflict || apiErr.RequestID != "req_123" || strings.Contains(apiErr.Error(), "req_123") {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestIncompatibleVersionAlwaysIncludesUpgradeGuidance(t *testing.T) {
	err := (&ErrIncompatibleVersion{Message: "protocol 1 is unsupported"}).Error()
	if !strings.Contains(err, "upgrade pb") {
		t.Fatalf("error = %q", err)
	}
}

func TestClientRejectsUnsafeRequestID(t *testing.T) {
	for _, value := range []string{"secret value", "path/value", "line\nbreak"} {
		if got := safeRequestID(value); got != "" {
			t.Fatalf("safeRequestID(%q) = %q", value, got)
		}
	}
}

func TestUserMachineConnectionReadinessForSessionUsesSelectedTerminalID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines/um_1/connection-readiness" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("terminal_session_id"); got != "pts_api" {
			t.Fatalf("terminal_session_id = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"schema": ConnectionSchemaV1, "machine_id": "um_1", "connectable": false}})
	}))
	defer server.Close()

	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if _, err := client.UserMachineConnectionReadinessForSession(context.Background(), "um_1", "pts_api"); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeCanonicalConnectionDescriptor(t *testing.T) {
	expires := time.Now().Add(time.Minute).UTC()
	response := ConnectionDescriptor{
		Schema: ConnectionSchemaV1, Issuer: "https://api.paperboat.test", Connectable: true, ExpiresAt: expires,
		Environment:  &Environment{ID: "env_1", Kind: "machine", ResourceID: "um_1", Alias: "studio", State: "ready", Root: "/Users/paperboat"},
		Terminal:     &Terminal{Protocol: "paperboat.terminal.v1", Endpoints: TerminalEndpoints{QUIC: "quic://edge.paperboat.test:443", WSS: "wss://edge.paperboat.test/v1/runtime"}, SessionID: "session_1"},
		FileTransfer: &FileTransfer{Endpoint: "https://edge.paperboat.test/v1/file-transfers"},
	}
	if err := response.NormalizeConnectionDescriptor(); err != nil {
		t.Fatal(err)
	}
	if response.UserMachineID != "um_1" || response.Environment.EnvironmentID != "env_1" || response.Environment.ProjectRoot != "/Users/paperboat" {
		t.Fatalf("canonical environment was not normalized: %#v", response)
	}
	if response.Terminal.Protocol != "paperboat.terminal.v1" || response.Terminal.Endpoints.WSS == "" {
		t.Fatalf("canonical terminal was not normalized: %#v", response.Terminal)
	}
	if response.FileTransfer.Endpoint != "https://edge.paperboat.test/v1/file-transfers" {
		t.Fatalf("file transfer was not normalized: %#v", response.FileTransfer)
	}
}

func TestNormalizeConnectionDescriptorRejectsUnknownSchema(t *testing.T) {
	response := ConnectionDescriptor{Schema: "paperboat.environment-connection/unknown"}
	if err := response.NormalizeConnectionDescriptor(); err == nil {
		t.Fatal("expected unknown schema to fail closed")
	}
}

func TestUserMachineConnectionDescriptorDecodesCanonicalDescriptor(t *testing.T) {
	expires := time.Now().Add(time.Minute).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/um_1/connection-descriptor" {
			t.Fatalf("descriptor request = %s %s", r.Method, r.URL.Path)
		}
		writeData(w, http.StatusOK, map[string]any{
			"schema": ConnectionSchemaV1, "issuer": "https://api.paperboat.test", "connectable": true, "expires_at": expires,
			"environment":   map[string]any{"id": "env_1", "kind": "machine", "resource_id": "um_1", "state": "ready", "root": "/workspace"},
			"terminal":      map[string]any{"protocol": "paperboat.terminal.v1", "endpoints": map[string]any{"quic": "quic://edge.paperboat.test:443", "wss": "wss://edge.paperboat.test/v1/runtime"}, "session_id": "session_1", "cwd": "/workspace"},
			"file_transfer": map[string]any{"endpoint": "https://edge.paperboat.test/v1/file-transfers"},
		})
	}))
	defer server.Close()

	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	response, err := client.UserMachineConnectionDescriptor(context.Background(), "um_1")
	if err != nil {
		t.Fatal(err)
	}
	if response.UserMachineID != "um_1" || response.Terminal.Endpoints.WSS != "wss://edge.paperboat.test/v1/runtime" || response.FileTransfer.Endpoint != "https://edge.paperboat.test/v1/file-transfers" {
		t.Fatalf("canonical response not decoded: %#v", response)
	}
}

func TestConfigRepositoryDiscoveryAndConnectionUseNativeCredential(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing native authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/config-repositories/candidates":
			fmt.Fprint(w, `{"data":{"items":[{"provider":"github","external_id":"123","display_name":"owner/private config","default_branch":"main"}]}}`)
		case "POST /v1/config-repositories":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["provider"] != "github" || body["external_ref"] != "123" || body["display_name"] != "owner/private config" {
				t.Errorf("incorrect repository request: %v %v", body, err)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"data":{"id":"repo","provider":"github","external_ref":"123","display_name":"owner/private config","state":"active"}}`)
		default:
			t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, nil)
	candidates, err := client.ConfigRepositoryCandidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discovery %v %v", candidates, err)
	}
	repository, err := client.ConnectConfigRepository(context.Background(), candidates[0])
	if err != nil || repository.ID != "repo" || repository.State != "active" || requests != 2 {
		t.Fatalf("connection %+v %v requests=%d", repository, err, requests)
	}
}

func TestClientSendsBuildDistribution(t *testing.T) {
	prior := buildinfo.Distribution
	t.Cleanup(func() { buildinfo.Distribution = prior })
	for _, distribution := range []string{"official", "custom"} {
		t.Run(distribution, func(t *testing.T) {
			buildinfo.Distribution = distribution
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Paperboat-Distribution") != distribution || r.Header.Get("X-Paperboat-Client") != "paperboat" {
					t.Error("incorrect CLI distribution marker")
				}
				writeData(w, http.StatusOK, Me{ID: "usr_1"})
			}))
			defer srv.Close()
			if _, err := New(srv.URL, config.Credential{AccessToken: "test-token"}, nil).Me(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublicCallRetainsBodyFailureObservesAndRecovers(t *testing.T) {
	var requests int
	const privateMarker = "private_response_marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get(supportref.Header) == "" {
			t.Error("request support reference missing")
		}
		if requests == 1 {
			body := `{"error":{"code":"internal","message":"` + privateMarker + `"}}`
			w.Header().Set("Content-Length", fmt.Sprint(len(body)+32))
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, body)
			return
		}
		writeData(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	defer server.Close()

	var mu sync.Mutex
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		mu.Lock()
		faults = append(faults, fault)
		mu.Unlock()
	})
	t.Cleanup(restoreObserver)

	reference := supportref.New()
	if reference == "" {
		t.Fatal("could not create support reference")
	}
	ctx := supportref.WithContext(context.Background(), reference)
	var out struct {
		OK bool `json:"ok"`
	}
	err := publicCall(ctx, server.URL, "/v1/auth/test", nil, "", &out, server.Client())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("status or original response-read cause lost: %T %v", err, err)
	}
	if strings.Contains(err.Error(), privateMarker) || apiErr.SupportReference != reference {
		t.Fatalf("unsafe response prose or support reference: %v", err)
	}
	if !errorreport.HTTPAttemptObserved(err) {
		t.Fatal("returned failure lost its HTTP reporting owner; outer callers would report it again")
	}
	mu.Lock()
	if len(faults) != 1 {
		mu.Unlock()
		t.Fatalf("observations=%d, want one", len(faults))
	}
	fault := faults[0]
	mu.Unlock()
	if fault.Code != "control_request_failed" || fault.HTTPStatus != http.StatusServiceUnavailable || fault.SupportReference != reference || strings.Contains(strings.Join(fault.ErrorChain, " "), privateMarker) {
		t.Fatalf("unsafe or incomplete observed fault: %+v", fault)
	}

	if err := publicCall(ctx, server.URL, "/v1/auth/test", nil, "", &out, server.Client()); err != nil || !out.OK || requests != 2 {
		t.Fatalf("recovery=%v ok=%t requests=%d", err, out.OK, requests)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(faults) != 1 {
		t.Fatalf("healthy recovery added an observation: %d", len(faults))
	}
}

type publicCallRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip publicCallRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type publicCallCloseErrorBody struct {
	*strings.Reader
	closeErr error
	closed   bool
}

func (body *publicCallCloseErrorBody) Close() error {
	body.closed = true
	return body.closeErr
}

func TestPublicCallPreservesResponseCloseFailure(t *testing.T) {
	closeErr := errors.New("private response close marker")
	body := &publicCallCloseErrorBody{Reader: strings.NewReader(`{"data":{"ok":true}}`), closeErr: closeErr}
	client := &http.Client{Transport: publicCallRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: request}, nil
	})}
	var out struct {
		OK bool `json:"ok"`
	}
	err := publicCall(context.Background(), "https://api.example.test", "/v1/auth/test", nil, "", &out, client)
	if err == nil || !errors.Is(err, closeErr) || !body.closed {
		t.Fatalf("response close failure was lost: %v", err)
	}
	if err.Error() != "paperboat-server response could not be closed" || strings.Contains(err.Error(), "private") {
		t.Fatalf("response close details escaped: %v", err)
	}
}

func TestPublicCallDoesNotRepeatOwnedHTTPStatusObservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"private response text"}}`)
	}))
	defer server.Close()

	var mu sync.Mutex
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		mu.Lock()
		faults = append(faults, fault)
		mu.Unlock()
	})
	t.Cleanup(restoreObserver)

	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	client := server.Client()
	client.Transport = errorreport.Transport(client.Transport, server.URL)
	err := publicCall(ctx, server.URL, "/v1/auth/test", nil, "", nil, client)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !errors.Is(err, ErrUnauthenticated) || apiErr.SupportReference != reference || strings.Contains(err.Error(), "private") {
		t.Fatalf("safe authorization failure contract changed: %T %v", err, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(faults) != 1 || faults[0].HTTPStatus != http.StatusUnauthorized || faults[0].SupportReference != reference {
		t.Fatalf("owned HTTP attempt was observed more than once or lost its reference: %+v", faults)
	}
}

func TestPublicCallBoundsResponseAndKeepsOversizeCausePrivate(t *testing.T) {
	const privateMarker = "private_response_marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := `{"data":"` + privateMarker + strings.Repeat("x", maxTunnelResponseBytes) + `"}`
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	var out struct {
		Value string `json:"value"`
	}
	err := publicCall(context.Background(), server.URL, "/v1/auth/test", nil, "", &out, server.Client())
	var decodeErr *ResponseDecodeError
	if !errors.As(err, &decodeErr) || !errors.Is(err, errPublicResponseTooLarge) {
		t.Fatalf("oversized response was not rejected with its cause: %T %v", err, err)
	}
	if strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("response content escaped in error: %v", err)
	}
}

// The server pages config status; every page is read so machines past the first 50 are not lost.
func TestConfigSyncStatusReadsEveryPage(t *testing.T) {
	var offsets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		if r.URL.Query().Get("limit") != "200" {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("offset") == "0" {
			writeData(w, http.StatusOK, map[string]any{"state": "conflict", "environments": []map[string]any{{"machine_id": "first", "alias": "a"}}, "pagination": map[string]any{"next_offset": 1}})
			return
		}
		writeData(w, http.StatusOK, map[string]any{"state": "healthy", "environments": []map[string]any{{"machine_id": "second", "alias": "z"}}, "pagination": map[string]any{"next_offset": nil}})
	}))
	defer server.Close()
	status, err := New(server.URL, config.Credential{AccessToken: "native"}, nil).ConfigSyncStatus(context.Background())
	if err != nil || len(status.Environments) != 2 || status.Environments[1].MachineID != "second" || status.State != "conflict" || len(offsets) != 2 || offsets[1] != "1" {
		t.Fatalf("status = %+v, offsets = %v, err = %v", status, offsets, err)
	}
}

func TestConfigSyncStatusRefusesAPageThatDoesNotAdvance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, map[string]any{"state": "healthy", "environments": []map[string]any{}, "pagination": map[string]any{"next_offset": 0}})
	}))
	defer server.Close()
	if _, err := New(server.URL, config.Credential{}, nil).ConfigSyncStatus(context.Background()); !errors.Is(err, ErrConfigStatusInvalid) {
		t.Fatalf("err = %v", err)
	}
}

// Team default calls go to the team they name: in the personal workspace the server refused them with
// 409 workspace_target_conflict, so `pb config team-default-set/adopt/unadopt` never worked (Lane L, pbvm3).
func TestConfigTeamDefaultCallsUseTheNamedTeamWorkspace(t *testing.T) {
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = r.URL.Query().Get("workspace")
		writeData(w, 200, ConfigTeamDefault{TeamID: "team-one"})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	if err := client.SetWorkspace("personal"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _ = client.ConfigTeamDefault(ctx, "team-one")
	_, _ = client.SetConfigTeamDefault(ctx, "team-one", "repository-1", 0)
	_, _ = client.AdoptConfigTeamDefault(ctx, "team-one", 1)
	_ = client.UnadoptConfigTeamDefault(ctx, "team-one")
	for _, call := range []string{"GET /v1/teams/team-one/config-default", "PUT /v1/teams/team-one/config-default", "PUT /v1/config-sync/team-default-adoption", "DELETE /v1/config-sync/team-default-adoption"} {
		if seen[call] != "team-one" {
			t.Fatalf("%s sent in workspace %q", call, seen[call])
		}
	}
	if client.Workspace() != "personal" {
		t.Fatal("a team default call changed the command's workspace")
	}
}
