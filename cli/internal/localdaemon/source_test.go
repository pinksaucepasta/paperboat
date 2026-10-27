package localdaemon

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type rotatingAuthSource struct {
	mu    sync.Mutex
	calls int
}

func (s *rotatingAuthSource) Refresh() (config.Credential, error) {
	return s.Credential()
}

func (s *rotatingAuthSource) Credential() (config.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return config.Credential{AccessToken: fmt.Sprintf("token-%d", s.calls)}, nil
}

func TestAuthenticatedMachineSourceLoadsCredentialForEveryRefresh(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"items":[],"pagination":{"limit":200,"offset":0,"total":0,"next_offset":null}}}`))
	}))
	defer server.Close()
	auth := &rotatingAuthSource{}
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: auth}
	for range 2 {
		if _, err := source.ListUserMachines(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authorizations) != 2 || authorizations[0] != "Bearer token-1" || authorizations[1] != "Bearer token-2" {
		t.Fatalf("authorizations=%v", authorizations)
	}
}

func TestAuthenticatedMachineSourceRunsAutomaticPeerApprovalBeforePublishing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/machines" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"items":[],"pagination":{"limit":200,"offset":0,"total":0,"next_offset":null}}}`))
	}))
	defer server.Close()

	wantErr := errors.New("automatic approval failed")
	calls := 0
	source := AuthenticatedMachineSource{
		ServerURL: server.URL,
		Auth:      &rotatingAuthSource{},
		AutoApprovePeerEnrollments: func(ctx context.Context, client *api.Client, machines []api.UserMachine) error {
			calls++
			if ctx == nil || client == nil || machines == nil || len(machines) != 0 {
				t.Fatalf("callback context=%v client=%v machines=%v", ctx, client, machines)
			}
			return wantErr
		},
	}
	if _, err := source.ListUserMachines(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("automatic approval error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("automatic approval calls = %d", calls)
	}
}

func TestAuthenticatedMachineSourceReportsMissingSignerWithoutHidingInventory(t *testing.T) {
	rootPublic, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/machines":
			_, _ = writer.Write([]byte(`{"data":{"items":[],"pagination":{"limit":200,"offset":0,"total":0,"next_offset":null}}}`))
		case "/v1/e2ee/pending-endpoints":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": []api.PendingEndpointIdentity{{RequestID: "per_cli_0123456789", EndpointID: "cli_new", Role: "cli", State: "pending", Generation: 1, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute), SafetyCode: "abcde-fghij"}}})
		case "/v1/e2ee/root":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": api.E2EERoot{Version: 1, TrustedKeys: []api.E2EEKey{{KeyID: "aek_" + hex.EncodeToString(rootFingerprint[:]), PublicKey: base64.RawURLEncoding.EncodeToString(rootPublic), Fingerprint: hex.EncodeToString(rootFingerprint[:]), Generation: 1}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	rootDir := t.TempDir()
	store := config.ProfileStore{Path: rootDir, Secrets: config.FileSecretStore{Dir: filepath.Join(rootDir, "secrets")}}
	if err := store.SavePeerAccountRootPublic(server.URL, "account_1", rootPublic); err != nil {
		t.Fatal(err)
	}
	profile := config.Profile{Issuer: server.URL, Account: config.Account{ID: "account_1"}, CLIClientSessionID: "cli_daemon"}
	var reported []PeerApprovalSignerUnavailableError
	source := AuthenticatedMachineSource{
		ServerURL: server.URL,
		Auth:      &rotatingAuthSource{},
		AutoApprovePeerEnrollments: func(ctx context.Context, client *api.Client, machines []api.UserMachine) error {
			return ApproveOwnedPeerEnrollments(ctx, store, profile, client, machines)
		},
		ReportPeerApprovalSignerUnavailable: RateLimitedPeerApprovalReporter(time.Now, time.Minute, func(issue PeerApprovalSignerUnavailableError) {
			reported = append(reported, issue)
		}),
	}
	for range 2 {
		machines, err := source.ListUserMachines(context.Background())
		if err != nil || len(machines) != 0 {
			t.Fatalf("machines=%+v err=%v", machines, err)
		}
	}
	if len(reported) != 1 || reported[0].PendingRequests != 1 {
		t.Fatalf("reported=%+v", reported)
	}
}

