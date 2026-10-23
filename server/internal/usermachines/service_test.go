package usermachines

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func TestExactNoRowsDoesNotMaskJoinedStorageFailure(t *testing.T) {
	for _, err := range []error{sql.ErrNoRows, pgx.ErrNoRows} {
		if !exactNoRows(err) {
			t.Errorf("exact no-row sentinel %T was not recognized", err)
		}
	}
	if exactNoRows(errors.Join(pgx.ErrNoRows, errors.New("rollback failed"))) {
		t.Fatal("joined no-row and rollback failure was classified as an expected missing resource")
	}
}

func TestMintMachineControlPreservesStorageFailure(t *testing.T) {
	store, err := db.Open(config.Database{Driver: "pgx", DSN: "postgres://paperboat:paperboat@127.0.0.1:1/paperboat_test?sslmode=disable"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service := New(store, nil, Policy{}, nil)
	_, err = service.mintMachineControl(context.Background(), dbsqlc.UserMachine{ID: "machine_fault_01", InstallationGeneration: 1}, "operation_fault_01")
	if err == nil || errors.Is(err, ErrMachineControlInvalid) {
		t.Fatalf("mint failure=%v; want original storage failure, not rejected proof", err)
	}
}

func TestProvisionApprovedMachineRequiresCanonicalHelperGrant(t *testing.T) {
	service := &Service{encryptionKey: "configured"}
	err := service.provisionApprovedUserMachine(context.Background(), "usr_1", "pair_1", UserMachine{ID: "um_1", EnvironmentID: "env_1", Platform: "linux", Architecture: "amd64"})
	if !errors.Is(err, ErrProvisioningUnavailable) {
		t.Fatalf("provision error = %v, want ErrProvisioningUnavailable", err)
	}
}

func TestEnrollmentStartJSONIncludesBootstrapTokenForOneShotSetup(t *testing.T) {
	value := EnrollmentStart{
		Enrollment:       Enrollment{ID: "ume_1"},
		BootstrapToken:   "super-secret-bootstrap-token",
		BootstrapCommand: "install-paperboat",
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, value.BootstrapToken) {
		t.Fatalf("enrollment response omitted one-shot bootstrap token: %s", text)
	}
}

func TestRandomEnrollmentTokenMatchesInstallerContract(t *testing.T) {
	token, err := randomEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != enrollmentTokenLength {
		t.Fatalf("token length = %d, want %d", len(token), enrollmentTokenLength)
	}
}

func TestEnvironmentInjectionRemainsInInternalServiceComposition(t *testing.T) {
	capabilities := defaultConfiguredCapabilities()
	for _, name := range []string{"file_receive", "preview_launch", "terminal_host", "session_host", "ssh_host", "environment_injection"} {
		if !slices.Contains(capabilities, name) {
			t.Fatalf("missing default %s", name)
		}
	}
	if slices.Contains(capabilities, "peer_relay") {
		t.Fatal("peer relay enabled by default")
	}

	resolved := mapCapabilities([]string{"environment_injection"}, []string{"environment_injection"})
	if !resolved.EnvironmentInjection.Configured || !resolved.EnvironmentInjection.Observed {
		t.Fatalf("resolved environment capability = %+v", resolved.EnvironmentInjection)
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"environment_injection":{"configured":true,"observed":true}`) {
		t.Fatalf("machine capability JSON omitted environment management: %s", encoded)
	}
}

func TestEnrollmentTokenMetadataParityAndLength(t *testing.T) {
	tests := []struct {
		shell string
		even  bool
	}{
		{shell: "posix", even: true},
		{shell: "powershell", even: false},
	}
	for _, test := range tests {
		token, err := randomEnrollmentTokenFor(test.shell)
		if err != nil {
			t.Fatal(err)
		}
		if len(token) != 26 {
			t.Fatalf("%s token length = %d, want 26", test.shell, len(token))
		}
		if enrollmentCharacterEven(token[0]) != test.even {
			t.Fatalf("%s token metadata = %q, wrong parity", test.shell, token[:1])
		}
	}
}

func TestEnrollmentTokenMetadataIsPartOfCredential(t *testing.T) {
	token, err := randomEnrollmentTokenFor("posix")
	if err != nil {
		t.Fatal(err)
	}
	variant := "1" + token[1:]
	if enrollmentTokenHash(token) == enrollmentTokenHash(variant) {
		t.Fatal("metadata-only token variant retained the credential hash")
	}
	changedSecret := variant[:1] + variant[1:len(variant)-1] + "0"
	if changedSecret == variant {
		changedSecret = variant[:len(variant)-1] + "1"
	}
	if enrollmentTokenHash(token) == enrollmentTokenHash(changedSecret) {
		t.Fatal("secret change did not change the credential hash")
	}
}

func enrollmentCharacterEven(character byte) bool {
	if character >= '0' && character <= '9' {
		return (character-'0')%2 == 0
	}
	return (character-'A'+1)%2 == 0
}

func TestCanonicalWorkspaceRootIsPlatformAware(t *testing.T) {
	tests := []struct {
		platform string
		root     string
		want     string
		valid    bool
	}{
		{platform: "linux", root: "/home/sailor", want: "/home/sailor", valid: true},
		{platform: "windows", root: `C:\Users\pujan`, want: `C:\Users\pujan`, valid: true},
		{platform: "windows", root: `\\server\share\workspace`, want: `\\server\share\workspace`, valid: true},
		{platform: "windows", root: `C:\Users\..\Windows`, valid: false},
		{platform: "windows", root: `\Users\pujan`, valid: false},
	}
	for _, test := range tests {
		got, valid := canonicalWorkspaceRoot(test.platform, test.root)
		if got != test.want || valid != test.valid {
			t.Errorf("canonicalWorkspaceRoot(%q, %q) = %q, %v; want %q, %v", test.platform, test.root, got, valid, test.want, test.valid)
		}
	}
}

func TestEntitlementActiveRejectsExpiredPeriod(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	if entitlementActive("active", now, now) {
		t.Fatal("expired active entitlement was accepted")
	}
	if !entitlementActive("trialing", now.Add(time.Second), now) {
		t.Fatal("current trialing entitlement was rejected")
	}
}

func TestNewDefaultsUserMachineOfflineTimeout(t *testing.T) {
	service := New(nil, nil, Policy{}, nil)
	if service.policy.OfflineAfter != 2*time.Minute {
		t.Fatalf("offline timeout = %s", service.policy.OfflineAfter)
	}
}

func TestMachineArchitectureIsExactAndPortable(t *testing.T) {
	for _, value := range []string{"amd64", "AMD64", " arm64 "} {
		if !validMachineArchitecture(value) {
			t.Fatalf("architecture %q was rejected", value)
		}
	}
	for _, value := range []string{"", "x86", "aarch64", "armv7", "amd64/x"} {
		if validMachineArchitecture(value) {
			t.Fatalf("architecture %q was accepted", value)
		}
	}
}

func TestSetupAndPairingRejectDarwinAMD64BeforePersistence(t *testing.T) {
	publicIdentityKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	service := New(nil, nil, Policy{AllowedPlatforms: []string{"darwin"}, PairingLifetime: time.Minute}, nil)
	setup := SetupInput{Alias: "intel-mac", Platform: "darwin", Architecture: "amd64", WorkspaceRoot: "/Users/paperboat", PublicIdentityKey: publicIdentityKey}
	if _, err := service.Setup(t.Context(), "usr_1", setup); !errors.Is(err, ErrInvalidSetup) {
		t.Fatalf("darwin amd64 setup error = %v", err)
	}
	pairing := PairingInput{Verifier: "verifier", Alias: "intel-mac", Platform: "darwin", Architecture: "amd64", WorkspaceRoot: "/Users/paperboat", PublicIdentityKey: publicIdentityKey}
	if err := service.validatePairing(pairing); !errors.Is(err, ErrPairingPlatformUnsupported) {
		t.Fatalf("darwin amd64 pairing error = %v", err)
	}
}

func TestConnectionDescriptorSerializesCanonicalPayload(t *testing.T) {
	expires := time.Now().UTC().Add(time.Minute)
	response := ConnectionDescriptor{
		Schema: accessdescriptor.SchemaV1, Issuer: "https://api.example", UserMachineID: "um_1", UserMachineState: "online", Connectable: true, ExpiresAt: expires,
		Capabilities: []string{accessdescriptor.CapabilityTerminal, accessdescriptor.CapabilityFileTransfer}, Status: "ready", Reason: "ready",
		Environment:  map[string]any{"id": "env_1", "kind": "machine", "resource_id": "um_1", "alias": "studio", "state": "ready", "root": "/Users/paperboat", "user_machine_id": "um_1"},
		Terminal:     map[string]any{"endpoint": "wss://edge.example/e/env_1/terminal", "session_id": "session", "cwd": "/Users/paperboat", "auth": map[string]any{"method": "websocket_ticket", "ticket": "t", "expires_at": expires, "scopes": []string{"terminal:operate"}}, "kind": "paperboat_terminal_v1"},
		FileTransfer: map[string]any{"endpoint": "https://edge.example/v1/file-transfers", "policy": accessdescriptor.FileTransferPolicy{Revision: "file-transfer-v1", MaxFileBytes: 50 << 20, MaxBatchFiles: 10, MaxBatchBytes: 500 << 20, MaxConcurrentTransfers: 2, RetentionSeconds: 604800, DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 1 << 30}, "auth": map[string]any{"method": "bearer", "token": "u", "expires_at": expires, "scopes": []string{"file:transfer"}}},
	}
	b, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"display_name", "user_machine_id", "user_machine_state", "paperboat_terminal_v1", "paperboat_staged_image_v1", "websocket_base_url", "http_base_url", `"upload"`} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("canonical payload contains legacy field %q: %s", forbidden, b)
		}
	}
}

func TestPairingJSONUsesConnectorFieldNames(t *testing.T) {
	encoded, err := json.Marshal(Pairing{ID: "ump_1", UserCode: "ABCD1234", ExpiresAt: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, field := range []string{"\"user_code\"", "\"expires_at\""} {
		if !strings.Contains(value, field) {
			t.Fatalf("pairing response missing %s: %s", field, value)
		}
	}
}

func TestMachineJSONUsesDashboardFieldNames(t *testing.T) {
	encoded, err := json.Marshal(UserMachine{ID: "um_1", EnvironmentID: "env_1", Alias: "test-mac", SeatState: "occupied", RuntimeVersions: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, field := range []string{"\"id\"", "\"environment_id\"", "\"alias\"", "\"seat_state\"", "\"runtime_versions\""} {
		if !strings.Contains(value, field) {
			t.Fatalf("machine response missing %s: %s", field, value)
		}
	}
}

func TestTerminalSessionExpiryUsesConfirmedCloseTime(t *testing.T) {
	if got := terminalSessionExpiresAt("running", sql.NullTime{}); got != nil {
		t.Fatalf("open session expiry = %v, want nil", got)
	}
	closedAt := time.Date(2026, time.September, 1, 12, 30, 0, 0, time.FixedZone("test", 5*60*60+30*60))
	closedAtValue := sql.NullTime{Time: closedAt, Valid: true}
	if got := terminalSessionExpiresAt("running", closedAtValue); got != nil {
		t.Fatalf("running session with stale close timestamp expiry = %v, want nil", got)
	}
	got := terminalSessionExpiresAt("closed", closedAtValue)
	want := closedAt.UTC().Add(30 * 24 * time.Hour)
	if got == nil || !got.Equal(want) {
		t.Fatalf("closed session expiry = %v, want %v", got, want)
	}
	reservedDefault := mapTerminalSession(dbsqlc.UserMachineTerminalSession{IsDefault: true, RuntimeState: "closed", ClosedAt: closedAtValue})
	if reservedDefault.ExpiresAt != nil {
		t.Fatalf("reserved default expiry = %v, want nil until a safe replacement flow exists", reservedDefault.ExpiresAt)
	}
}

func TestMachineMetadataValidation(t *testing.T) {
	for _, value := range []string{"", "Office", "office room", "api", "-office", strings.Repeat("a", 64)} {
		if !invalidMachineAlias(value) {
			t.Fatalf("invalid alias accepted: %q", value)
		}
	}
	b, err := json.Marshal(UserMachine{Alias: "office"})
	if err != nil || strings.Contains(string(b), "display_name") || strings.Contains(string(b), "description") {
		t.Fatalf("machine metadata JSON: %s %v", b, err)
	}
}

func TestCanonicalTerminalLaunchCWD(t *testing.T) {
	tests := []struct {
		platform, cwd, want string
		valid               bool
	}{
		{"linux", "/tmp/project", "/tmp/project", true},
		{"darwin", "/Users/sailor/My Project", "/Users/sailor/My Project", true},
		{"linux", "/tmp/space ", "/tmp/space ", true},
		{"linux", "relative", "relative", false},
		{"linux", "/tmp/../other", "/tmp/../other", false},
		{"linux", "", "", false},
		{"linux", "/tmp/\x00secret", "", false},
		{"linux", "/" + strings.Repeat("x", 1024), "", false},
		{"windows", `C:\Users\sailor\My Project`, `C:\Users\sailor\My Project`, true},
		{"windows", `\\server\share\project`, `\\server\share\project`, true},
		{"windows", `C:relative`, "", false},
		{"windows", `\relative`, "", false},
		{"windows", `C:\Users\..\Windows`, "", false},
	}
	for _, test := range tests {
		got, valid := canonicalTerminalLaunchCWD(test.platform, test.cwd)
		if valid != test.valid || got != test.want {
			t.Errorf("canonicalTerminalLaunchCWD(%q, %q) = %q, %v; want %q, %v", test.platform, test.cwd, got, valid, test.want, test.valid)
		}
	}
}

func TestMachineTerminalEndpointsPreserveCustomerInfrastructureRouting(t *testing.T) {
	customer := machineTerminalEndpoints("wss://edge.customer.test/v1/runtime/machine.customer.test")
	if customer["wss"] != "wss://edge.customer.test/v1/runtime/machine.customer.test/v1/runtime" {
		t.Fatalf("customer runtime prefix was lost: %#v", customer)
	}
	if _, ok := customer["quic"]; ok {
		t.Fatal("advertised QUIC cannot carry customer logical runtime route")
	}
	managed := machineTerminalEndpoints("wss://machine.runtime.example.test")
	if managed["wss"] != "wss://machine.runtime.example.test/v1/runtime" || managed["quic"] != "quic://machine.runtime.example.test:443" {
		t.Fatalf("managed terminal endpoints changed: %#v", managed)
	}
}
