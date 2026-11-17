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

func TestRuntimeDescriptorWire(t *testing.T) {
	raw, err := json.Marshal(RuntimeDescriptor{AssignmentVersion: 1, Mode: ModePullOnly, Policy: RuntimePolicy{Format: "f", AbsoluteRuntimeExclusionRoots: []string{"/never/on/the/wire"}}})
	want := `{"assignment_version":1,"configuration_revision":"","path_rules":null,"repository_bindings":null,"write_mode":"","mode":"pull_only","repository_id":"","automatic_updates":false,"assignment_id":"","environment_id":"","machine_id":"","machine_alias":"","installation_generation":0,"sync_revision_floor":0,"warning_revision":"","policy":{"format":"f","revision":"","manifest_contract":"","manifest_max_bytes":0,"manifest_max_lines":0,"manifest_max_pattern_bytes":0,"max_file_bytes":0,"max_batch_bytes":0,"debounce":0,"minimum_push_interval":0,"maximum_dirty_delay":0,"remote_poll_interval":0,"retry_limit":0,"shutdown_flush_timeout":0,"summary_limit":0}}`
	if err != nil || string(raw) != want {
		t.Fatalf("descriptor = %s, %v", raw, err)
	}
	if !ModePushOnly.Valid() || AssignmentMode("x").Valid() {
		t.Fatal("assignment mode validity")
	}
}
