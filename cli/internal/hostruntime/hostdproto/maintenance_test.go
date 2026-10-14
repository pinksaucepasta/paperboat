package hostdproto

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOwnerMaintenanceAdmissionCancellationBusyAndExpiry(t *testing.T) {
	now := time.Now()
	c := &Controller{active: worker{workerID: "worker", apiVersion: 1, epoch: 1}, now: func() time.Time { return now }}
	release, err := c.AcquireActive("worker", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.PrepareMaintenance(ctx, false, nil); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatal(err)
	}
	release()
	busy := func() WorkloadStatus { return WorkloadStatus{Generation: 1, Protected: 1} }
	if _, err := c.PrepareMaintenance(context.Background(), false, busy); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatal(err)
	}
	release, err = c.AcquireActive("worker", 1)
	if err != nil {
		t.Fatal("busy preparation left fence", err)
	}
	release()
	if _, err := c.PrepareMaintenance(context.Background(), true, busy); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcquireActive("worker", 1); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatal("new work crossed fence", err)
	}
	c.AbortMaintenance()
	release, err = c.AcquireActive("worker", 1)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := c.PrepareMaintenance(context.Background(), false, nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(OwnerMaintenanceLease)
	release, err = c.AcquireActive("worker", 1)
	if err != nil {
		t.Fatal("abandoned lease did not expire", err)
	}
	release()
}

func TestOwnerStopAcknowledgementDrainsInputBeforeReplacementAdmission(t *testing.T) {
	c := &Controller{active: worker{workerID: "old", apiVersion: 1, epoch: 1}, candidate: worker{workerID: "new", apiVersion: 1, epoch: 2, lease: testLease(2), ready: true}, now: time.Now}
	release, err := c.AcquireActive("old", 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.FinishWorkerStop(context.Background()) }()
	// Observe the admission barrier directly under its owning mutex rather than
	// depending on scheduling sleeps.
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		replacing := c.replacing
		c.mu.Unlock()
		if replacing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop did not fence admission")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := c.AcquireActive("old", 1); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("stop acknowledged inflight write: %v", err)
	default:
	}
	activation := Activate{WorkerID: "new", APIVersion: 1, Epoch: 2, Lease: testLease(2)}
	if _, err := c.Activate(activation); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := c.Activate(activation); err != nil {
		t.Fatal(err)
	}
	release, err = c.AcquireActive("new", 2)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := c.AcquireActive("old", 1); !errors.Is(err, ErrFenced) {
		t.Fatal(err)
	}
}

func TestRestartedCandidateMaintenanceKeepsReadinessButFencesMutation(t *testing.T) {
	now := time.Now()
	previous, err := NewController(ControllerConfig{APIMin: 1, APIMax: 1, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	old, err := previous.Negotiate(Hello{WorkerID: "old", Version: "2026.10.13.1", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := previous.MarkReady(readyFor(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := previous.Activate(activateFor(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := previous.PrepareMaintenance(context.Background(), false, nil); err != nil {
		t.Fatal(err)
	}
	committed := false
	candidate, err := NewController(ControllerConfig{APIMin: 1, APIMax: 1, InitialEpoch: old.Epoch, Clock: func() time.Time { return now }, MaintenancePending: true, VerifyMaintenanceCommitted: func(context.Context) error {
		if !committed {
			return ErrMaintenanceBusy
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	next, err := candidate.Negotiate(Hello{WorkerID: "next", Version: "2026.10.13.2", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.MarkReady(readyFor(next)); err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.Activate(activateFor(next)); err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.AcquireActive(next.WorkerID, next.Epoch); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("candidate admitted mutation before native commit after restart: %v", err)
	}
	candidate.AbortMaintenance()
	now = now.Add(24 * time.Hour)
	if _, err := candidate.AcquireActive(next.WorkerID, next.Epoch); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("abort/expiry bypassed native commit: %v", err)
	}
	if err := candidate.CompleteMaintenance(context.Background()); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("uncommitted source cleared startup fence: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := candidate.CompleteMaintenance(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled completion cleared startup: %v", err)
	}
	committed = true
	if err := candidate.CompleteMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	release, err := candidate.AcquireActive(next.WorkerID, next.Epoch)
	if err != nil {
		t.Fatalf("committed candidate stayed fenced: %v", err)
	}
	release()
	if err := candidate.CompleteMaintenance(context.Background()); err != nil {
		t.Fatalf("matching committed retry failed: %v", err)
	}
	if _, err := NewController(ControllerConfig{APIMin: 1, APIMax: 1, MaintenancePending: true}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("pending native declaration without verifier admitted: %v", err)
	}

}
