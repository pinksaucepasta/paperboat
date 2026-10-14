package tunnelenrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connectorrotation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type recoveryOrigins struct{}

func (recoveryOrigins) ProbeOrigin(context.Context, hoststate.TunnelConfigRoute) error { return nil }

func recoveryAssemblyConfig(t *testing.T) tunnelmanager.ProductionAssemblyConfig {
	t.Helper()
	now := time.Now().UTC()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := connectorprotocol.IdentityThumbprint(public)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := connectorprotocol.IdentityKeyID(public)
	if err != nil {
		t.Fatal(err)
	}
	identity := connector.DataCarrierIdentity{AccountID: "account_01", HostID: "host_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", SessionID: "session_01", ProcessGeneration: 2, Generation: 1}
	auth := connectorprotocol.AuthRequest{AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, HostID: identity.HostID, IdentityKeyID: keyID, IdentityKeyThumbprint: thumbprint, ProcessGeneration: 2, CredentialGeneration: 1, Nonce: "assembly-recovery-nonce-01", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	auth, err = connectorprotocol.SignAuthProof(auth, func(payload []byte) []byte { return ed25519.Sign(private, payload) })
	if err != nil {
		t.Fatal(err)
	}
	source, err := connector.NewDataCarrierSessionSource(identity, connector.DataCarrierPoolConfig{}, connector.DataCarrierDialer(func(context.Context, connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
		t.Error("state-open observation unexpectedly dialed a carrier")
		return connector.DataCarrierDialResult{}, errors.New("unexpected carrier dial")
	}))
	if err != nil {
		t.Fatal(err)
	}
	return tunnelmanager.ProductionAssemblyConfig{
		Production:       tunnelmanager.ProductionConfig{StateRoot: t.TempDir(), HostID: identity.HostID, Report: func(tunnelmanager.Observation) {}},
		StableEndpointID: activationRequestFixture().StableEndpointID, Clock: bootstrapClock{now: now}, SessionSource: source, Origins: recoveryOrigins{},
		InitialConnector: &hoststate.Connector{ID: identity.ConnectorID, TunnelID: identity.TunnelID, HostID: identity.HostID, Credential: hoststate.CredentialReference{Reference: "protected-file://paperboat/connectors/credential_01", Generation: 1}, RotationGeneration: 1},
		Control: connectorrotation.ControlSessionConfig{
			Hello:   connectorprotocol.Hello{Protocol: connectorprotocol.ProtocolName, MinVersion: connectorprotocol.ProtocolVersion, MaxVersion: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, HostID: identity.HostID, ProcessGeneration: 2, Capabilities: []string{connectorprotocol.CapabilitySnapshot, connectorprotocol.CapabilityDelta, connectorprotocol.CapabilityAck, connectorprotocol.CapabilityHeartbeat, connectorprotocol.CapabilityRenewal}, Auth: auth},
			Drainer: newAssemblyDrainer(identity.TunnelID, identity.ConnectorID), Clock: bootstrapClock{now: now},
			Renewal: connectorrotation.CredentialRenewalSourceFunc(func(context.Context, time.Time) (string, string, error) {
				return "renewal-nonce-01", "renewal-proof-01", nil
			}),
		},
	}
}

func TestAssemblyRecoveryIsObservedOnceAndFreshOpenIsQuiet(t *testing.T) {
	config := recoveryAssemblyConfig(t)
	local := diagnostics.NewMemoryRecorder()
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), local)
	assembly, err := openObservedProductionAssembly(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(local.Recent()) != 0 {
		t.Fatal("initial creation emitted a recovery")
	}
	backup := filepath.Join(config.Production.StateRoot, "tunnels", "state.backup.json")
	if err := os.WriteFile(backup, []byte("PRIVATE-RECOVERY-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	assembly, err = openObservedProductionAssembly(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	events := local.Recent()
	if len(events) != 1 {
		t.Fatalf("recovery event count=%d", len(events))
	}
	event := events[0]
	if event.Code != "recovered" || event.Severity != "warning" || event.SupportReference != reference || event.Fields["reason"] != "backup_corrupt_repaired" || event.Fields["state"] != "primary" || event.Fields["outcome"] != "success" {
		t.Fatalf("recovery observation=%+v", event)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), config.Production.StateRoot) || strings.Contains(string(raw), "PRIVATE-RECOVERY-CONTENT") || strings.Contains(string(raw), "preserved") {
		t.Fatal("private recovery data was recorded")
	}
	if _, err := openObservedProductionAssembly(ctx, config); !errors.Is(err, hoststate.ErrLocked) {
		t.Fatal("second open did not retain exclusive state ownership")
	}
	if len(local.Recent()) != 1 {
		t.Fatal("failed open emitted a successful recovery")
	}
	if err := assembly.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	assembly, err = openObservedProductionAssembly(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(local.Recent()) != 1 {
		t.Fatal("fresh healthy open repeated recovery")
	}
}

func TestAssemblyRecoveryKeepsUsableStateWhenDiagnosticsUnavailable(t *testing.T) {
	config := recoveryAssemblyConfig(t)
	assembly, err := openObservedProductionAssembly(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.Production.StateRoot, "tunnels", "state.backup.json"), []byte("PRIVATE-RECOVERY-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	var observed []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		observed = append(observed, fault)
	})
	defer restore()
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), &diagnostics.Recorder{})
	assembly, err = openObservedProductionAssembly(ctx, config)
	if err != nil {
		t.Fatal("unavailable diagnostics prevented usable state recovery")
	}
	defer func() {
		if err := assembly.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if len(observed) != 1 || observed[0].Stage != "diagnostic_storage" || observed[0].Code != "diagnostic_storage_unavailable" || observed[0].SupportReference != reference || observed[0].ErrorType == "" {
		t.Fatalf("diagnostic-storage observation=%+v", observed)
	}
}
