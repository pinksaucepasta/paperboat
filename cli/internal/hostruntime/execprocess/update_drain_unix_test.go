//go:build darwin || linux

package execprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestUpdateAdmissionWaitsForNaturalExitAndReopens(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	command, _, err := manager.Start(context.Background(), Request{
		OperationID: "operation_update_drain_running",
		Argv:        []string{"/bin/sh", "-c", "sleep 0.2; printf finished"},
		CWD:         root,
	})
	if err != nil {
		t.Fatal(err)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	drainDone := make(chan error, 1)
	go func() { drainDone <- manager.BeginUpdate(drainCtx, "update_drain_01") }()

	// Probe through the public admission API until the update fence is visible.
	deadline := time.Now().Add(time.Second)
	fenced := false
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		probe, _, startErr := manager.Start(context.Background(), Request{
			OperationID: fmt.Sprintf("operation_update_probe_%d", attempt),
			Argv:        []string{"/bin/true"},
			CWD:         root,
		})
		if errors.Is(startErr, ErrUpdateInProgress) {
			fenced = true
			break
		}
		if startErr != nil {
			t.Fatal(startErr)
		}
		if _, waitErr := probe.Wait(context.Background()); waitErr != nil {
			t.Fatal(waitErr)
		}
	}
	if !fenced {
		t.Fatal("update drain did not fence new exec admission")
	}
	if _, _, err := manager.Start(context.Background(), Request{OperationID: "operation_update_rejected", Argv: []string{"/bin/true"}, CWD: root}); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("new exec crossed update fence: %v", err)
	}

	snapshot, err := command.Wait(drainCtx)
	if err != nil || snapshot.State != StateExited || snapshot.Result == nil || snapshot.Result.Code != 0 {
		t.Fatalf("running command did not finish naturally: snapshot=%+v err=%v", snapshot, err)
	}
	var output []byte
	for sequence := uint64(1); ; sequence++ {
		event, eventErr := command.Next(drainCtx, sequence)
		if errors.Is(eventErr, io.EOF) {
			break
		}
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		if event.Stream == "stdout" {
			output = append(output, event.Data...)
		}
	}
	if string(output) != "finished" {
		t.Fatalf("finished command output=%q", output)
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("drain after natural exit: %v", err)
	}
	if err := manager.EndUpdate("update_drain_01"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Start(context.Background(), Request{OperationID: "operation_update_reopened", Argv: []string{"/bin/true"}, CWD: root}); err != nil {
		t.Fatalf("exec admission did not reopen after update: %v", err)
	}
}

func TestCanceledUpdateDrainReopensWithoutCancelingRunningExec(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	command, _, err := manager.Start(context.Background(), Request{
		OperationID: "operation_update_drain_cancel",
		Argv:        []string{"/bin/sh", "-c", "sleep 0.2; printf preserved"},
		CWD:         root,
	})
	if err != nil {
		t.Fatal(err)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err = manager.BeginUpdate(drainCtx, "update_drain_cancel")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled drain error=%v", err)
	}
	if snapshot := command.Snapshot(); terminalState(snapshot.State) {
		t.Fatalf("canceled drain terminated the running command: %+v", snapshot)
	}
	if _, _, err := manager.Start(context.Background(), Request{OperationID: "operation_after_canceled_drain", Argv: []string{"/bin/true"}, CWD: root}); err != nil {
		t.Fatalf("canceled drain stranded exec admission: %v", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	snapshot, err := command.Wait(waitCtx)
	if err != nil || snapshot.State != StateExited || snapshot.Result == nil || snapshot.Result.Code != 0 {
		t.Fatalf("command did not survive canceled drain: snapshot=%+v err=%v", snapshot, err)
	}
}
