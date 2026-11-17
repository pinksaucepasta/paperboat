package inspector

import (
	"encoding/json"
	"testing"
)

// The edge used to make three carrier members optional; the server always sends them, so the one definition
// carries them always.
func TestCarrierAlwaysCarriesLeaseAttachmentAndOwner(t *testing.T) {
	raw, err := json.Marshal(Carrier{})
	want := `{"lease_generation":0,"attachment_generation":0,"owner_machine_id":"","machine_id":"","route_generation":0,"account_id":"","tunnel_id":"","connector_id":"","session_id":"","process_generation":0,"generation":0,"route_id":"","edge_node_id":"","edge_process_epoch":"","assignment_id":"","assignment_generation":0,"config_content_hash":""}`
	if err != nil || string(raw) != want {
		t.Fatalf("carrier = %s, %v", raw, err)
	}
}
