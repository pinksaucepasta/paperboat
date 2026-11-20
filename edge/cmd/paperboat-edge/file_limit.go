//go:build unix

package main

import (
	"context"
	"syscall"

	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	sharedtelemetry "github.com/pinksaucepasta/paperboat/shared/telemetry"
)

// fileLimitMargin is what carrier, mesh, control and visitor file descriptors need besides listeners: the edge has no
// connection limit, so the operator sizes the rest.
const fileLimitMargin = 1024

// openFileLimit is the process's effective soft RLIMIT_NOFILE (Go raises it to the hard limit at startup).
func openFileLimit() (uint64, bool) {
	var limit syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit) != nil {
		return 0, false
	}
	return uint64(limit.Cur), true
}

// warnIfFileLimitLow logs a warning event when the soft file limit cannot hold one listening socket for every public
// TCP tunnel port plus a margin. A node that forwards binds every port the
// peer directory names, up to the whole public range, so the unit's limit (LimitNOFILE, or the container's ulimit)
// must be at least this large; it returns whether the limit suffices.
func warnIfFileLimitLow(listeners int) bool {
	need := uint64(listeners) + fileLimitMargin
	have, ok := openFileLimit()
	if !ok || have >= need {
		return true
	}
	edgetelemetry.Log(context.Background(), sharedtelemetry.SeverityWarn, "file_limit_low", sharedtelemetry.OutcomeFailed, "have", have, "need", need, "repair", "raise LimitNOFILE (or the container ulimit -n) of the edge to at least the needed value")
	return false
}
