package main

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"io"
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

func TestOnlyAnUncertainPeerStartIsRetriedForAComparison(t *testing.T) {
	if !retryableConfigComparison(&configComparisonFailure{cause: &tunnel.ExecStartUncertainError{Cause: io.EOF}}) {
		t.Fatal("a comparison whose peer start was uncertain must be retried (it is read-only)")
	}
	for _, err := range []error{nil, errors.New("comparison integrity failed; refresh and retry"), &configComparisonFailure{cause: configsync.ErrConflictComparisonStale}} {
		if retryableConfigComparison(err) {
			t.Fatalf("%v must not be retried", err)
		}
	}
}
