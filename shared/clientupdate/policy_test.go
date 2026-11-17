package clientupdate

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPolicyWire(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(Policy{Schema: Schema, Revision: 3, MinimumVersion: "2030.01.01.1", Reason: "fix", EnforceAt: &at})
	want := `{"schema":"paperboat.client-update-policy/v1","revision":3,"minimum_version":"2030.01.01.1","reason":"fix","enforce_at":"2030-01-01T00:00:00Z"}`
	if err != nil || string(raw) != want {
		t.Fatalf("policy = %s, %v", raw, err)
	}
}
