//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestPeerListenerFailureKeepsBindStageAndOriginalCause(t *testing.T) {
	ctx, local := peerDiagnosticContext(t)
	failure := nativePeerListenerError{cause: syscall.EADDRINUSE}
	service := &productionNativePeerService{}
	service.recordPeerState(ctx, failure)
	events := local.Recent()
	if !errors.Is(service.LastError(), syscall.EADDRINUSE) || len(events) != 1 || events[0].Category != "listener_bind" || events[0].Code != "native_private_failed" || events[0].SupportReference != supportref.FromContext(ctx) {
		t.Fatalf("listener failure lost stage/cause/reference: %#v", events)
	}
}

func peerDiagnosticContext(t *testing.T) (context.Context, *diagnostics.Recorder) {
	t.Helper()
	restore := errorreport.Install(nil)
	t.Cleanup(restore)
	local := diagnostics.NewMemoryRecorder()
	ctx := diagnostics.WithRecorder(supportref.WithContext(t.Context(), supportref.New()), local)
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		if err := local.RecordFault(fault); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(restoreObserver)
	return ctx, local
}

func TestPeerDiagnosticsPreserveTransitionsWithoutRetryFlood(t *testing.T) {
	ctx, local := peerDiagnosticContext(t)
	s := &productionNativePeerService{}
	s.recordPeerState(ctx, errNativePeerPending)
	s.recordPeerState(ctx, errNativePeerPending)
	failure := &api.APIError{Status: 503, Message: "PRIVATE_DATABASE_DETAIL"}
	s.recordPeerState(ctx, failure)
	s.recordPeerState(ctx, &api.APIError{Status: 503, Message: "OTHER_PRIVATE_DETAIL"})
	if s.LastError() == nil {
		t.Fatal("retry lost its original cause")
	}
	s.recordPeerState(ctx, nil)
	events := local.Recent()
	if len(events) != 3 || events[0].Code != "approval_pending" || events[1].Fields["http_status"] != "503" || events[2].Code != "recovered" {
		t.Fatalf("transitions=%#v", events)
	}
	for _, event := range events {
		if event.SupportReference != supportref.FromContext(ctx) {
			t.Fatal("transition lost invocation reference")
		}
	}
	if s.LastError() != nil {
		t.Fatal("recovery retained stale failure")
	}
}

func TestPeerSupervisorRecordsBackgroundFailureAndRecovery(t *testing.T) {
	ctx, local := peerDiagnosticContext(t)
	first := &productionNativePeerGeneration{cancel: func() {}, errors: make(chan error, 1)}
	second := &productionNativePeerGeneration{cancel: func() {}, errors: make(chan error, 1)}
	starts := 0
	s := &productionNativePeerService{startGeneration: func(context.Context) (*productionNativePeerGeneration, error) {
		starts++
		if starts == 1 {
			return first, nil
		}
		return second, nil
	}}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(cleanup); err != nil {
			t.Error(err)
		}
	})
	first.errors <- errors.New("PRIVATE_LISTENER_DETAIL")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events := local.Recent()
		if len(events) == 2 && events[1].Code == "recovered" {
			if events[0].Fields["cause"] != "internal" || events[0].SupportReference != supportref.FromContext(ctx) || events[1].SupportReference != supportref.FromContext(ctx) {
				t.Fatal("background transition lost original metadata")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("failure/recovery missing: %#v", local.Recent())
}

func TestPeerApplicationContextBorrowsDiagnosticsAndPreservesOwnerLifetime(t *testing.T) {
	daemonCtx, local := peerDiagnosticContext(t)
	ownerCtx, cancel := context.WithCancel(context.Background())
	applicationCtx := nativePeerApplicationContext(ownerCtx, daemonCtx)
	if diagnostics.FromContext(applicationCtx) != local || supportref.FromContext(applicationCtx) != supportref.FromContext(daemonCtx) {
		t.Fatal("application handler lost daemon diagnostics")
	}
	cancel()
	if !errors.Is(applicationCtx.Err(), context.Canceled) {
		t.Fatal("metadata attachment replaced owner cancellation")
	}
	if err := local.Record("peer_authority", "still_open", "info", nil); err != nil {
		t.Fatal("application closed borrowed recorder")
	}
}
