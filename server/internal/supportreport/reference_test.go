package supportreport

import (
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestSupportReferenceFormat(t *testing.T) {
	value := NewReference()
	parsed, err := uuid.Parse(strings.TrimPrefix(value, "support_"))
	if !strings.HasPrefix(value, "support_") || err != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || !ValidReference(value) {
		t.Fatalf("invalid generated reference %q", value)
	}
	for _, value := range []string{"support_123e4567-e89b-12d3-a456-426614174000", "support_123E4567-e89b-42d3-a456-426614174000", "support_123e4567-e89b-42d3-a456-426614174000/other"} {
		if ValidReference(value) {
			t.Fatalf("invalid reference admitted %q", value)
		}
	}
}
