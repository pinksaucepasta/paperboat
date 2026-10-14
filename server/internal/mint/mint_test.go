package mint

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKey(seedByte byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed([]byte(strings.Repeat(string([]byte{seedByte}), ed25519.SeedSize)))
}

func TestSignPublishesFrozenClaims(t *testing.T) {
	provider, err := New([]Key{{ID: "key-2", PrivateKey: testKey(2)}, {ID: "key-1", PrivateKey: testKey(1)}}, "key-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Unix(1_700_000_000, 0)
	token, err := provider.Sign(ProofInput{Issuer: "https://api.example.test", EnvironmentID: "env_1", UserID: "usr_1", CLIClientSessionID: "cls_1", JTI: "jti_1", Nonce: "nonce_1", IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	var header map[string]any
	var payload map[string]any
	decodeJSONPart(t, parts[0], &header)
	decodeJSONPart(t, parts[1], &payload)
	if header["alg"] != "EdDSA" || header["typ"] != ProofType || header["kid"] != "key-2" {
		t.Fatalf("header = %#v", header)
	}
	if payload["aud"] != "t3-env:env_1" || payload["sub"] != "usr_1" || payload["clientSessionId"] != "cls_1" {
		t.Fatalf("payload = %#v", payload)
	}
	if _, ok := payload["cnf"]; ok {
		t.Fatal("unexpected cnf claim")
	}
	if !ed25519.Verify(testKey(2).Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), mustDecode(t, parts[2])) {
		t.Fatal("invalid signature")
	}
}

func TestSignRelayGrantPublishesFrozenTypeAndRejectsLongLifetime(t *testing.T) {
	provider, err := New([]Key{{ID: "relay-key", PrivateKey: testKey(31)}}, "relay-key", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{"version": 1, "iss": "https://api.example.test", "aud": "paperboat-relay", "iat": int64(1_700_000_000), "exp": int64(1_700_000_060), "generation": int64(4), "account_id": "account_1", "endpoint_id": "endpoint_1", "wireguard_public_key": strings.Repeat("A", 43), "quic_certificate_fingerprint": strings.Repeat("0", 64), "node_id": "node_1", "node_generation": int64(3), "process_epoch": "epoch_1", "peers": []any{}}
	token, err := provider.SignRelayGrant(claims)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	var header map[string]any
	decodeJSONPart(t, parts[0], &header)
	if header["alg"] != "EdDSA" || header["typ"] != RelayGrantType || header["kid"] != "relay-key" {
		t.Fatalf("header = %#v", header)
	}
	claims["exp"] = int64(1_700_000_061)
	if _, err := provider.SignRelayGrant(claims); err == nil {
		t.Fatal("accepted relay grant longer than 60 seconds")
	}
	claims["exp"] = int64(1_700_000_060)
	claims["peer_relay"] = map[string]any{"wireguard_public_key": strings.Repeat("A", 43), "disco_public_key": strings.Repeat("A", 43), "virtual_address": "fd7a:115c:a1e0::9"}
	if _, err := provider.SignRelayGrant(claims); err == nil {
		t.Fatal("accepted peer relay descriptor without endpoint discovery identity")
	}
	claims["disco_public_key"] = strings.Repeat("A", 43)
	if _, err := provider.SignRelayGrant(claims); err != nil {
		t.Fatalf("valid peer relay descriptor: %v", err)
	}
	scope := map[string]any{"resource_kind": "machine_access", "resource_id": "access_1", "resource_generation": 1, "capability": "file_transfer", "direction": "dial", "port": 443, "expires_at": int64(1_700_000_005)}
	claims["peers"] = []any{map[string]any{"wireguard_public_key": strings.Repeat("A", 43), "disco_public_key": strings.Repeat("A", 43), "scopes": []any{scope}}}
	token, err = provider.SignRelayGrant(claims)
	if err != nil {
		t.Fatalf("short live application scope rejected: %v", err)
	}
	parts = strings.Split(token, ".")
	if !ed25519.Verify(testKey(31).Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), mustDecode(t, parts[2])) {
		t.Fatal("invalid relay signature")
	}
	var body map[string]any
	decodeJSONPart(t, parts[1], &body)
	if body["exp"].(float64) != 1_700_000_060 || body["peers"].([]any)[0].(map[string]any)["scopes"].([]any)[0].(map[string]any)["expires_at"].(float64) != 1_700_000_005 {
		t.Fatal("relay or application lifetime changed")
	}
	for _, expiry := range []int64{1_700_000_000, 1_699_999_999} {
		scope["expires_at"] = expiry
		if _, err := provider.SignRelayGrant(claims); err == nil {
			t.Fatal("expired application scope signed")
		}
	}
}

func TestJWKSIncludesRotationOverlapKeys(t *testing.T) {
	provider, err := New([]Key{{ID: "current", PrivateKey: testKey(3)}, {ID: "previous", PrivateKey: testKey(4)}}, "current", 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	provider.ServeHTTP(recorder, httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=90" {
		t.Fatalf("cache-control = %q", got)
	}
	var body struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Keys) != 2 {
		t.Fatalf("keys = %d", len(body.Keys))
	}
	ids := map[string]bool{}
	for _, key := range body.Keys {
		ids[key["kid"]] = true
		if key["kty"] != "OKP" || key["crv"] != "Ed25519" || key["alg"] != "EdDSA" || key["use"] != "sig" {
			t.Fatalf("key = %#v", key)
		}
	}
	if !ids["current"] || !ids["previous"] {
		t.Fatalf("ids = %#v", ids)
	}
}

func TestSigningKeyRollbackUsesPublishedOverlapKey(t *testing.T) {
	keys := []Key{{ID: "current", PrivateKey: testKey(7)}, {ID: "previous", PrivateKey: testKey(8)}}
	now := time.Unix(1_700_000_000, 0)
	input := ProofInput{Issuer: "issuer", EnvironmentID: "env", UserID: "user", CLIClientSessionID: "client", JTI: "jti", Nonce: "nonce", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	current, err := New(keys, "current", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rolledBack, err := New(keys, "previous", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	currentToken, _ := current.Sign(input)
	rollbackToken, _ := rolledBack.Sign(input)
	var currentHeader map[string]any
	var rollbackHeader map[string]any
	decodeJSONPart(t, strings.Split(currentToken, ".")[0], &currentHeader)
	decodeJSONPart(t, strings.Split(rollbackToken, ".")[0], &rollbackHeader)
	if currentHeader["kid"] != "current" || rollbackHeader["kid"] != "previous" {
		t.Fatalf("current=%#v rollback=%#v", currentHeader, rollbackHeader)
	}
}

func TestSignRejectsOverlongProof(t *testing.T) {
	provider, _ := New([]Key{{ID: "key", PrivateKey: testKey(5)}}, "key", time.Minute)
	now := time.Now()
	_, err := provider.Sign(ProofInput{Issuer: "issuer", EnvironmentID: "env", UserID: "user", CLIClientSessionID: "client", JTI: "jti", Nonce: "nonce", IssuedAt: now, ExpiresAt: now.Add(MaxProofTTL + time.Second)})
	if err == nil {
		t.Fatal("expected lifetime error")
	}
}

func TestSignCredentialUsesExactClassBindings(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	provider, err := New([]Key{{ID: "key-1", PrivateKey: testKey(1)}}, "key-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.SignCredential(CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-enrollment", Subject: "env_1", JTI: "jti_enroll_1", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "helper_enrollment", Scopes: []string{"helper:enroll"}, EnvironmentID: "env_1", EnrollmentID: "enr_1"})
	if err != nil || token == "" {
		t.Fatalf("enrollment credential = %q, %v", token, err)
	}
	claims, err := provider.VerifyCredential(token, "https://api.example.test", "helper_enrollment", now)
	if err != nil || claims.EnrollmentID != "enr_1" || claims.EnvironmentID != "env_1" {
		t.Fatalf("verified claims = %#v, %v", claims, err)
	}
	tampered := token[:len(token)-1] + "A"
	if _, err := provider.VerifyCredential(tampered, "https://api.example.test", "helper_enrollment", now); err == nil {
		t.Fatal("tampered credential accepted")
	} else {
		var verificationError CredentialVerificationError
		if !errors.As(err, &verificationError) || verificationError.Code == "" {
			t.Fatalf("verification failure did not expose its typed code: %v", err)
		}
	}
	if _, err := provider.SignCredential(CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-enrollment", Subject: "env_1", JTI: "jti_enroll_2", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "helper_enrollment", Scopes: []string{"helper:connect"}, EnvironmentID: "env_1", EnrollmentID: "enr_1"}); err == nil {
		t.Fatal("broader or wrong scope accepted")
	}
}

func TestPrivateAccessCredentialUsesOnlyProtocolAudiences(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	provider, err := New([]Key{{ID: "private-key", PrivateKey: testKey(21)}}, "private-key", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	input := CredentialInput{
		Issuer: "https://api.example.test", Audience: "paperboat-preview-http", Subject: "acct_1",
		JTI: "nonce_1", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "private_access",
		Scopes: []string{"private:access"}, EnvironmentID: "paperboat-private-access", AccountID: "acct_1",
		UserID: "user_1", MachineID: "machine_1", SessionID: "session_1", ResourceKind: "preview",
		ResourceID: "preview_1", RouteID: "route_1", OperationID: "operation_1", Protocol: "http",
		CarrierSessionID: "carrier_session_1", RouteGeneration: 1, ProcessGeneration: 1, ConfigGeneration: 1,
		InstallationGeneration: 2, SessionGeneration: 3, AssignmentGeneration: 4, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001",
		Method: "GET", Host: "preview.example.test", Path: "/api", AccessMode: "private", RequestHash: strings.Repeat("a", 64),
		IdempotencyKey: "idem_1", RequestID: "req_001", CorrelationID: "corr_001",
	}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, input.CredentialClass, now)
	if err != nil || claims.Audience != input.Audience || claims.ResourceID != input.ResourceID || claims.InstallationGeneration != input.InstallationGeneration || claims.SessionGeneration != input.SessionGeneration || claims.AssignmentGeneration != input.AssignmentGeneration || claims.EdgeNodeID != input.EdgeNodeID || claims.EdgeProcessEpoch != input.EdgeProcessEpoch {
		t.Fatalf("private access claims = %#v, %v", claims, err)
	}
	for _, audience := range []string{"paperboat-edge", "paperboat-control", "paperboat-private-access", ""} {
		invalid := input
		invalid.Audience = audience
		if _, err := provider.SignCredential(invalid); err == nil {
			t.Fatalf("audience %q accepted for private access", audience)
		}
	}
}

func TestNativePrivateCredentialBindsExactLoopbackTarget(t *testing.T) {
	provider, err := NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_native_private_1", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "native_private", Scopes: []string{"private:native"}, EnvironmentID: "env_1", AccountID: "acct_1", MachineID: "machine_1", UserID: "usr_1", CLIClientSessionID: "cli_1", AssignmentID: "umas_1", OperationID: "operation_private_1", ResourceKind: "machine_service", ResourceID: "machine_1", RouteID: "tcp:22", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpectedGeneration: 2, RouteGeneration: 3, TargetGeneration: 4, InstallationGeneration: 2, BootID: "boot-012345678901", PolicyGeneration: 3, AnnouncementGeneration: 4}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, input.CredentialClass, now)
	if err != nil || claims.ResourceID != input.ResourceID || claims.RouteID != input.RouteID || claims.TargetAddress != input.TargetAddress || claims.TargetGeneration != input.TargetGeneration || claims.AssignmentID != input.AssignmentID || claims.CLIClientSessionID != input.CLIClientSessionID {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	for name, mutate := range map[string]func(*CredentialInput){
		"network target":    func(v *CredentialInput) { v.TargetAddress = "192.0.2.1:22" },
		"protocol":          func(v *CredentialInput) { v.Protocol = "http" },
		"target generation": func(v *CredentialInput) { v.TargetGeneration = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := input
			changed.JTI += name
			mutate(&changed)
			if _, err := provider.SignCredential(changed); err == nil {
				t.Fatal("invalid binding signed")
			}
		})
	}
}

func TestSignCredentialExecOperationBindsExactOperation(t *testing.T) {
	provider, err := New([]Key{{ID: "key-1", PrivateKey: testKey(1)}}, "key-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_exec_1", SourceMachineID: "machine_1", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "exec_operation", WorkspaceID: "personal", ActorAccountID: "usr_1", Scopes: []string{"exec:operate"}, EnvironmentID: "env_1", MachineID: "machine_1", UserID: "usr_1", CLIClientSessionID: "cli_1", OperationID: "operation_exec_1"}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, "exec_operation", now.Add(time.Minute))
	if err != nil || claims.OperationID != input.OperationID || claims.SourceMachineID != input.SourceMachineID {
		t.Fatalf("claims=%#v err=%v", claims, err)
	}
	missingSource := input
	missingSource.SourceMachineID = ""
	if _, err := provider.SignCredential(missingSource); err == nil {
		t.Fatal("missing source endpoint binding accepted")
	}
	input.OperationID = ""
	if _, err := provider.SignCredential(input); err == nil {
		t.Fatal("missing operation binding accepted")
	}
	input.OperationID = "operation_exec_1"
	input.Scopes = []string{"terminal:operate"}
	if _, err := provider.SignCredential(input); err == nil {
		t.Fatal("terminal scope accepted for exec credential")
	}
}

func TestTerminalOperationRequiresOneExactRoleScope(t *testing.T) {
	provider, err := NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_terminal_1", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "terminal_operation", WorkspaceID: "personal", ActorAccountID: "usr_1", EnvironmentID: "env_1", AccountID: "usr_1", MachineID: "machine_1", UserID: "usr_1", CLIClientSessionID: "cli_1", SessionID: "session_1", AssignmentID: "access_1", ExpectedGeneration: 7}
	for _, scope := range []string{"terminal:view", "terminal:control", "terminal:operate"} {
		input := base
		input.Scopes = []string{scope}
		input.JTI += scope
		token, signErr := provider.SignCredential(input)
		if signErr != nil {
			t.Fatalf("sign %s: %v", scope, signErr)
		}
		claims, verifyErr := provider.VerifyCredential(token, input.Issuer, "terminal_operation", now)
		if verifyErr != nil || len(claims.Scopes) != 1 || claims.Scopes[0] != scope {
			t.Fatalf("verify %s: %#v %v", scope, claims.Scopes, verifyErr)
		}
		if scope != "terminal:operate" && claims.AccountID != input.AccountID {
			t.Fatalf("shared %s omitted account binding: %#v", scope, claims)
		}
		if scope != "terminal:operate" && claims.ExpectedGeneration != input.ExpectedGeneration {
			t.Fatalf("shared %s omitted process generation: %#v", scope, claims)
		}
	}
	for _, scopes := range [][]string{nil, {"terminal:view", "terminal:control"}, {"terminal:admin"}, {"terminal:operate", "terminal:operate"}} {
		input := base
		input.Scopes = scopes
		if _, err := provider.SignCredential(input); err == nil {
			t.Fatalf("accepted scopes %#v", scopes)
		}
	}
	missingAccount := base
	missingAccount.AccountID = ""
	missingAccount.Scopes = []string{"terminal:view"}
	if _, err := provider.SignCredential(missingAccount); err == nil {
		t.Fatal("shared terminal credential accepted without account binding")
	}
	mismatchedAccount := base
	mismatchedAccount.AccountID = "other_account"
	mismatchedAccount.Scopes = []string{"terminal:control"}
	if _, err := provider.SignCredential(mismatchedAccount); err == nil {
		t.Fatal("shared terminal credential accepted a different account and user")
	}
}

func TestBrowserTerminalCredentialUsesBrowserAttachmentIdentity(t *testing.T) {
	provider, err := NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_browser_terminal_1", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "browser_terminal_operation", WorkspaceID: "personal", ActorAccountID: "usr_1", Scopes: []string{"terminal:view"}, EnvironmentID: "env_1", AccountID: "usr_1", MachineID: "machine_1", UserID: "usr_1", SessionID: "session_1", BrowserAttachmentID: "browser_attachment_1", BrowserPublicKeySHA256: strings.Repeat("A", 43), ExpectedGeneration: 4, PolicyGeneration: 2}
	token, err := provider.SignCredential(base)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, base.Issuer, base.CredentialClass, now)
	if err != nil || claims.BrowserAttachmentID != base.BrowserAttachmentID || claims.BrowserPublicKeySHA256 != base.BrowserPublicKeySHA256 || claims.CLIClientSessionID != "" || claims.PolicyGeneration != base.PolicyGeneration {
		t.Fatalf("browser credential bindings = %#v, %v", claims, err)
	}
	if _, err := provider.VerifyCredential(token, base.Issuer, "terminal_operation", now); err == nil {
		t.Fatal("browser credential accepted as a native terminal credential")
	}
	for _, mutate := range []func(*CredentialInput){
		func(in *CredentialInput) { in.BrowserAttachmentID = "" },
		func(in *CredentialInput) { in.BrowserPublicKeySHA256 = "" },
		func(in *CredentialInput) { in.CLIClientSessionID = "cli_1" },
		func(in *CredentialInput) { in.SourceMachineID = "machine_2" },
		func(in *CredentialInput) { in.PolicyGeneration = 0 },
		func(in *CredentialInput) { in.AccountID = "other" },
	} {
		invalid := base
		mutate(&invalid)
		if _, err := provider.SignCredential(invalid); err == nil {
			t.Fatalf("accepted invalid browser terminal binding: %#v", invalid)
		}
	}
}

func TestPreviewLaunchCredentialBindsLeaseAndTraceFields(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	provider, err := NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	input := CredentialInput{
		Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "user_1", JTI: "jti_preview_1",
		IssuedAt: now, ExpiresAt: now.Add(2 * time.Minute), CredentialClass: "preview_launch", Scopes: []string{"preview:launch"},
		EnvironmentID: "env_1", AccountID: "acct_1", MachineID: "machine_1", UserID: "user_1", ActorID: "user_1",
		OperationID: "operation_1", PreviewID: "prv_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground", IdempotencyKey: "create_1",
		RequestID: "request_1", CorrelationID: "correlation_1", TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public",
		Endpoint: "https://abc.preview.example.test", LeaseDeadline: now.Add(time.Hour), LeaseETag: `"ptv1:preview_lease:cHJ2XzE:1"`,
		State: "allocating", AllocationState: "pending", EdgeState: "pending", OriginState: "unknown", CreatedAt: now, LastRenewedAt: now,
		ExpectedGeneration: 1, RequestHash: strings.Repeat("a", 64),
	}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, input.CredentialClass, now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AccountID != input.AccountID || claims.ActorID != input.ActorID || claims.PreviewID != input.PreviewID || claims.OperationID != input.OperationID || claims.OwnerSessionID != input.OwnerSessionID || claims.OwnerSessionKind != input.OwnerSessionKind || claims.LeaseETag != input.LeaseETag || claims.ExpectedGeneration != input.ExpectedGeneration || claims.RequestHash != input.RequestHash || claims.IdempotencyKey != input.IdempotencyKey || claims.RequestID != input.RequestID || claims.CorrelationID != input.CorrelationID {
		t.Fatalf("claims = %#v", claims)
	}
	for index, mutate := range []func(*CredentialInput){
		func(value *CredentialInput) { value.OwnerSessionKind = "" },
		func(value *CredentialInput) { value.OwnerSessionKind = "unknown" },
		func(value *CredentialInput) { value.IdempotencyKey = "" },
		func(value *CredentialInput) { value.RequestID = "" },
		func(value *CredentialInput) { value.CorrelationID = "" },
		func(value *CredentialInput) { value.RequestHash = strings.Repeat("b", 63) },
		func(value *CredentialInput) { value.ExpectedGeneration = 0 },
	} {
		invalid := input
		mutate(&invalid)
		if _, err := provider.SignCredential(invalid); err == nil {
			t.Fatalf("mutation %d accepted", index)
		}
	}
	if strings.Contains(token, "127.0.0.1:3000") {
		t.Fatal("credential unexpectedly exposed raw target address")
	}
}

func TestSignCredentialSSHOperationIsNotInterchangeable(t *testing.T) {
	provider, err := New([]Key{{ID: "key-1", PrivateKey: testKey(1)}}, "key-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_ssh_1", SourceMachineID: "machine_source", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "ssh_operation", WorkspaceID: "personal", ActorAccountID: "usr_1", Scopes: []string{"ssh:operate"}, EnvironmentID: "env_1", MachineID: "machine_1", UserID: "usr_1", CLIClientSessionID: "cli_1", OperationID: "operation_ssh_1"}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, "ssh_operation", now.Add(time.Minute))
	if err != nil || claims.OperationID != input.OperationID || claims.SourceMachineID != input.SourceMachineID {
		t.Fatalf("claims=%#v err=%v", claims, err)
	}
	missingSource := input
	missingSource.SourceMachineID = ""
	if _, err := provider.SignCredential(missingSource); err == nil {
		t.Fatal("missing source endpoint binding accepted")
	}
	if _, err := provider.VerifyCredential(token, input.Issuer, "exec_operation", now.Add(time.Minute)); err == nil {
		t.Fatal("ssh credential accepted as exec credential")
	}
	input.CredentialClass, input.Scopes = "exec_operation", []string{"exec:operate"}
	execToken, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.VerifyCredential(execToken, input.Issuer, "ssh_operation", now.Add(time.Minute)); err == nil {
		t.Fatal("exec credential accepted as ssh credential")
	}
}

func TestVerifyCredentialExpiryGraceIsExplicitAndBounded(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	provider, err := New([]Key{{ID: "key-1", PrivateKey: testKey(1)}}, "key-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.SignCredential(CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-control", Subject: "helper_1", JTI: "jti_identity_1", IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), CredentialClass: "helper_identity", Scopes: []string{"helper:connect", "helper:renew"}, EnvironmentID: "env_1", HelperID: "helper_1", MachineID: "machine_1", KeyThumbprint: "sha256:key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.VerifyCredential(token, "https://api.example.test", "helper_identity", now); err == nil {
		t.Fatal("strict verification accepted expired credential")
	}
	if _, err := provider.VerifyCredentialWithExpiryGrace(token, "https://api.example.test", "helper_identity", now, 2*time.Hour); err != nil {
		t.Fatalf("grace verification rejected credential: %v", err)
	}
	if _, err := provider.VerifyCredentialWithExpiryGrace(token, "https://api.example.test", "helper_identity", now, 30*time.Minute); err == nil {
		t.Fatal("grace verification accepted credential outside grace")
	}
}

