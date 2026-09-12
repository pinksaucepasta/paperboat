package connectorprotocol

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"os"
	"path/filepath"
	"testing"
)

// The server producer runs real shared-account SQL mutations and exports the
// canonical desired snapshots. Consume them with the endpoint's durable config
// applier so cross-account audit provenance cannot change endpoint ownership.
func TestServerIssuedSharedTunnelManagementConfig(t *testing.T) {
	path := os.Getenv("PAPERBOAT_SHARED_TUNNEL_FIXTURE")
	if path == "" {
		t.Skip("requires TestSharedTunnelManagementOnPostgres fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if !t.Failed() {
			_ = os.Remove(path)
		}
	}()
	var f struct {
		Owner, Actor, Machine, Tunnel, Endpoint string
		Before, After                           json.RawMessage
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Owner == "" || f.Actor == f.Owner || f.Machine == "" {
		t.Fatal("fixture must contain a different management actor")
	}
	store, _, err := hoststate.Open(hoststate.Config{Root: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	applier := &HostStateApplier{Store: store, StableEndpointID: f.Endpoint}
	for i, payload := range []json.RawMessage{f.Before, f.After} {
		var meta struct {
			Generation   uint64
			DesiredState string `json:"desired_state"`
		}
		if err = json.Unmarshal(payload, &meta); err != nil {
			t.Fatal(err)
		}
		snapshot, err := NewSnapshot(f.Tunnel, meta.Generation, payload)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.AccountID = f.Owner
		snapshot.ConnectorID = "shared_connector"
		snapshot.SessionID = "shared_session"
		snapshot.ProcessGeneration = 1
		prepared, err := applier.PrepareSnapshot(context.Background(), snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err = prepared.(*hostStatePrepared).EnsureConnector(hoststate.Connector{ID: snapshot.ConnectorID, TunnelID: f.Tunnel, HostID: f.Machine, Credential: hoststate.CredentialReference{Reference: "keychain://paperboat/connectors/shared-test", Generation: 1}, RotationGeneration: 1}); err != nil {
				t.Fatal(err)
			}
		}
		if err = prepared.Activate(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, _, err := store.Snapshot()
		if err != nil || len(state.Tunnels) != 1 {
			t.Fatal("configuration was not persisted")
		}
		got := state.Tunnels[0]
		if got.ID != f.Tunnel || got.AppliedGeneration != meta.Generation || got.DesiredState != meta.DesiredState {
			t.Fatalf("applied state mismatch at step %d", i)
		}
		if i == 1 && got.DesiredState != "paused" {
			t.Fatal("shared pause was not consumed")
		}
	}
}
