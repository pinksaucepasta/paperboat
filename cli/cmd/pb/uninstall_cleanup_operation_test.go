package main

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"reflect"
	"testing"
)

type uninstallOperationTestError struct {
	operation string
	cause     error
}

func (e uninstallOperationTestError) Error() string            { panic("cleanup cause must not be formatted") }
func (e uninstallOperationTestError) Unwrap() error            { return e.cause }
func (e uninstallOperationTestError) CleanupOperation() string { return e.operation }

func TestUninstallCleanupOperationsPreserveJoinedProvenance(t *testing.T) {
	native := errors.New("PRIVATE native path")
	err := errors.Join(errors.Join(
		uninstallOperationTestError{machineguard.CleanupTaskRemove, native},
		uninstallOperationTestError{machineguard.CleanupWFPRemove, native},
		uninstallOperationTestError{machineguard.CleanupTaskRemove, native},
		uninstallOperationTestError{"PRIVATE path", native},
	))
	operations := uninstallCleanupOperations(err)
	if !reflect.DeepEqual(operations, []string{"guard_task_remove", "guard_wfp_remove"}) || !errors.Is(err, native) {
		t.Fatalf("joined cleanup provenance lost: %v", operations)
	}
	for _, invalid := range [][]string{{"PRIVATE path"}, {"guard_task_remove", "guard_task_remove"}, make([]string, machineguard.MaxCleanupOperations+1)} {
		if validUninstallCleanupOperations(invalid) {
			t.Fatal("unbounded/private cleanup metadata accepted")
		}
	}
}
