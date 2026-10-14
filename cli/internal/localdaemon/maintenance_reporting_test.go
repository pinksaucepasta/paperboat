package localdaemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestMaintenanceOutageRetainsCauseReferenceAndRecovery(t *testing.T) {
	for _, kind := range []string{"managed_ssh", "peer_metadata"} {
		t.Run(kind, func(t *testing.T) {
			recorder := diagnostics.NewMemoryRecorder()
			ctx := supportref.WithContext(t.Context(), supportref.New())
			var faults []errorreport.Fault
			restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
			defer restore()
			observe := maintenanceObserver(ctx, recorder, kind)
			observe(syscall.EIO)
			observe(syscall.EIO)
			observe(context.Canceled)
			if len(faults) != 1 {
				t.Fatal("maintenance retry duplicated or suppressed")
			}
			observe(nil)
			records := recorder.Recent()
			if len(records) != 2 || records[0].Fields["outcome"] != "degraded" || records[1].Fields["outcome"] != "ready" {
				t.Fatalf("records=%+v", records)
			}
			for _, record := range records {
				if record.SupportReference != supportref.FromContext(ctx) {
					t.Fatal("maintenance reference changed")
				}
			}
			observe(errors.Join(context.Canceled, syscall.EIO))
			if len(faults) != 2 || faults[1].Errno != int(syscall.EIO) || faults[1].Outcome == "canceled" {
				t.Fatal("maintenance failure masked by cancellation")
			}
		})
	}
}

func TestManagedSSHStartupReportingSkipsOwnedHTTPAndPureCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable"}}`))
	}))
	defer server.Close()
	ctx := supportref.WithContext(t.Context(), supportref.New())
	recorder := diagnostics.NewMemoryRecorder()
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	_, err := api.New(server.URL, config.Credential{}, server.Client()).Me(ctx)
	if err == nil || !errorreport.HTTPAttemptObserved(err) || len(faults) != 1 {
		t.Fatal("fixture did not record HTTP attempt")
	}
	source := &scriptedMachineSource{}
	reportManagedSSHStartup(ctx, source, recorder, err)
	reportManagedSSHStartup(ctx, source, recorder, context.Canceled)
	if len(faults) != 1 {
		t.Fatal("startup duplicated HTTP attempt or ordinary cancellation")
	}
	reportManagedSSHStartup(ctx, source, recorder, errors.Join(context.Canceled, syscall.EIO))
	if len(faults) != 2 || faults[1].Errno != int(syscall.EIO) || faults[1].SupportReference != supportref.FromContext(ctx) {
		t.Fatal("startup masked independent operational failure")
	}
}
