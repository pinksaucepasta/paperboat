package envname

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The console's TypeScript rule reads the same file (console/lib/envvault/names.test.ts).
func TestRuleMatchesTheSharedNameList(t *testing.T) {
	data, err := os.ReadFile("testdata/names.json")
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
		if got := Valid(c.Name); got != c.Valid {
			t.Errorf("Valid(%q) = %v, list says %v (%v)", c.Name, got, c.Valid, Check(c.Name))
		}
	}
	if Valid(strings.Repeat("A", MaxBytes+1)) || !Valid(strings.Repeat("A", MaxBytes)) {
		t.Fatal("the cap is not exactly MaxBytes")
	}
}
