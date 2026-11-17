package vaultprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// The team-rotate document used to say confirm_total_loss with omitempty on the server and without it in the
// client; the one shared tag always carries it, so a false is present on the wire.
func TestTeamRotateAlwaysCarriesConfirmTotalLoss(t *testing.T) {
	raw, err := json.Marshal(VaultTeamRotate{})
	if err != nil || !strings.Contains(string(raw), `"confirm_total_loss":false`) {
		t.Fatalf("document = %s, %v", raw, err)
	}
}

func TestObservationCoordinate(t *testing.T) {
	o := VaultLayerObservation{WorkspaceID: "w", OwnerKind: "team", OwnerID: "o", SourceMachineID: "m"}
	if o.Coordinate() != (VaultLayerCoordinate{WorkspaceID: "w", OwnerKind: "team", OwnerID: "o", MachineID: "m"}) {
		t.Fatalf("coordinate = %#v", o.Coordinate())
	}
}
