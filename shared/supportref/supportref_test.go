package supportref

import (
	"context"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestReferenceAndContext(t *testing.T) {
	value := New()
	id, err := uuid.Parse(strings.TrimPrefix(value, "support_"))
	if !Valid(value) || err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || "support_"+id.String() != value {
		t.Fatalf("New() = %q", value)
	}
	if got := FromContext(WithContext(context.Background(), value)); got != value {
		t.Fatalf("FromContext() = %q, want %q", got, value)
	}
	for _, invalid := range []string{"", "support_01234567-89ab-1cde-8fab-0123456789ab", "support_01234567-89ab-4cde-0fab-0123456789ab", strings.ToUpper(value)} {
		if Valid(invalid) {
			t.Errorf("Valid(%q) = true", invalid)
		}
	}
}
