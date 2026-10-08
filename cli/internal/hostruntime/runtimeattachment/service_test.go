package runtimeattachment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestNewUsesOneSharedCarrierWithBrowserStreamCapacity(t *testing.T) {
	service, err := New(Config{
		ControlURL: "https://api.example.test", StateRoot: t.TempDir(), MachineID: "machine-test",
		WorkerGeneration: 1, InstallationGeneration: 1, ListenAddress: "127.0.0.1:38080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.sessions == nil {
		t.Fatal("runtime attachment has no shared carrier source")
	}
}

func TestChooseLiveKeepsRecoveredPrimaryAsBackup(t *testing.T) {
	primary := admissionBinding{Binding: binding{EdgeNodeID: "edge-primary", EdgeProcessEpoch: "epoch-primary", SessionID: "session-primary"}}
	backup := admissionBinding{Binding: binding{EdgeNodeID: "edge-backup", EdgeProcessEpoch: "epoch-backup", SessionID: "session-backup"}}
	primaryLive, backupLive := &live{}, &live{}
	ordered := []admissionBinding{primary, backup}
	lives := map[string]*live{carrierKey(primary): primaryLive, carrierKey(backup): backupLive}
	chosen, admission := chooseLive(ordered, lives, backup.Binding.EdgeNodeID, backup.Binding.EdgeProcessEpoch)
	if chosen != backupLive || admission.Binding.EdgeNodeID != backup.Binding.EdgeNodeID {
		t.Fatal("recovered primary preempted healthy active backup")
	}
	delete(lives, carrierKey(backup))
	chosen, admission = chooseLive(ordered, lives, backup.Binding.EdgeNodeID, backup.Binding.EdgeProcessEpoch)
	if chosen != primaryLive || admission.Binding.EdgeNodeID != primary.Binding.EdgeNodeID {
		t.Fatal("failed active backup did not promote ready primary")
	}
	backup.Binding.EdgeProcessEpoch = "replacement-epoch"
	ordered[1] = backup
	lives[carrierKey(backup)] = backupLive
	chosen, admission = chooseLive(ordered, lives, "edge-backup", "epoch-backup")
	if chosen != primaryLive || admission.Binding.EdgeNodeID != primary.Binding.EdgeNodeID {
		t.Fatal("stale active edge epoch retained authority")
	}
}

func TestRuntimeCarrierDiagnosticsPreserveAuthorityCauseWithoutText(t *testing.T) {
	secret := errors.New("private machine identity detail")
	err := carrierFailure(errors.Join(preview.ErrMachineAttachmentTrustRequired, secret))
	if !errors.Is(err, preview.ErrMachineAttachmentTrustRequired) || !errors.Is(err, secret) {
		t.Fatalf("typed cause chain=%v", err)
	}
	if strings.Contains(err.Error(), secret.Error()) {
		t.Fatalf("raw identity detail escaped: %v", err)
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboat-daemon", "preview_attachment", "lifecycle", "service_failed", err)
	if fault.Stage != peerAuthorityStage || fault.Code != peerAuthorityCode || fault.Cause == "context_canceled" {
		t.Fatalf("authority classification=%+v", fault)
	}
}

func TestStartCanceledDoesNotInstallBackgroundWorker(t *testing.T) {
	service := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error=%v", err)
	}
	if service.done != nil || service.cancel != nil {
		t.Fatal("canceled start installed a background worker")
	}
}

func TestBackgroundFailureObservationsKeepSafeTypedClassesAndRecoveryBoundary(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()

	service := &Service{}
	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	secretCause := errors.New("edge credential content")
	first := carrierFailure(errors.Join(preview.ErrMachineAttachmentTrustRequired, secretCause))
	previous := service.observeFailure(ctx, first, nil)
	previous = service.observeFailure(ctx, first, previous)
	previous = service.observeFailure(ctx, peerConnectFailure(secretCause), previous)
	previous = service.observeFailure(ctx, errors.Join(context.Canceled, secretCause), previous)
	previous = service.observeFailure(ctx, context.Canceled, previous)

	if len(faults) != 3 {
		t.Fatalf("faults after duplicate and cancellation filtering=%d, want 3", len(faults))
	}
	if faults[0].Stage != peerAuthorityStage || faults[0].Code != peerAuthorityCode || faults[0].SupportReference != reference {
		t.Fatalf("authority fault=%+v", faults[0])
	}
	if faults[1].Stage != peerConnectStage || faults[1].Code != transportFailure {
		t.Fatalf("connect fault=%+v", faults[1])
	}
	if faults[2].Stage != "lifecycle" || faults[2].Code != "service_failed" || faults[2].Cause == "context_canceled" {
		t.Fatalf("mixed cancellation fault=%+v", faults[2])
	}
	for _, fault := range faults {
		if strings.Contains(strings.Join(fault.ErrorChain, " "), "edge credential content") || fault.SupportReference != reference {
			t.Fatalf("unsafe or uncorrelated fault=%+v", fault)
		}
	}
}
