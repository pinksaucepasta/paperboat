package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/admission"
)

func tokenFor(t testing.TB, private ed25519.PrivateKey, keyID string, mutate func(map[string]any)) string {
	t.Helper()
	now := time.Unix(1000, 0)
	header := map[string]any{"alg": "EdDSA", "kid": keyID, "typ": "paperboat-credential+jwt"}
	claims := map[string]any{"iss": "https://api.paperboat.test", "aud": "paperboat-edge", "sub": "machine", "jti": "jti_admit_01", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "scope": []string{"connector:admit"}, "credential_class": "connector_admission", "environment_id": "env", "machine_id": "machine", "installation_generation": 1, "connector_id": "runtime", "connector_generation": 3, "edge_pool": "default", "edge_node_id": "edge", "route_binding": base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "file_transfer_policy": map[string]any{"revision": "file-transfer-v1", "max_file_bytes": 50 << 20, "max_batch_files": 10, "max_batch_bytes": 500 << 20, "max_concurrent_transfers": 2, "retention_seconds": 604800, "delivery_timeout_seconds": 600, "max_pending_spool_bytes": 1 << 30}}
	if mutate != nil {
		mutate(claims)
	}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(signing)))
}

func TestVerifierAcceptsExactConnectorCredential(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }, ClockSkew: time.Minute}
	claims, err := verifier.Verify(context.Background(), tokenFor(t, private, "key-1", nil))
	if err != nil || claims.JTI != "jti_admit_01" || claims.ConnectorGeneration != 3 || claims.EdgeNodeID != "edge" {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
}

func TestVerifierRejectsDuplicateSignedHeaderAndClaims(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }, ClockSkew: time.Minute}
	canonical := tokenFor(t, private, "key-1", nil)
	parts := strings.Split(canonical, ".")

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	duplicateHeader := strings.Replace(string(header), `"kid":"key-1"`, `"kid":"key-1","kid":"key-1"`, 1)
	if duplicateHeader == string(header) {
		t.Fatal("header fixture did not mutate")
	}
	if _, err := verifier.Verify(context.Background(), resignToken(duplicateHeader, string(mustDecodeSegment(t, parts[1])), private)); err == nil {
		t.Fatal("duplicate signed header accepted")
	}

	payload := string(mustDecodeSegment(t, parts[1]))
	duplicateClaims := strings.Replace(payload, `"jti":"jti_admit_01"`, `"jti":"jti_admit_01","jti":"jti_admit_01"`, 1)
	if duplicateClaims == payload {
		t.Fatal("claims fixture did not mutate")
	}
	if _, err := verifier.Verify(context.Background(), resignToken(string(header), duplicateClaims, private)); err == nil {
		t.Fatal("duplicate signed claims accepted")
	}

	if _, err := verifier.Verify(context.Background(), parts[0]+"=."+parts[1]+"."+parts[2]); err == nil {
		t.Fatal("non-canonical base64url header accepted")
	}
}

func resignToken(header, payload string, private ed25519.PrivateKey) string {
	headerPart := base64.RawURLEncoding.EncodeToString([]byte(header))
	payloadPart := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signing := headerPart + "." + payloadPart
	return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(signing)))
}

