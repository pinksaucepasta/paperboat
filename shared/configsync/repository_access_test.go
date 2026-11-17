package configsync

import (
	"encoding/json"
	"testing"
	"time"
)

// Stored encrypted and read back strictly: the field set and order are the stored shape.
func TestRepositoryAccessWire(t *testing.T) {
	raw, err := json.Marshal(RepositoryAccess{RepositoryID: "r", AssignmentID: "a", EnvironmentID: "e", MachineID: "m", CloneURL: "c", PublishURL: "p", Branch: "b", Transport: "t", Username: "u", Password: "w", Capability: "k", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)})
	want := `{"repository_id":"r","assignment_id":"a","environment_id":"e","machine_id":"m","clone_url":"c","publish_url":"p","branch":"b","transport":"t","username":"u","password":"w","capability":"k","expires_at":"2030-01-01T00:00:00Z"}`
	if err != nil || string(raw) != want {
		t.Fatalf("access = %s, %v", raw, err)
	}
}
