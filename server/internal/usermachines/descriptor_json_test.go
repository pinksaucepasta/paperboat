package usermachines

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
)

// The CLI binds a shared-terminal join to the machine's generation and refuses a descriptor without one;
// the canonical wire form must therefore carry it (it was dropped when the descriptor was projected).
func TestConnectionDescriptorWireFormCarriesTheMachineGeneration(t *testing.T) {
	descriptor := ConnectionDescriptor{Schema: terminalprotocol.DescriptorSchemaV1, Issuer: "https://api.example.test", MachineGeneration: 3, Connectable: true, ExpiresAt: time.Unix(1, 0).UTC(),
		Environment: terminalprotocol.Environment{ID: "m", Kind: "machine", ResourceID: "m", Alias: "a", State: "ready"}, Status: "ready", Reason: "ready"}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["machine_generation"] != float64(3) {
		t.Fatalf("machine_generation missing from the wire form: %s", raw)
	}
}