func TestMachineControlCredentialRequiresAllMachineBindings(t *testing.T) {
	provider, err := NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-control", Subject: "mch_1", JTI: "mcc_1", IssuedAt: now, ExpiresAt: now.Add(time.Hour), CredentialClass: "machine_control", Scopes: []string{"machine:connect", "machine:renew"}, EnvironmentID: "env_1", MachineID: "mch_1", UserID: "usr_1", KeyThumbprint: "sha256:key", InstallationGeneration: 2, SessionGeneration: 3}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, "machine_control", now)
	if err != nil || claims.UserID != input.UserID || claims.MachineID != input.MachineID || claims.InstallationGeneration != 2 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	input.InstallationGeneration = 0
	if _, err := provider.SignCredential(input); err == nil {
		t.Fatal("credential without installation generation was accepted")
	}
	input.InstallationGeneration, input.SessionGeneration = 2, 0
	if _, err := provider.SignCredential(input); err == nil {
		t.Fatal("credential without session generation was accepted")
	}
}

func TestSignCredentialConfigSyncBindsAssignmentAndWarning(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	provider, err := New([]Key{{ID: "key-config", PrivateKey: testKey(11)}}, "key-config", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.SignCredential(CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "machine_1", JTI: "jti_config_1", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "config_sync", Scopes: []string{"config:pull", "config:apply", "config:report"}, EnvironmentID: "env_1", MachineID: "machine_1", InstallationGeneration: 2, AssignmentID: "assignment_1", WarningRevision: "warning_7"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, "https://api.example.test", "config_sync", now)
	if err != nil || claims.AssignmentID != "assignment_1" || claims.WarningRevision != "warning_7" || claims.MachineID != "machine_1" || claims.InstallationGeneration != 2 {
		t.Fatalf("claims = %#v, %v", claims, err)
	}
	if _, err := provider.SignCredential(CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "machine_1", JTI: "jti_config_2", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "config_sync", Scopes: []string{"config:pull"}, EnvironmentID: "env_1", MachineID: "machine_1", InstallationGeneration: 2, AssignmentID: "assignment_1", WarningRevision: "warning_7"}); err == nil {
		t.Fatal("incomplete config scopes accepted")
	}
}

