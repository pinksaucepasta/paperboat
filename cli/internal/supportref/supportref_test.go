package supportref

import (
	"context"
	"testing"
)

func TestReferenceAndContext(t *testing.T) {
	value := New()
	if !Valid(value) {
		t.Fatalf("New() = %q", value)
	}
	if got := FromContext(WithContext(context.Background(), value)); got != value {
		t.Fatalf("FromContext() = %q, want %q", got, value)
	}
	for _, invalid := range []string{"", "PB-0123456789abcdef0123456789abcdef", "pb-0123", "pb-0123456789abcdef0123456789abcdeg"} {
		if Valid(invalid) {
			t.Errorf("Valid(%q) = true", invalid)
		}
	}
}
