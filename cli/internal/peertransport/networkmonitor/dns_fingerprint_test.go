package networkmonitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateDNSContextPreservesCancellationCause(t *testing.T) {
	cause := errors.New("caller cancellation cause")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	err := validateDNSContext(ctx)
	if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("DNS context cause=%v, want original cause", err)
	}
}

func TestDNSFailureKeepsCauseWithoutExposingResolverDetails(t *testing.T) {
	cause := errors.New("resolver at 192.0.2.4 contained private search data")
	err := dnsUnavailable("resolver configuration read", cause)
	if !errors.Is(err, ErrDNSUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("classification/cause lost: %T", err)
	}
	if strings.Contains(err.Error(), "192.0.2.4") || strings.Contains(err.Error(), "private search data") {
		t.Fatalf("unsafe failure text: %q", err)
	}
}