func TestSignRevocationUsesSeparateTypeAndScope(t *testing.T) {
	provider, _ := New([]Key{{ID: "key", PrivateKey: testKey(6)}}, "key", time.Minute)
	now := time.Unix(1_700_000_000, 0)
	token, err := provider.SignRevocation(RevocationInput{
		ProofInput: ProofInput{Issuer: "issuer", EnvironmentID: "env", UserID: "user", CLIClientSessionID: "client", JTI: "jti", Nonce: "nonce", IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
		SessionIDs: []string{"session-1"}, Reason: "logout",
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	var header map[string]any
	var payload map[string]any
	decodeJSONPart(t, parts[0], &header)
	decodeJSONPart(t, parts[1], &payload)
	if header["typ"] != RevokeType || payload["reason"] != "logout" {
		t.Fatalf("header=%#v payload=%#v", header, payload)
	}
	if scope := payload["scope"].([]any); len(scope) != 1 || scope[0] != RevokeScope {
		t.Fatalf("scope=%#v", scope)
	}
}

func TestSignHealthUsesDedicatedTypeAndScope(t *testing.T) {
	provider, _ := New([]Key{{ID: "health-key", PrivateKey: testKey(9)}}, "health-key", time.Minute)
	now := time.Unix(1_700_000_000, 0)
	token, err := provider.SignHealth(ProofInput{
		Issuer: "https://paperboat.example", EnvironmentID: "env_1", UserID: "usr_1",
		CLIClientSessionID: "cls_1", JTI: "health-jti", Nonce: "health-nonce",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	var header map[string]any
	var payload map[string]any
	decodeJSONPart(t, parts[0], &header)
	decodeJSONPart(t, parts[1], &payload)
	if header["typ"] != HealthType || header["kid"] != "health-key" {
		t.Fatalf("header=%#v", header)
	}
	if payload["aud"] != "t3-env:env_1" || payload["sub"] != "usr_1" || payload["environmentId"] != "env_1" || payload["clientSessionId"] != "cls_1" {
		t.Fatalf("payload=%#v", payload)
	}
	if scope := payload["scope"].([]any); len(scope) != 1 || scope[0] != HealthScope {
		t.Fatalf("scope=%#v", scope)
	}
}

func TestFileTransferCredentialIsSessionAndClientBound(t *testing.T) {
	provider, _ := New([]Key{{ID: "transfer-key", PrivateKey: testKey(10)}}, "transfer-key", time.Minute)
	now := time.Unix(1_700_000_000, 0)
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_transfer_1", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "file_transfer", Scopes: []string{"file:transfer"}, EnvironmentID: "env_1", MachineID: "machine_1", SourceMachineID: "machine_source", UserID: "usr_1", CLIClientSessionID: "cli_1", SessionID: "ses_1"}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, "file_transfer", now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.CLIClientSessionID != "cli_1" || claims.SessionID != "ses_1" || len(claims.Scopes) != 1 || claims.Scopes[0] != "file:transfer" {
		t.Fatalf("claims=%#v", claims)
	}
	input.Scopes = []string{"file:stage"}
	if _, err := provider.SignCredential(input); err == nil {
		t.Fatal("accepted legacy scope")
	}
}

func TestFileTransferCredentialAllowsNoTerminalSession(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	provider, err := New([]Key{{ID: "transfer-key", PrivateKey: testKey(11)}}, "transfer-key", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "usr_1", JTI: "jti_transfer_2", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "file_transfer", Scopes: []string{"file:transfer"}, EnvironmentID: "env_1", MachineID: "machine_1", SourceMachineID: "machine_source", UserID: "usr_1", CLIClientSessionID: "cli_1"}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, "file_transfer", now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SessionID != "" || claims.SourceMachineID != input.SourceMachineID {
		t.Fatalf("claims=%+v", claims)
	}
}

func decodeJSONPart(t *testing.T, part string, target any) {
	t.Helper()
	if err := json.Unmarshal(mustDecode(t, part), target); err != nil {
		t.Fatal(err)
	}
}
func mustDecode(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestFileTransferCredentialApprovalBinding(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	provider, err := New([]Key{{ID: "file-approval", PrivateKey: testKey(12)}}, "file-approval", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "member", JTI: "file-approval-credential", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "file_transfer", Scopes: []string{"file:transfer"}, EnvironmentID: "environment", MachineID: "destination", SourceMachineID: "source", UserID: "member", CLIClientSessionID: "member-client", RequestID: "inbox_request", IdempotencyKey: "approved_batch", RequestHash: strings.Repeat("a", 64)}
	t.Run("roundtrip", func(t *testing.T) {
		for _, binding := range []struct{ request, batch, digest string }{{input.RequestID, input.IdempotencyKey, input.RequestHash}, {"r", "b", input.RequestHash}, {strings.Repeat("r", 256), strings.Repeat("b", 128), input.RequestHash}, {"", "", ""}} {
			current := input
			current.RequestID, current.IdempotencyKey, current.RequestHash = binding.request, binding.batch, binding.digest
			token, err := provider.SignCredential(current)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := provider.VerifyCredential(token, input.Issuer, "file_transfer", now)
			if err != nil || claims.RequestID != binding.request || claims.IdempotencyKey != binding.batch || claims.RequestHash != binding.digest {
				t.Fatal("signed file credential lost or changed its exact approval binding")
			}
		}
	})
	invalid := map[string]func(*CredentialInput){
		"missing request":  func(in *CredentialInput) { in.RequestID = "" },
		"missing batch":    func(in *CredentialInput) { in.IdempotencyKey = "" },
		"missing digest":   func(in *CredentialInput) { in.RequestHash = "" },
		"request alone":    func(in *CredentialInput) { in.IdempotencyKey = ""; in.RequestHash = "" },
		"batch alone":      func(in *CredentialInput) { in.RequestID = ""; in.RequestHash = "" },
		"digest alone":     func(in *CredentialInput) { in.RequestID = ""; in.IdempotencyKey = "" },
		"long request":     func(in *CredentialInput) { in.RequestID = strings.Repeat("r", 257) },
		"long batch":       func(in *CredentialInput) { in.IdempotencyKey = strings.Repeat("b", 129) },
		"invalid request":  func(in *CredentialInput) { in.RequestID = "request/other" },
		"invalid batch":    func(in *CredentialInput) { in.IdempotencyKey = " batch" },
		"short digest":     func(in *CredentialInput) { in.RequestHash = strings.Repeat("a", 63) },
		"nonhex digest":    func(in *CredentialInput) { in.RequestHash = strings.Repeat("g", 64) },
		"uppercase digest": func(in *CredentialInput) { in.RequestHash = strings.Repeat("A", 64) },
	}
	baseToken, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			current := input
			mutate(&current)
			if _, err := provider.SignCredential(current); err == nil {
				t.Error("signer accepted invalid approval binding")
			}
			var claims map[string]any
			decodeJSONPart(t, strings.Split(baseToken, ".")[1], &claims)
			claims["request_id"], claims["idempotency_key"], claims["request_hash"] = current.RequestID, current.IdempotencyKey, current.RequestHash
			token, err := provider.signClaims("paperboat-credential+jwt", claims)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.VerifyCredential(token, input.Issuer, "file_transfer", now); err == nil {
				t.Error("verifier accepted invalid approval binding")
			}
		})
	}
}

func TestMachineServiceCredentialBindsRuntimeEpochs(t *testing.T) {
	provider, err := NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "caller", JTI: "native-machine-test", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "native_private", Scopes: []string{"private:native"}, EnvironmentID: "environment", AccountID: "caller", UserID: "caller", MachineID: "machine", CLIClientSessionID: "cli", AssignmentID: "access", OperationID: "machine-service-test", ResourceKind: "machine_service", ResourceID: "machine", RouteID: "tcp:5432", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpectedGeneration: 1, RouteGeneration: 2, TargetGeneration: 3, InstallationGeneration: 1, BootID: "boot-012345678901", PolicyGeneration: 2, AnnouncementGeneration: 3}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, "native_private", now)
	if err != nil || claims.BootID != input.BootID || claims.InstallationGeneration != 1 || claims.PolicyGeneration != 2 || claims.AnnouncementGeneration != 3 || claims.AssignmentID != "access" {
		t.Fatalf("runtime binding %+v %v", claims, err)
	}
	for name, change := range map[string]func(*CredentialInput){"wrong machine": func(v *CredentialInput) { v.ResourceID = "other" }, "wrong route": func(v *CredentialInput) { v.RouteID = "tcp:22" }, "missing boot": func(v *CredentialInput) { v.BootID = "" }, "missing policy": func(v *CredentialInput) { v.PolicyGeneration = 0 }, "non-machine epochs": func(v *CredentialInput) { v.ResourceKind = "tunnel" }} {
		t.Run(name, func(t *testing.T) {
			bad := input
			change(&bad)
			if _, e := provider.SignCredential(bad); e == nil {
				t.Fatal("invalid native binding signed")
			}
		})
	}
}

