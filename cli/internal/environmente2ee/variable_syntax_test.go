package environmente2ee

import (
	"regexp"
	"testing"
)

func TestVariableSyntaxMatchesTheDocumentedPattern(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for _, value := range []string{"", "A", "a", "_", "_1", "1", "1A", "A1", "A-B", "A B", "A.B", "É", "A\n", "TOKEN_2"} {
		if got, want := validVariableSyntax(value), pattern.MatchString(value); got != want {
			t.Errorf("validVariableSyntax(%q) = %v, regexp says %v", value, got, want)
		}
	}
}