func mustDecodeSegment(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestVerifierAcceptsExactHelperAccessCredential(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }}
	token := tokenFor(t, private, "key-1", func(claims map[string]any) {
		claims["aud"] = "paperboat-machine"
		claims["credential_class"] = "terminal_operation"
		claims["scope"] = []string{"terminal:operate"}
		claims["machine_id"] = "machine_1"
		claims["source_machine_id"] = "machine_source"
		claims["user_id"] = "usr_1"
		claims["cli_client_session_id"] = "acs_1"
		claims["session_id"] = "pts_1"
		delete(claims, "connector_id")
		delete(claims, "connector_generation")
		delete(claims, "edge_pool")
		delete(claims, "edge_node_id")
		delete(claims, "file_transfer_policy")
	})
	claims, err := verifier.VerifyHelperAccess(context.Background(), token)
	if err != nil || claims.JTI != "jti_admit_01" || claims.EnvironmentID != "env" || claims.MachineID != "machine_1" || claims.CredentialClass != "terminal_operation" {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	if _, err := verifier.VerifyHelperAccess(context.Background(), tokenFor(t, private, "key-1", nil)); err == nil {
		t.Fatal("connector credential accepted as helper access")
	}
	fileToken := tokenFor(t, private, "key-1", func(claims map[string]any) {
		claims["aud"] = "paperboat-machine"
		claims["credential_class"] = "file_transfer"
		claims["scope"] = []string{"file:transfer"}
		claims["machine_id"] = "machine_1"
		claims["source_machine_id"] = "machine_source"
		claims["user_id"] = "usr_1"
		claims["cli_client_session_id"] = "acs_1"
		delete(claims, "session_id")
		delete(claims, "connector_id")
		delete(claims, "connector_generation")
		delete(claims, "edge_pool")
		delete(claims, "edge_node_id")
		delete(claims, "file_transfer_policy")
	})
	claims, err = verifier.VerifyHelperAccess(context.Background(), fileToken)
	if err != nil || claims.CredentialClass != "file_transfer" || len(claims.Scopes) != 1 || claims.Scopes[0] != "file:transfer" {
		t.Fatalf("file claims=%+v err=%v", claims, err)
	}
	missingTerminalSession := tokenFor(t, private, "key-1", func(claims map[string]any) {
		claims["aud"] = "paperboat-machine"
		claims["credential_class"] = "terminal_operation"
		claims["scope"] = []string{"terminal:operate"}
		claims["machine_id"] = "machine_1"
		claims["user_id"] = "usr_1"
		claims["cli_client_session_id"] = "acs_1"
		delete(claims, "connector_id")
		delete(claims, "connector_generation")
		delete(claims, "edge_pool")
		delete(claims, "edge_node_id")
		delete(claims, "file_transfer_policy")
	})
	if _, err := verifier.VerifyHelperAccess(context.Background(), missingTerminalSession); err == nil {
		t.Fatal("terminal credential without session accepted")
	}
	wrongScope := tokenFor(t, private, "key-1", func(claims map[string]any) {
		claims["aud"] = "paperboat-machine"
		claims["credential_class"] = "file_transfer"
		claims["scope"] = []string{"file:stage"}
		claims["machine_id"] = "machine_1"
		claims["user_id"] = "usr_1"
		claims["cli_client_session_id"] = "acs_1"
		claims["session_id"] = "pts_1"
		delete(claims, "connector_id")
		delete(claims, "connector_generation")
		delete(claims, "edge_pool")
		delete(claims, "edge_node_id")
		delete(claims, "file_transfer_policy")
	})
	if _, err := verifier.VerifyHelperAccess(context.Background(), wrongScope); err == nil {
		t.Fatal("retired file:stage scope accepted")
	}
}

func TestVerifierAcceptsBoundCodexCredentials(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }}
	for class, scopes := range map[string][]string{"codex_connect": {"codex:connect"}, "codex_manage": {"codex:prepare", "codex:browse", "codex:renew", "codex:stop"}} {
		token := tokenFor(t, private, "key-1", func(claims map[string]any) {
			claims["aud"] = "paperboat-machine"
			claims["credential_class"] = class
			claims["scope"] = scopes
			claims["machine_id"] = "machine_1"
			claims["user_id"] = "usr_1"
			claims["cli_client_session_id"] = "cls_1"
			claims["session_id"] = "cdx_1"
			delete(claims, "file_transfer_policy")
		})
		if claims, err := verifier.VerifyHelperAccess(context.Background(), token); err != nil || claims.CredentialClass != class {
			t.Fatalf("%s claims=%+v err=%v", class, claims, err)
		}
	}
	missingSession := tokenFor(t, private, "key-1", func(claims map[string]any) {
		claims["aud"] = "paperboat-machine"
		claims["credential_class"] = "codex_connect"
		claims["scope"] = []string{"codex:connect"}
		claims["machine_id"] = "machine_1"
		claims["user_id"] = "usr_1"
		claims["cli_client_session_id"] = "cls_1"
		delete(claims, "session_id")
		delete(claims, "file_transfer_policy")
	})
	if _, err := verifier.VerifyHelperAccess(context.Background(), missingSession); err == nil {
		t.Fatal("Codex credential without session accepted")
	}
}

func TestVerifierAcceptsPreviewLaunchWithoutCLIClientSession(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }}
	token := tokenFor(t, private, "key-1", func(claims map[string]any) {
		claims["aud"] = "paperboat-machine"
		claims["sub"] = "usr_1"
		claims["credential_class"] = "preview_launch"
		claims["scope"] = []string{"preview:launch"}
		claims["account_id"] = "usr_1"
		claims["machine_id"] = "machine_1"
		claims["user_id"] = "usr_1"
		claims["actor_id"] = "usr_1"
		claims["preview_id"] = "prv_1"
		claims["owner_session_id"] = "owner_local_1"
		claims["operation_id"] = "op_1"
		claims["target_scheme"] = "http"
		claims["target_address"] = "127.0.0.1:43871"
		claims["access_mode"] = "public"
		claims["endpoint"] = "https://preview-1.preview.pprbt.dev"
		claims["lease_deadline"] = int64(1100)
		claims["lease_etag"] = `"ptv1:preview_lease:cHJ2XzE:1"`
		claims["state"] = "connecting"
		claims["allocation_state"] = "pending"
		claims["edge_state"] = "pending"
		claims["origin_state"] = "unknown"
		claims["created_at"] = int64(1000)
		claims["last_renewed_at"] = int64(1000)
		claims["expected_generation"] = int64(1)
		claims["request_hash"] = "sha256:test"
		claims["idempotency_key"] = "idem_1"
		claims["request_id"] = "req_1"
		claims["correlation_id"] = "cor_1"
		delete(claims, "cli_client_session_id")
		delete(claims, "session_id")
		delete(claims, "connector_id")
		delete(claims, "connector_generation")
		delete(claims, "edge_pool")
		delete(claims, "edge_node_id")
		delete(claims, "file_transfer_policy")
	})
	claims, err := verifier.VerifyHelperAccess(context.Background(), token)
	if err != nil || claims.CredentialClass != "preview_launch" || claims.MachineID != "machine_1" {
		t.Fatalf("preview claims=%+v err=%v", claims, err)
	}
}

