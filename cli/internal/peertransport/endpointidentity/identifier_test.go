package endpointidentity

import (
	"regexp"
	"strings"
	"testing"
)

// The scan replaced a regexp; both must agree on every shape that matters.
func TestValidIdentifierMatchesTheDocumentedPattern(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	cases := []string{"", "a", "A", "0", "_", "-", "a_", "a-", "_a", "-a", "a b", "a.b", "é", "a\n", "a\x00", "ab_C-9",
		strings.Repeat("a", 128), strings.Repeat("a", 129), "0" + strings.Repeat("-", 127), "0" + strings.Repeat("-", 128)}
	for _, value := range cases {
		if got, want := ValidIdentifier(value), pattern.MatchString(value); got != want {
			t.Errorf("ValidIdentifier(%q) = %v, regexp says %v", value, got, want)
		}
	}
}
