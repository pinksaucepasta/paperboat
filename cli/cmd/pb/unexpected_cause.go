package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
)

// lastUnexpectedFailureFile keeps the cause text of the latest unexpected command failure next to the
// diagnostics, owner-only and never uploaded: the diagnostic event carries only the error's type chain,
// so without it an intermittent internal failure (a bare error) cannot be traced to its source.
const lastUnexpectedFailureFile = "last-unexpected-failure.txt"

func recordUnexpectedFailureCause(directory, operation, supportReference string, err error) error {
	if directory == "" || err == nil {
		return nil
	}
	if mkErr := os.MkdirAll(directory, 0o700); mkErr != nil {
		return mkErr
	}
	body := time.Now().UTC().Format(time.RFC3339) + " " + operation + " " + supportReference + "\n" + hostruntimecmd.FailureCauseText(err) + "\n"
	return atomicfile.Write(filepath.Join(directory, lastUnexpectedFailureFile), []byte(body), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
