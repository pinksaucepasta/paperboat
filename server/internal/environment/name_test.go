package environment

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// The server checks the plaintext names of the manifest path with the rule of shared/envname, whose list the
// CLI, the browser engine and the console are tested against too (128 characters is the last allowed length).
func TestValidateNameAgreesWithTheSharedNameList(t *testing.T) {
	data, err := os.ReadFile("../../../shared/envname/testdata/names.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string `json:"name"`
		Valid bool   `json:"valid"`
	}
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) < 20 {
		t.Fatalf("names.json: %v (%d cases)", err, len(cases))
	}
	for _, c := range cases {
		if err := ValidateName(c.Name); (err == nil) != c.Valid || err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidateName(%q) = %v, list says valid=%v", c.Name, err, c.Valid)
		}
	}
}
