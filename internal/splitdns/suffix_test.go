package splitdns

import "testing"

func TestValidateSuffixContract(t *testing.T) {
	if len(ianaTLDs) < 1400 {
		t.Fatalf("bundled IANA root zone is incomplete: %d entries", len(ianaTLDs))
	}
	if got, err := validateSuffix("pprbt"); err != nil || got != "pprbt" {
		t.Fatalf("valid suffix = %q, %v", got, err)
	}
	for _, value := range []string{"a", "abcdefghijklmnopq", "Upper", "with-hyphen", "local", "com", "in", "aaa"} {
		if _, err := validateSuffix(value); err == nil {
			t.Errorf("invalid suffix %q accepted", value)
		}
	}
}
