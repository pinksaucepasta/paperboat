package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type diagnosticEnvironment struct {
	failed   bool
	sequence uint64
}

func TestRuntimeObservationCanceledStartDoesNotInstallLoop(t *testing.T) {
	transport := &livenessObservationTransport{notify: make(chan struct{}, 1)}
	sender := &runtimeObservationSender{endpoint: "https://observations.invalid/v1/runtime-observations", tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{}, operationID: func() (string, error) { return "operation_test", nil }, client: &http.Client{Transport: transport}}
	service := &runtimeObservationService{sender: sender, interval: time.Millisecond, timeout: time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := service.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled startup was accepted")
	}
	if transport.calls.Load() != 0 || service.cancel != nil || service.done != nil {
		t.Fatal("canceled startup installed a heartbeat loop")
	}
}

func (environment *diagnosticEnvironment) FlushLayerObservations(context.Context) error {
	if environment.failed {
		return syscall.ENOSPC
	}
	return nil
}

func TestRuntimeEnvironmentOutageKeepsHeartbeatAndReportsRecovery(t *testing.T) {
	local := diagnostics.NewMemoryRecorder()
	reference := supportref.New()
	ctx := diagnostics.WithRecorder(supportref.WithContext(t.Context(), reference), local)
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
		if err := local.RecordFault(fault); err != nil {
			t.Fatal(err)
		}
	})
	defer restore()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"data":{}}`))
	}))
	defer server.Close()
	environment := &diagnosticEnvironment{failed: true}
	sender := &runtimeObservationSender{endpoint: server.URL, tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{}, operationID: func() (string, error) { return "operation_test", nil }, layers: environment, client: server.Client()}
	for range 2 {
		if err := sender.Send(ctx); err != nil {
			t.Fatal("auxiliary storage outage suppressed heartbeat")
		}
	}
	if len(faults) != 1 || faults[0].Code != "environment_sync_failed" || faults[0].Cause != "resource_exhausted" || faults[0].SupportReference != reference || faults[0].Stage != "diagnostic_storage" {
		t.Fatalf("environment faults=%#v", faults)
	}
	environment.failed = false
	for range 2 {
		if err := sender.Send(ctx); err != nil {
			t.Fatal("recovered environment suppressed heartbeat")
		}
	}
	if calls.Load() != 4 {
		t.Fatal("heartbeat stopped during outage/recovery")
	}
	events := local.Recent()
	if len(events) != 2 || events[1].Code != "recovered" || events[1].SupportReference != reference {
		t.Fatalf("environment recovery events=%#v", events)
	}
}
