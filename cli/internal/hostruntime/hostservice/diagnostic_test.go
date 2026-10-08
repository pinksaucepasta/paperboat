package hostservice

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type cyclicDiagnosticError struct{ next error }

func (e *cyclicDiagnosticError) Error() string { return "cyclic diagnostic error" }
func (e *cyclicDiagnosticError) Unwrap() error { return e.next }

func TestErrorLeafClassificationDoesNotHideMixedFailures(t *testing.T) {
	if !normalRequestTermination(errors.Join(context.Canceled, io.EOF)) {
		t.Fatal("pure cancellation and EOF should be quiet")
	}
	if normalRequestTermination(errors.Join(context.Canceled, errors.New("storage unavailable"))) {
		t.Fatal("cancellation must not hide an operational failure")
	}
	if expectedRequestFailure(errors.Join(ErrPeerDenied, errors.New("credential lookup failed"))) {
		t.Fatal("peer denial must not hide an operational failure")
	}
}

func TestErrorLeafClassificationBoundsCyclesAndWideJoins(t *testing.T) {
	cyclic := &cyclicDiagnosticError{}
	cyclic.next = cyclic
	if normalRequestTermination(cyclic) {
		t.Fatal("cyclic error trees must be treated as unexpected")
	}
	wide := make([]error, 17)
	for index := range wide {
		wide[index] = errors.New("normal child")
	}
	if allErrorLeavesMatch(errors.Join(wide...), func(error) bool { return true }) {
		t.Fatal("oversized error trees must be rejected conservatively")
	}
}

func TestOperationFailureKeepsCauseWithoutFormattingIt(t *testing.T) {
	cause := errors.New("sensitive test value")
	err := failAt("peer_authority", "managed_ssh_failed", cause)
	if !errors.Is(err, cause) {
		t.Fatal("typed failure dropped its original cause")
	}
	if got := err.Error(); got != "managed SSH authorization reconciliation failed" {
		t.Fatalf("unsafe or unstable error text: %q", got)
	}
}

func TestConsumedRequestObservationKeepsReferenceAndMixedCause(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	reference := "support_123e4567-e89b-42d3-a456-426614174000"
	ctx := supportref.WithContext(context.Background(), reference)
	secret := "sensitive test value"
	observeUnexpected(ctx, "service", "lifecycle", "service_failed", errors.Join(context.Canceled, errors.New(secret)))
	if len(faults) != 1 {
		t.Fatalf("mixed cancellation/operation fault count=%d", len(faults))
	}
	fault := faults[0]
	if fault.Operation != "service" || fault.Stage != "lifecycle" || fault.Code != "service_failed" || fault.SupportReference != reference {
		t.Fatalf("fault lost owner classification or reference: %+v", fault)
	}
	if strings.Contains(fault.Cause, secret) || strings.Contains(strings.Join(fault.ErrorChain, ","), secret) {
		t.Fatalf("fault retained arbitrary cause text: %+v", fault)
	}
	observeUnexpected(ctx, "service", "lifecycle", "service_failed", context.Canceled)
	if len(faults) != 1 {
		t.Fatalf("pure cancellation should be quiet, fault count=%d", len(faults))
	}
}
