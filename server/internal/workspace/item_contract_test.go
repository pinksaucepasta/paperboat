package workspace

import (
	"encoding/json"
	"testing"
)

// The fields of GET /v1/workspaces: this list is the answer's contract (server/docs/openapi.json Workspace) and
// changes with it. Clients (shared/client/api, the console) ignore fields they do not know.
func TestWorkspaceItemCarriesOnlyDocumentedFields(t *testing.T) {
	signedIn := false
	raw, err := json.Marshal(Item{ID: "t", Name: "t", Kind: "team", Role: "owner", ApproveNewMachines: true, SSORequired: true, SSOSignedIn: &signedIn})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"id": true, "name": true, "kind": true, "role": true, "approve_new_machines": true, "sso_required": true, "sso_signed_in": true}
	for name := range fields {
		if !allowed[name] {
			t.Errorf("workspace item field %q is not documented in openapi.json Workspace", name)
		}
	}
}
