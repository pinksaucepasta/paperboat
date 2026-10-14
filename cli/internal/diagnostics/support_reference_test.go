package diagnostics

import (
	"testing"
	"time"
)

func TestEventSupportReferenceIsTypedAndValidated(t *testing.T) {
	event, err := NewEventWithSupportReference(time.Unix(1, 0).UTC(), "cli", "operation_failed", "error", "support_01234567-89ab-4def-8123-456789abcdef", nil)
	if err != nil || event.SupportReference == "" {
		t.Fatalf("event=%#v err=%v", event, err)
	}
	if _, err := NewEventWithSupportReference(time.Unix(1, 0).UTC(), "cli", "operation_failed", "error", "request-secret", nil); err == nil {
		t.Fatal("invalid support reference accepted")
	}
}
