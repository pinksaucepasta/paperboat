package tunnel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

func TestCLINativePeerEventsProjectSafeAttemptAndWorkerFailures(t *testing.T) {
	var observed []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		observed = append(observed, fault)
	})
	defer restore()

	privateCause := errors.New("peer machine_secret at 100.64.1.2 exposed private data")
	observeCLINativePeerEvent(native.Event{
		Kind:   "dial_failed",
		PeerID: "machine_secret",
		Err:    &nativePhaseFailure{stage: "peer_authority", code: "peer_authority_failed", err: privateCause},
	})
	if len(observed) != 1 {
		t.Fatalf("dial attempt produced %d faults, want one", len(observed))
	}
	dial := observed[0]
	if dial.Component != "paperboat-cli" || dial.Operation != "connect" || dial.Stage != "peer_authority" || dial.Code != "peer_authority_failed" {
		t.Fatalf("dial fault classification = %+v", dial)
	}
	if strings.Contains(strings.Join(dial.ErrorChain, ","), "machine_secret") || strings.Contains(dial.SupportReference, "machine_secret") {
		t.Fatalf("dial fault retained private identity: %+v", dial)
	}

	observeCLINativePeerEvent(native.Event{
		Kind:   "accept_failed",
		PeerID: "machine_secret",
		Err:    errors.New("private transport detail"),
	})
	if len(observed) != 2 {
		t.Fatalf("terminal worker failure produced %d faults, want two total", len(observed))
	}
	worker := observed[1]
	if worker.Operation != "connect" || worker.Stage != "peer_connect" || worker.Code != "native_private_failed" {
		t.Fatalf("worker fault classification = %+v", worker)
	}
	if strings.Contains(strings.Join(worker.ErrorChain, ","), "machine_secret") || strings.Contains(strings.Join(worker.ErrorChain, ","), "private transport detail") {
		t.Fatalf("worker fault retained private data: %+v", worker)
	}

	observeCLINativePeerEvent(native.Event{Kind: "serve_failed", Err: errors.New("private serving detail")})
	if len(observed) != 3 || observed[2].Operation != "serve" || observed[2].Stage != "lifecycle" || observed[2].Code != "service_failed" {
		t.Fatalf("generic serving fault classification = %+v", observed)
	}
}

func TestCLINativePeerEventsIgnoreCancellationAndUnknownKinds(t *testing.T) {
	var count int
	restore := errorreport.InstallFaultObserver(func(context.Context, errorreport.Fault) { count++ })
	defer restore()

	observeCLINativePeerEvent(native.Event{Kind: "dial_failed", Err: context.Canceled})
	observeCLINativePeerEvent(native.Event{Kind: "unknown_kind", Err: errors.New("unmapped event")})
	if count != 0 {
		t.Fatalf("canceled or unknown native events produced %d faults", count)
	}
}