func TestVerifierRejectsMalformedWrongKeySignatureAndClaims(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPrivate, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }, ClockSkew: time.Minute}
	tokens := []string{"not-a-token", tokenFor(t, private, "unknown", nil), tokenFor(t, otherPrivate, "key-1", nil), tokenFor(t, private, "key-1", func(claims map[string]any) { claims["aud"] = "other" }), tokenFor(t, private, "key-1", func(claims map[string]any) { claims["exp"] = int64(999) }), tokenFor(t, private, "key-1", func(claims map[string]any) { claims["unknown"] = true })}
	for _, token := range tokens {
		if _, err := verifier.Verify(context.Background(), token); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
}

func TestVerifierRejectsMissingOrInvalidFileTransferPolicy(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }, ClockSkew: time.Minute}
	for _, mutate := range []func(map[string]any){
		func(claims map[string]any) { delete(claims, "file_transfer_policy") },
		func(claims map[string]any) { claims["file_transfer_policy"].(map[string]any)["max_file_bytes"] = 0 },
	} {
		if _, err := verifier.Verify(context.Background(), tokenFor(t, private, "key-1", mutate)); err == nil {
			t.Fatal("invalid file transfer policy accepted")
		}
	}
}

type revokedSource struct{}

func (revokedSource) Revoked(context.Context, admission.Claims) (bool, error) { return true, nil }

func TestVerifierPropagatesRevocationAndFailsClosedOnSourceError(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Revocations: revokedSource{}, Now: func() time.Time { return time.Unix(1000, 0) }}
	claims, err := verifier.Verify(context.Background(), tokenFor(t, private, "key-1", nil))
	if err != nil || !claims.Revoked {
		t.Fatalf("revocation = %+v, %v", claims, err)
	}
	verifier.Keys = failingKeys{}
	if _, err := verifier.Verify(context.Background(), tokenFor(t, private, "key-1", nil)); err == nil {
		t.Fatal("key source failure accepted")
	}
}

type failingKeys struct{}

func (failingKeys) Key(context.Context, string) (ed25519.PublicKey, error) {
	return nil, errors.New("unavailable")
}

func TestVerifierBrowserTerminalCredentialBindings(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	verifier := &Verifier{Issuer: "https://api.paperboat.test", Keys: StaticKeys{"key-1": public}, Now: func() time.Time { return time.Unix(1000, 0) }}
	for _, scenario := range []string{"valid", "view", "interactive", "missing_attachment", "missing_session", "missing_generation", "bad_browser_key", "cli_session", "wrong_scope"} {
		t.Run(scenario, func(t *testing.T) {
			token := tokenFor(t, private, "key-1", func(c map[string]any) {
				c["aud"] = "paperboat-machine"
				c["credential_class"] = "browser_terminal_operation"
				c["scope"] = []string{"terminal:operate"}
				c["machine_id"] = "machine_1"
				c["user_id"] = "usr_1"
				c["sub"] = "usr_1"
				c["account_id"] = "usr_1"
				c["session_id"] = "terminal_1"
				c["browser_attachment_id"] = "attachment_1"
				c["browser_public_key_sha256"] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				c["expected_generation"] = 1
				c["policy_generation"] = 1
				delete(c, "cli_client_session_id")
				delete(c, "source_machine_id")
				switch scenario {
				case "view":
					c["scope"] = []string{"terminal:view"}
				case "interactive":
					c["scope"] = []string{"terminal:control"}
				case "missing_attachment":
					delete(c, "browser_attachment_id")
				case "missing_session":
					delete(c, "session_id")
				case "missing_generation":
					delete(c, "policy_generation")
				case "bad_browser_key":
					c["browser_public_key_sha256"] = "invalid"
				case "cli_session":
					c["cli_client_session_id"] = "cli_1"
				case "wrong_scope":
					c["scope"] = []string{"file:transfer"}
				}
			})
			_, err := verifier.VerifyHelperAccess(context.Background(), token)
			if scenario == "valid" || scenario == "view" || scenario == "interactive" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid browser credential accepted")
			}
		})
	}
}
