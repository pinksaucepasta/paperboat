package machineguard

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestPortConflictResponseRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		code, message string
		conflict      bool
	}{
		{"port_in_use", "bind failed", true},
		{"", "protected port is already in use", false},
		{"other", "failure", false},
	} {
		body, err := json.Marshal(response{ErrorCode: tc.code, Error: tc.message})
		if err != nil {
			t.Fatal(err)
		}
		var decoded response
		if err = json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if got := responseError(decoded); got == nil || errors.Is(got, ErrPortInUse) != tc.conflict {
			t.Fatalf("code %q: %v", tc.code, got)
		}
	}
	if err := responseError(response{}); err != nil {
		t.Fatal(err)
	}
}
