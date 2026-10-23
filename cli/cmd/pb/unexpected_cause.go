package main

import "github.com/pinksaucepasta/paperboat/internal/causetext"

// lastUnexpectedFailureFile keeps the cause text of the latest unexpected command failure next to the
// diagnostics (internal/causetext).
const lastUnexpectedFailureFile = "last-unexpected-failure.txt"

func recordUnexpectedFailureCause(directory, operation, supportReference string, err error) error {
	return causetext.Record(directory, lastUnexpectedFailureFile, operation, supportReference, err)
}
