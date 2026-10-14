package tunnelmanager

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"testing"
	"time"
)

func TestConnectorStatusRejectsHistoricalTransportAfterDrainOrClose(t *testing.T) {
	for _, state := range []string{"disconnected", "draining", "closed"} {
		t.Run(state, func(t *testing.T) {
			identity := connector.DataCarrierIdentity{AccountID: "account_01", HostID: "host_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", SessionID: "session_01", ProcessGeneration: 2, Generation: 3}
			var edge *connector.DataCarrier
			carrier, cleanup := newConnectedReconnectCarrier(t, identity, &edge)
			t.Cleanup(cleanup)
			active := &reconnectCarrierActive{base: &fakeActive{tunnelID: identity.TunnelID, connectorID: identity.ConnectorID, generation: identity.Generation}, carrier: carrier}
			assembly := &ProductionAssembly{Manager: &ProductionManager{Manager: &Manager{active: map[string]Active{identity.TunnelID: active}}}}
			if status := assembly.ConnectorStatus(); !status.Connected || status.Generation != 3 {
				t.Fatalf("established carrier status: %+v", status)
			}
			if state == "disconnected" {
				if err := edge.Close(); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Second)
				for assembly.ConnectorStatus().Connected && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			} else if state == "draining" {
				if err := carrier.BeginDrain(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := carrier.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if status := assembly.ConnectorStatus(); status.Connected || status.Generation != 0 || status.Transport != "" {
				t.Fatalf("unusable carrier reported connected: %+v", status)
			}
		})
	}
}
