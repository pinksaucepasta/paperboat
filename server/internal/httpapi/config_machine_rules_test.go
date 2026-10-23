package httpapi

import (
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"testing"
)

func TestConfigAssignmentResponseExplicitRules(t *testing.T) {
	item := dbsqlc.ControlConfigAssignment{PathRules: []byte(`[{"id":"editor","source":"machine", "repository_path":"editor/settings.json","local_path":"C:\\Users\\owner\\settings.json","kind":"file","include":[],"exclude":[]}]`), RepositoryBindings: []byte(`[{"repository_id":"repo","url":"ssh://git@lan/config.git"}]`)}
	response := assignmentResponse(item)
	if len(response.PathRules) != 1 || response.PathRules[0].LocalPath != `C:\Users\owner\settings.json` || len(response.RepositoryBindings) != 1 {
		t.Fatalf("explicit rules missing: %+v", response)
	}
	encoded, err := json.Marshal(assignmentResponse(dbsqlc.ControlConfigAssignment{}))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if string(out["path_rules"]) != "[]" || string(out["repository_bindings"]) != "[]" {
		t.Fatalf("disabled assignment must return arrays: %s", encoded)
	}
}
