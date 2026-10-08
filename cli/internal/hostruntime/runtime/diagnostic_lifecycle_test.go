package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/observability"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestRuntimeDiagnosticsRetainReferenceThroughFreshShutdownContext(t *testing.T) {
	restore := errorreport.Install(nil)
	defer restore()
	local := diagnostics.NewMemoryRecorder()
	reference := supportref.New()
	ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), local)
	r, err := NewRuntime(Config{Version: "test", Components: []Component{{Capability: "worker_lifecycle", Required: true, Service: &service{name: "worker", recorder: &recorder{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := local.Recent()
	if len(events) != 2 || events[0].Code != "ready" || events[1].Code != "stopped" {
		t.Fatalf("events=%#v", events)
	}
	for _, event := range events {
		if event.SupportReference != reference {
			t.Fatal("lifecycle reference changed")
		}
	}
	if err := local.Record("lifecycle", "still_open", "info", nil); err != nil {
		t.Fatal("runtime closed its borrowed recorder")
	}
}

func TestRuntimeDiagnosticFailureAndRollbackPublishOnceWithOriginalCause(t *testing.T) {
	for _, test := range []struct {
		name                    string
		cause                   error
		outcome, severity, code string
	}{
		{"cancellation", context.Canceled, "canceled", "info", "start_canceled"},
		{"deadline", context.DeadlineExceeded, "failed", "error", "start_deadline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			restore := errorreport.Install(nil)
			defer restore()
			local := diagnostics.NewMemoryRecorder()
			reference := supportref.New()
			ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), local)
			var faults []errorreport.Fault
			restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) {
				faults = append(faults, f)
				if err := local.RecordFault(f); err != nil {
					t.Fatal(err)
				}
			})
			defer restoreObserver()
			events, err := observability.NewEventLog(8)
			if err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			calls := &recorder{}
			r, err := NewRuntime(Config{Version: "test", EventLog: events, Components: []Component{
				{Capability: "worker_lifecycle", Required: true, Service: &service{name: "worker", recorder: calls}},
				{Capability: "config_sync", Required: true, Service: &service{name: "config", recorder: calls, startErr: test.cause}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Start(ctx); !errors.Is(err, test.cause) {
				t.Fatal("original start cause lost")
			}
			if len(faults) != 1 || faults[0].Code != test.code || faults[0].Outcome != test.outcome || faults[0].Severity != test.severity || faults[0].SupportReference != reference {
				t.Fatalf("faults=%#v", faults)
			}
			if len(events.Snapshot()) != 3 || len(faults) != 1 {
				t.Fatal("optional event log duplicated publication")
			}
			retained := local.Recent()
			if len(retained) != 3 || retained[0].Code != "ready" || retained[1].Code != test.code || retained[2].Code != "stopped" || retained[2].Category != "component_rollback" {
				t.Fatalf("retained=%#v", retained)
			}
			for _, event := range retained {
				if event.SupportReference != reference {
					t.Fatal("rollback reference changed")
				}
			}
		})
	}
}