func TestConfigCompareCredentialsBindOnlyExactRead(t *testing.T) {
	now := time.Now().UTC()
	provider, err := New([]Key{{ID: "key-1", PrivateKey: testKey(1)}}, "key-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "account", JTI: "token_compare", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "config_compare", Scopes: []string{"config:compare"}, EnvironmentID: "environment", AccountID: "account", UserID: "account", MachineID: "machine", InstallationGeneration: 2, SourceMachineID: "source", CLIClientSessionID: "client", OperationID: "operation_compare", AssignmentID: "assignment", AssignmentVersion: 3, ConfigPath: "settings/editor", ConflictRevision: strings.Repeat("a", 64), ExpectedRemoteRevision: "remote"}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, "config_compare", now)
	if err != nil || claims.AssignmentVersion != 3 || claims.ConfigPath != input.ConfigPath || claims.AssignmentID != input.AssignmentID || claims.InstallationGeneration != 2 || claims.SessionID != "" {
		t.Fatalf("compare bindings lost: %v", err)
	}
	for _, modify := range []func(*CredentialInput){func(in *CredentialInput) { in.Scopes = []string{"terminal:operate"} }, func(in *CredentialInput) { in.ConfigPath = "" }, func(in *CredentialInput) { in.AssignmentVersion = 0 }, func(in *CredentialInput) { in.SourceMachineID = "" }, func(in *CredentialInput) { in.InstallationGeneration = 0 }, func(in *CredentialInput) { in.AccountID = "other" }} {
		changed := input
		modify(&changed)
		if _, err = provider.SignCredential(changed); err == nil {
			t.Fatal("incomplete/elevated comparison credential minted")
		}
	}
}

func TestProcessCredentialContextIsSignedAndRequiredBeforeLaunch(t *testing.T) {
	provider, err := NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for class, scope := range map[string]string{"terminal_operation": "terminal:operate", "exec_operation": "exec:operate", "ssh_operation": "ssh:operate", "browser_terminal_operation": "terminal:operate"} {
		t.Run(class, func(t *testing.T) {
			input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "account", JTI: "token_context", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: class, Scopes: []string{scope}, EnvironmentID: "environment", AccountID: "account", UserID: "account", WorkspaceID: "team_creation", ActorAccountID: "account", MachineID: "machine", CLIClientSessionID: "client", SessionID: "terminal", OperationID: "operation_context", BrowserAttachmentID: "attachment", BrowserPublicKeySHA256: strings.Repeat("A", 43), ExpectedGeneration: 1, PolicyGeneration: 1}
			if class == "exec_operation" || class == "ssh_operation" {
				input.SourceMachineID = "source-machine"
			}
			if class == "browser_terminal_operation" {
				input.CLIClientSessionID = ""
			}
			token, err := provider.SignCredential(input)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := provider.VerifyCredential(token, input.Issuer, class, now)
			if err != nil || claims.WorkspaceID != input.WorkspaceID || claims.ActorAccountID != input.ActorAccountID {
				t.Fatalf("process context was not authenticated: %v", err)
			}
			for _, mutate := range []func(*CredentialInput){func(in *CredentialInput) { in.WorkspaceID = "" }, func(in *CredentialInput) { in.ActorAccountID = "" }, func(in *CredentialInput) { in.ActorAccountID = "other-account" }} {
				invalid := input
				mutate(&invalid)
				if _, err := provider.SignCredential(invalid); err == nil {
					t.Fatal("missing or different launch context accepted")
				}
			}
		})
	}
}

func TestBrowserGuestContextDoesNotInventAccountLaunchAuthority(t *testing.T) {
	provider, err := NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: "guest_session", JTI: "guest_context", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "browser_terminal_operation", Scopes: []string{"terminal:control"}, EnvironmentID: "environment", AccountID: "guest_session", UserID: "guest_session", WorkspaceID: "team_creation", MachineID: "machine", SessionID: "terminal", BrowserAttachmentID: "attachment", BrowserPublicKeySHA256: strings.Repeat("A", 43), ExpectedGeneration: 1, PolicyGeneration: 1}
	token, err := provider.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := provider.VerifyCredential(token, input.Issuer, input.CredentialClass, now)
	if err != nil || claims.ActorAccountID != "" || claims.WorkspaceID != input.WorkspaceID {
		t.Fatalf("guest read/control context = %v", err)
	}
	input.Scopes = []string{"terminal:operate"}
	if _, err := provider.SignCredential(input); err == nil {
		t.Fatal("anonymous guest received launch authority")
	}
}
