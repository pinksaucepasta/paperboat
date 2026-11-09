package main

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/causetext"
)

// lastUnexpectedFailureFile keeps the cause text of the latest unexpected command failure next to the
// diagnostics (internal/causetext).
const lastUnexpectedFailureFile = "last-unexpected-failure.txt"

// A Windows uninstall failure owns private causes (cleanup paths, native errors) that are never formatted, here
// or anywhere: only its own fixed message is recorded. Every other failure keeps its full cause text.
func recordUnexpectedFailureCause(directory, operation, supportReference string, err error) error {
	var uninstall interface {
		error
		windowsUninstallFailureOwnership() (string, bool)
	}
	if errors.As(err, &uninstall) {
		err = errors.New(uninstall.Error())
	}
	return causetext.Record(directory, lastUnexpectedFailureFile, operation, supportReference, err)
}
