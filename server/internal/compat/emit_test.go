package compat

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/peersessions"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// TestEmitCompatArtifacts writes, to $COMPAT_ARTIFACTS_DIR, the artifacts every client receives
// whatever its version. Artifacts gated on a client capability (the transfer_mode claims, sent only
// to destinations that observe file_transfer_modes) are deliberately not here.
func TestEmitCompatArtifacts(t *testing.T) {
	dir := os.Getenv("COMPAT_ARTIFACTS_DIR")
	if dir == "" {
		t.Skip("set COMPAT_ARTIFACTS_DIR to emit compat artifacts for tools/compat")
	}
	seed := sha256.Sum256([]byte("paperboat-compat-key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	provider, err := mint.New([]mint.Key{{ID: "compat-key", PrivateKey: key}}, "compat-key", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	write := func(name string, value any) {
		t.Helper()
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	payload := func(token string) json.RawMessage {
		t.Helper()
		parts := strings.Split(token, ".")
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil || len(parts) != 3 {
			t.Fatalf("token shape: %v", err)
		}
		return raw
	}
	policy := config.Default().Access.FileTransfer.Published()
	write("file_transfer_policy.json", policy)

	sign := func(input mint.CredentialInput) string {
		t.Helper()
		token, err := provider.SignCredential(input)
		if err != nil {
			t.Fatalf("sign %s: %v", input.CredentialClass, err)
		}
		return token
	}
	base := mint.CredentialInput{Issuer: "https://api.compat.example.test", Subject: "usr_compat", JTI: "jti_compat", IssuedAt: now, ExpiresAt: now.Add(4 * time.Minute), EnvironmentID: "env_compat", MachineID: "machine_compat", UserID: "usr_compat"}
	transfer := base
	transfer.Audience, transfer.CredentialClass, transfer.Scopes = "paperboat-machine", "file_transfer", []string{"file:transfer"}
	transfer.SourceMachineID, transfer.CLIClientSessionID = "machine_source", "cli_compat"
	terminal := base
	terminal.Audience, terminal.CredentialClass, terminal.Scopes = "paperboat-machine", "terminal_operation", []string{"terminal:operate"}
	terminal.WorkspaceID, terminal.ActorAccountID, terminal.AccountID, terminal.CLIClientSessionID, terminal.SessionID, terminal.AssignmentID, terminal.ExpectedGeneration = "personal", "usr_compat", "usr_compat", "cli_compat", "session_compat", "access_compat", 1
	binding := sha256.Sum256([]byte("route-binding"))
	connector := base
	connector.Audience, connector.CredentialClass, connector.Scopes = "paperboat-edge", "connector_admission", []string{"connector:admit"}
	connector.InstallationGeneration, connector.ConnectorID, connector.ConnectorGeneration, connector.EdgePool, connector.EdgeNodeID = 1, "connector_compat", 1, "default", "node_compat"
	connector.RouteBinding, connector.FileTransferPolicy = base64.RawURLEncoding.EncodeToString(binding[:]), &policy
	write("credential_claims.json", map[string]json.RawMessage{
		"file_transfer":       payload(sign(transfer)),
		"terminal_operation":  payload(sign(terminal)),
		"connector_admission": payload(sign(connector)),
	})

	endpoint := "https://runtime.compat.example.test/v1/file-transfers"
	write("file_transfer_descriptor.json", usermachines.FileTransferDescriptor{
		Endpoint: endpoint, SourceMachineID: "machine_source", DestinationMachineID: "machine_compat", InitiatingUserID: "usr_compat",
		Policy: accessdescriptor.FileTransferPolicy(policy),
		Auth:   map[string]any{"method": "bearer", "token": sign(transfer), "expires_at": now.Add(4 * time.Minute), "scopes": []string{"file:transfer"}, "access_session_id": "access_compat"},
	})

	pin := sha256.Sum256([]byte("relay-pin"))
	node := peersessions.RegionalNode{NodeID: "node_compat", NodeGeneration: 1, ProcessEpoch: "epoch_compat", Region: "eu-central", FailureDomain: "fd-1", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.compat.example.test", EndpointTCPPort: 443, EndpointQUICPort: 443, State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix(), TLSPin: pin[:]}
	nodeClaims, err := node.Claims()
	if err != nil {
		t.Fatal(err)
	}
	own := node
	own.NodeID, own.SelfHosted = "node_own_compat", true
	ownClaims, err := own.Claims()
	if err != nil {
		t.Fatal(err)
	}
	write("regional_candidates.json", peersessions.RegionalCandidateSet("https://api.compat.example.test", "usr_compat", "endpoint_compat", 1, now, now.Add(time.Minute), []map[string]any{nodeClaims, ownClaims}))
}
