package machineguard

import "testing"

func TestGuardUnitsAcceptTheCandidatesOwnSetAndRefuseUnusableOnes(t *testing.T) {
	good := map[string]string{"a.service": "x", "b.service": "y"}
	if err := checkGuardUnits(good, "a.service"); err != nil {
		t.Fatalf("a set holding the required unit was refused: %v", err)
	}
	// A later build may add a unit file: the installing (older) build still installs the set.
	if err := checkGuardUnits(map[string]string{"a.service": "x", "new.service": "z"}, "a.service"); err != nil {
		t.Fatalf("a larger candidate set was refused: %v", err)
	}
	for name, units := range map[string]map[string]string{
		"missing required": {"b.service": "y"},
		"empty unit":       {"a.service": ""},
		"path in name":     {"a.service": "x", "../etc/evil": "y"},
		"dot name":         {"a.service": "x", ".": "y"},
	} {
		if err := checkGuardUnits(units, "a.service"); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
