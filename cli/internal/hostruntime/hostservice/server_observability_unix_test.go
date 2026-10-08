//go:build darwin || linux

package hostservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestAvailabilityRequestCapturesOriginalFailureWithSafeResponse(t *testing.T) {
	const sensitive = "sensitive test configuration value"
	server := testServer(t, os.Getuid(), &fakeApplier{err: errors.New(sensitive)})
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	reference := "support_123e4567-e89b-42d3-a456-426614174000"
	ctx := supportref.WithContext(context.Background(), reference)
	serverSide, clientSide := unixPair(t)
	done := make(chan error, 1)
	go func() {
		done <- server.serveContext(ctx, serverSide)
		_ = serverSide.Close()
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
	if err := <-done; err != nil {
		t.Fatal(err)
	}
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
	if strings.Contains(fault.Cause, sensitive) || strings.Contains(strings.Join(fault.ErrorChain, ","), sensitive) {
		t.Fatalf("fault retained arbitrary cause text: %+v", fault)
	}
}
