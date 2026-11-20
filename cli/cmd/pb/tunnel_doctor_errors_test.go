package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func TestTunnelDoctorPartialCollectionIsObservedAndRecovers(t *testing.T) {
	private := errors.New("PRIVATE_DOCTOR_PROVIDER_DETAIL")
	deps := defaultTunnelDependencies()
	deps.hostDiagnostics = func(context.Context) (tunnelHostDiagnostics, error) {
		return tunnelHostDiagnostics{}, errors.Join(private, syscall.EIO)
	}
	deps.localReport = func(context.Context) (localDoctorReport, error) { return localDoctorReport{}, nil }
	deps.dnsReady = func(context.Context) bool { return false }
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	faults := make(chan errorreport.Fault, 4)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	ctx := supportref.WithContext(deps.context(t.Context()), reference)
	_, preview, err := buildTunnelDoctorBundle(ctx, api.TunnelHealth{}, tunnelDoctorBundleDefaultBytes)
	if err != nil || !errors.Is(preview.CollectionError(), private) || !errors.Is(preview.CollectionError(), syscall.EIO) {
		t.Fatal("partial bundle lost its original collector causes or became fatal")
	}
	encoded, err := json.Marshal(preview)
	if err != nil || strings.Contains(string(encoded), private.Error()) {
		t.Fatal("partial preview serialized private diagnostic causes")
	}
	select {
	case fault := <-faults:
		if fault.SupportReference != reference || fault.Stage != "command" || fault.Code != "command_failed" || fault.Cause != "system_error" || fault.Errno != int(syscall.EIO) {
			t.Fatalf("partial collection lost its correlated observation: %+v", fault)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("partial collection failure disappeared before the reporting owner")
	}
	deps.hostDiagnostics = func(context.Context) (tunnelHostDiagnostics, error) { return tunnelHostDiagnostics{}, nil }
	ctx = deps.context(ctx)
	_, preview, err = buildTunnelDoctorBundle(ctx, api.TunnelHealth{}, tunnelDoctorBundleDefaultBytes)
	if err != nil || preview.CollectionError() != nil {
		t.Fatal("fresh diagnostic collection did not recover")
	}
	select {
	case <-faults:
		t.Fatal("partial collection was observed more than once")
	default:
	}
}
