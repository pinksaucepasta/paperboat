package main

import "github.com/pinksaucepasta/paperboat/internal/machineguard"

func uninstallCleanupOperations(err error) []string {
	var operations []string
	remaining := 128
	var visit func(error, int)
	visit = func(err error, depth int) {
		if err == nil || depth > 32 || remaining == 0 {
			return
		}
		remaining--
		if owner, ok := err.(interface{ CleanupOperation() string }); ok {
			operation := owner.CleanupOperation()
			if machineguard.ValidCleanupOperation(operation) {
				found := false
				for _, existing := range operations {
					found = found || existing == operation
				}
				if !found {
					operations = append(operations, operation)
				}
			}
		}
		switch owner := err.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range owner.Unwrap() {
				visit(child, depth+1)
			}
		case interface{ Unwrap() error }:
			visit(owner.Unwrap(), depth+1)
		}
	}
	visit(err, 0)
	return operations
}

func validUninstallCleanupOperations(operations []string) bool {
	if len(operations) > machineguard.MaxCleanupOperations {
		return false
	}
	for index, operation := range operations {
		if !machineguard.ValidCleanupOperation(operation) {
			return false
		}
		for _, previous := range operations[:index] {
			if previous == operation {
				return false
			}
		}
	}
	return true
}
