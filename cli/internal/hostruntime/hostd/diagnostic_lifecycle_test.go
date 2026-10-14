package hostd

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type diagnosticService struct{ start, stop func(context.Context) error }

func (s diagnosticService) Start(ctx context.Context) error    { return s.start(ctx) }
func (s diagnosticService) Shutdown(ctx context.Context) error { return s.stop(ctx) }

func TestSupervisorRetainsDiagnosticsThroughCanceledStartAndDetachedRollback(t *testing.T) {
	restore := errorreport.Install(nil)
	defer restore()
	local := diagnostics.NewMemoryRecorder()
	reference := supportref.New()
	parent, cancel := context.WithCancel(diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), local))
	defer cancel()
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) {
		faults = append(faults, f)
		if err := local.RecordFault(f); err != nil {
			t.Fatal(err)
		}
	})
	defer restoreObserver()
	cleanups := 0
	stop := func(ctx context.Context) error {
		if ctx.Err() != nil || supportref.FromContext(ctx) != reference || diagnostics.FromContext(ctx) != local {
			t.Fatal("rollback lost detached execution context or process diagnostics")
		}
		cleanups++
		return nil
	}
	d, err := New(Config{Workloads: Workloads{Transfers: &filetransfer.Service{}}, Components: []Component{
		{Name: "storage", Required: true, Service: diagnosticService{start: func(context.Context) error { return nil }, stop: stop}},
		{Name: "config_sync", Required: true, Service: diagnosticService{start: func(context.Context) error { cancel(); return context.Canceled }, stop: stop}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(parent); !errors.Is(err, context.Canceled) {
		t.Fatal("startup cause was lost")
	}
	if cleanups != 2 || len(faults) != 1 || faults[0].Cause != "context_canceled" || faults[0].Severity != "info" {
		t.Fatalf("cleanups=%d faults=%#v", cleanups, faults)
	}
	events := local.Recent()
	if len(events) != 4 {
		t.Fatalf("local lifecycle events=%#v", events)
	}
	for _, event := range events {
		if event.SupportReference != reference {
			t.Fatal("supervisor lifecycle reference changed")
		}
	}
	if err := local.Record("lifecycle", "still_open", "info", nil); err != nil {
		t.Fatal("supervisor closed borrowed diagnostics")
	}
}