func TestRateLimitedPeerApprovalReporterBoundsRepeatedRefreshes(t *testing.T) {
	now := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	var reports []PeerApprovalSignerUnavailableError
	reporter := RateLimitedPeerApprovalReporter(func() time.Time { return now }, time.Minute, func(issue PeerApprovalSignerUnavailableError) {
		reports = append(reports, issue)
	})
	reporter(PeerApprovalSignerUnavailableError{PendingRequests: 1})
	reporter(PeerApprovalSignerUnavailableError{PendingRequests: 2})
	now = now.Add(time.Minute)
	reporter(PeerApprovalSignerUnavailableError{PendingRequests: 2})
	if len(reports) != 2 || reports[0].PendingRequests != 1 || reports[1].PendingRequests != 2 {
		t.Fatalf("reports=%+v", reports)
	}
}

func TestIssuePeerStreamRefreshesRejectedCredentialOnce(t *testing.T) {
	expires := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	var operations []string
	var workspaces []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			OperationID string `json:"operation_id"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		operations = append(operations, body.OperationID)
		workspaces = append(workspaces, request.URL.Query().Get("workspace"))
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") == "Bearer token-1" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":{"code":"unauthenticated","message":"expired"}}`))
			return
		}
		_, _ = fmt.Fprintf(writer, `{"data":{"operation_id":"operation_1","environment":{"id":"environment_1","kind":"machine","resource_id":"machine_1","state":"ready","root":"/root"},"endpoints":{"quic":"quic://example.test:443","wss":"wss://example.test/v1/runtime"},"auth":{"method":"bearer","token":"operation-token","expires_at":%q,"scopes":["exec:operate"]},"expires_at":%q}}`, expires, expires)
	}))
	defer server.Close()
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, SourceMachineID: "source_1"}
	request := localapi.PeerStreamRequest{Workspace: "team-audit", Schema: localapi.PeerStreamSchemaV1, Consumer: "exec", MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Deadline: time.Now().UTC().Add(time.Minute), MaximumBytes: 1024, Payload: json.RawMessage(`{"operation_id":"operation_1"}`)}
	result, err := source.IssuePeerStream(context.Background(), request)
	if !reflect.DeepEqual(workspaces, []string{"team-audit", "team-audit"}) {
		t.Fatalf("descriptor workspace changed or missing: %v", workspaces)
	}
	if err != nil || result.Credential != "operation-token" || !reflect.DeepEqual(operations, []string{"operation_1", "operation_1"}) {
		t.Fatalf("result=%+v operations=%v err=%v", result, operations, err)
	}
}

// The SSH authority comes from the inventory item; an inventory refresh makes no request per machine.
func TestAuthenticatedMachineSourceReconcilesSSHAuthority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/v1/machines" {
			t.Errorf("per-machine request %s %s; the inventory carries SSH targets", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`{"data":{"items":[` +
			`{"id":"machine_1","alias":"studio","state":"active","online":true,"installation_generation":4,"ssh":{"os_user":"deploy","port":22,"target_generation":4,"host_key_generation":4}},` +
			`{"id":"machine_2","alias":"build","state":"active","online":true,"installation_generation":2,"ssh":{"os_user":"ci","port":2222,"target_generation":2,"host_key_generation":0}},` +
			`{"id":"machine_3","alias":"old","state":"active","online":true,"installation_generation":3,"ssh":{"os_user":"x","port":22,"target_generation":2,"host_key_generation":2}},` +
			`{"id":"machine_4","alias":"away","state":"active","online":false,"installation_generation":1,"ssh":{"os_user":"y","port":22,"target_generation":1,"host_key_generation":1}},` +
			`{"id":"machine_5","alias":"clone","state":"active","online":true,"installation_generation":1,"ssh":{"os_user":"z","port":22,"target_generation":1,"host_key_generation":0,"host_key_conflict":{"machine_name":"studio"}}}` +
			`],"pagination":{"limit":200,"offset":0,"total":5,"next_offset":null}}}`))
	}))
	defer server.Close()
	machines, err := (AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}}).ListUserMachines(context.Background())
	if err != nil || len(machines) != 5 {
		t.Fatalf("machines=%+v err=%v", machines, err)
	}
	want := []struct {
		user string
		port uint16
		auth api.SSHAuthority
	}{
		{"deploy", 22, api.SSHAuthority{TargetGeneration: 4, HostKeyGeneration: 4}},
		{"ci", 2222, api.SSHAuthority{TargetGeneration: 2}},
		{"", 0, api.SSHAuthority{}},                                                                    // target of an older installation
		{"", 0, api.SSHAuthority{}},                                                                    // offline
		{"z", 22, api.SSHAuthority{TargetGeneration: 1, HostKeyInUse: true, HostKeyInUseBy: "studio"}}, // refused as studio's keys
	}
	for i, machine := range machines {
		if machine.SSHUser != want[i].user || machine.SSHPort != want[i].port || machine.SSHAuthority != want[i].auth {
			t.Errorf("%s: user=%q port=%d authority=%+v, want %+v", machine.Alias, machine.SSHUser, machine.SSHPort, machine.SSHAuthority, want[i])
		}
	}
}

