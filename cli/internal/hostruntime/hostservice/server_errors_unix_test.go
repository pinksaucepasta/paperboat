//go:build darwin || linux

package hostservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestAvailabilityRequestCapturesOriginalFailureWithSafeResponse(t *testing.T) {
	const sensitive = "/private/credential/config.json"
	cause := &os.PathError{Op: "read", Path: sensitive, Err: syscall.EIO}
	server := testServer(t, os.Getuid(), &fakeApplier{err: errors.Join(context.Canceled, cause)})
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	reference := "support_123e4567-e89b-42d3-a456-426614174000"
	ctx := supportref.WithContext(context.Background(), reference)
	response := requestAvailability(t, server, ctx)
	if response.Status != "error" || response.ErrorCode != "availability_apply_failed" {
		t.Fatalf("response did not preserve the finite failure code: %+v", response)
	}
	if len(faults) != 1 {
		t.Fatalf("captured failure count=%d", len(faults))
	}
	fault := faults[0]
	if fault.Operation != "service" || fault.Stage != "reconciliation" || fault.Code != "availability_apply_failed" || fault.SupportReference != reference {
		t.Fatalf("fault lost phase or request reference: %+v", fault)
	}
	if fault.Errno != int(syscall.EIO) {
		t.Fatalf("fault lost the underlying operational cause: errno=%d", fault.Errno)
	}
	if strings.Contains(fault.Cause, sensitive) || strings.Contains(strings.Join(fault.ErrorChain, ","), sensitive) {
		t.Fatalf("fault retained arbitrary cause text: %+v", fault)
	}
}

func TestAvailabilityRequestKeepsPureCancellationQuiet(t *testing.T) {
	server := testServer(t, os.Getuid(), &fakeApplier{err: context.Canceled})
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()

	response := requestAvailability(t, server, context.Background())
	if response.Status != "error" || response.ErrorCode != "availability_apply_failed" {
		t.Fatalf("response did not preserve the finite failure code: %+v", response)
	}
	if len(faults) != 0 {
		t.Fatalf("pure cancellation produced an operational observation: %+v", faults)
	}
}

func TestAvailabilityRequestObservesMalformedCauses(t *testing.T) {
	cycle := &cyclicDiagnosticError{}
	cycle.next = cycle
	wides := make([]error, 17)
	for index := range wides {
		wides[index] = errors.New("expected-looking child")
	}
	tests := []struct {
		name  string
		cause error
	}{
		{name: "cycle", cause: cycle},
		{name: "oversized tree", cause: errors.Join(wides...)},
		{name: "panicking unwrap", cause: panicUnwrapDiagnosticError{}},
		{name: "noncomparable leaf", cause: nonComparableDiagnosticError("private diagnostic detail")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := testServer(t, os.Getuid(), &fakeApplier{err: test.cause})
			var faults []errorreport.Fault
			restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
				faults = append(faults, fault)
			})
			defer restore()

			response := requestAvailability(t, server, context.Background())
			if response.Status != "error" || response.ErrorCode != "availability_apply_failed" {
				t.Fatalf("response did not preserve the finite failure code: %+v", response)
			}
			if len(faults) != 1 || faults[0].Operation != "service" || faults[0].Stage != "reconciliation" || faults[0].Code != "availability_apply_failed" {
				t.Fatalf("malformed operational cause was not observed once: %+v", faults)
			}
		})
	}
}

func requestAvailability(t *testing.T, server *Server, ctx context.Context) Response {
	t.Helper()
	serverSide, clientSide := unixPair(t)
	done := make(chan struct{})
	go func() {
		server.serveConnection(ctx, serverSide)
		close(done)
	}()
	if err := json.NewEncoder(clientSide).Encode(Request{Schema: ProtocolV1, Operation: "apply_availability", Mode: KeepAwake, Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := clientSide.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(clientSide).Decode(&response); err != nil {
		t.Fatal(err)
	}
	_ = clientSide.Close()
	<-done
	return response
}

type cyclicDiagnosticError struct{ next error }

func (e *cyclicDiagnosticError) Error() string { return "cyclic diagnostic error" }
func (e *cyclicDiagnosticError) Unwrap() error { return e.next }

type panicUnwrapDiagnosticError struct{}

func (panicUnwrapDiagnosticError) Error() string { return "panicking diagnostic error" }
func (panicUnwrapDiagnosticError) Unwrap() error { panic("private diagnostic detail") }

type nonComparableDiagnosticError []byte

func (nonComparableDiagnosticError) Error() string { return "noncomparable diagnostic error" }
