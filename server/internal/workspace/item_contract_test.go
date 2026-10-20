package workspace

import (
	"encoding/json"
	"testing"
)

// Released CLIs decode GET /v1/workspaces strictly: a field they do not know makes every daemon's
// inventory refresh fail ("Local machine access is unavailable"). Adding a field here means the
// client accepts it first and is released, so this list changes only with that order.
func TestWorkspaceItemCarriesOnlyFieldsReleasedClientsDecode(t *testing.T) {
	raw, err := json.Marshal(Item{ID: "t", Name: "t", Kind: "team", Role: "owner", ApproveNewMachines: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"id": true, "name": true, "kind": true, "role": true, "approve_new_machines": true}
	for name := range fields {
		if !allowed[name] {
			t.Errorf("workspace item field %q is unknown to released clients; ship the client first", name)
		}
	}
}