type failedRefreshAuth struct{ cause error }

func (failedRefreshAuth) Credential() (config.Credential, error) {
	return config.Credential{AccessToken: "fixture"}, nil
}
func (s failedRefreshAuth) Refresh() (config.Credential, error) { return config.Credential{}, s.cause }

func TestIssuePeerStreamPreservesRefreshFailureAfterRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
	}))
	defer server.Close()
	cause := syscall.EIO
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: failedRefreshAuth{cause}, SourceMachineID: "source_1"}
	request := localapi.PeerStreamRequest{Workspace: "personal", Schema: localapi.PeerStreamSchemaV1, Consumer: "exec", MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Deadline: time.Now().UTC().Add(time.Minute), MaximumBytes: 1024, Payload: json.RawMessage(`{"operation_id":"operation_1"}`)}
	_, err := source.IssuePeerStream(t.Context(), request)
	if !errors.Is(err, cause) || !errors.Is(err, api.ErrUnauthenticated) {
		t.Fatal("refresh error or original rejection lost")
	}
	fault := errorreport.ProjectFault(t.Context(), "paperboatd", "peer_stream", "control_request", "control_request_failed", err)
	if fault.Errno != int(cause) || fault.Outcome == "rejected" || fault.Stage != "peer_authority" {
		t.Fatalf("refresh cause masked: %+v", fault)
	}
}

func TestIssuePeerStreamRejectsInvalidWorkspaceBeforeProvider(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, SourceMachineID: "source_1"}
	for _, selector := range []string{"", "INVALID/team"} {
		_, err := source.IssuePeerStream(t.Context(), localapi.PeerStreamRequest{Consumer: "exec", Workspace: selector})
		if err == nil {
			t.Fatalf("workspace %q accepted", selector)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid selector reached provider: %d calls", calls)
	}
}

func TestAuthenticatedMachineSourceRefreshesRejectedAccessToken(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer token-1" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"items":[],"pagination":{"limit":200,"offset":0,"total":0,"next_offset":null}}}`))
	}))
	defer server.Close()
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}}
	if _, err := source.ListUserMachines(t.Context()); err != nil || calls != 2 {
		t.Fatal("machine inventory did not resume with refreshed access token")
	}
}

// With Paperboat unreachable (no answer, or a 503 from the API itself) the daemon opens exec and
// ssh without a descriptor for the host to admit from its own signed scopes; a refusal or any
// other answer stays final, and a client cannot ask for an offline stream itself (F21-11).
func TestIssuePeerStreamGoesOfflineOnlyWhenPaperboatIsUnreachable(t *testing.T) {
	status := http.StatusServiceUnavailable
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"down"}}`))
	}))
	request := localapi.PeerStreamRequest{Workspace: "personal", Schema: localapi.PeerStreamSchemaV1, Consumer: "exec", MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Deadline: time.Now().UTC().Add(time.Minute), MaximumBytes: 1024, Payload: json.RawMessage(`{"operation_id":"operation_1"}`)}
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, SourceMachineID: "source_1"}
	result, err := source.IssuePeerStream(context.Background(), request)
	if err != nil || !result.Offline || result.Credential != "" || result.AccessSessionID != "" || result.Validate(time.Now().UTC()) != nil {
		t.Fatalf("a 503 from Paperboat: result=%+v err=%v", result, err)
	}
	status = http.StatusForbidden
	if result, err := source.IssuePeerStream(context.Background(), request); err == nil || result.Offline {
		t.Fatalf("a refusal went offline: %+v", result)
	}
	server.Close()
	result, err = source.IssuePeerStream(context.Background(), request)
	if err != nil || !result.Offline {
		t.Fatalf("no answer from Paperboat: result=%+v err=%v", result, err)
	}
	request.Offline = true
	if _, err := source.IssuePeerStream(context.Background(), request); err == nil {
		t.Fatal("a client-requested offline stream was accepted")
	}
}

// D1 (F5-07): an SSH stream asks Paperboat nothing; the host admits it from its own signed rules.
func TestIssuePeerStreamSendsSSHToTheHostRulesWithoutARequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("SSH stream made a control request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	request := localapi.PeerStreamRequest{Workspace: "personal", Schema: localapi.PeerStreamSchemaV1, Consumer: "ssh", MachineID: "machine_1", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Deadline: time.Now().UTC().Add(time.Minute), MaximumBytes: 1024}
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, SourceMachineID: "source_1"}
	result, err := source.IssuePeerStream(context.Background(), request)
	if err != nil || !result.Offline || result.Credential != "" || result.Validate(time.Now().UTC()) != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
