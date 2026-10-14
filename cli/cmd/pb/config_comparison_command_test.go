package main

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
)

func TestConfigComparisonMixedCancellationRetainsCauseUnderStaticMessage(t *testing.T) {
	private := errors.New("PRIVATE_STREAM_FAILURE")
	mixed := errors.Join(context.Canceled, private, syscall.EIO)
	got := protectConfigComparisonReadError(true, mixed)
	var comparisonFailure *configComparisonFailure
	if !errors.As(got, &comparisonFailure) || !errors.Is(got, context.Canceled) || !errors.Is(got, private) || !errors.Is(got, syscall.EIO) {
		t.Fatalf("mixed read causes were not preserved: %T", got)
	}
	if strings.Contains(got.Error(), "PRIVATE") {
		t.Fatalf("comparison failure exposed cause text: %q", got.Error())
	}
	if got := protectConfigComparisonReadError(true, context.Canceled); got != context.Canceled {
		t.Fatalf("pure cancellation changed: %T", got)
	}
	if got := protectConfigComparisonReadError(true, context.DeadlineExceeded); got != context.DeadlineExceeded {
		t.Fatalf("pure deadline changed: %T", got)
	}
	if got := protectConfigComparisonReadError(false, private); got != private {
		t.Fatalf("pre-read error changed: %T", got)
	}
}
