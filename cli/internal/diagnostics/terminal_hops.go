package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// TerminalHopsEnabled enables metadata-only capture on an explicitly instrumented device.
func TerminalHopsEnabled() bool { return os.Getenv("PAPERBOAT_TERMINAL_HOPS") == "1" }

// TerminalHop correlates byte ranges, never terminal data or resource identities.
// at may precede a lock: retaining the observation time avoids mistaking lock wait for PTY delay.
func TerminalHop(stage, identity string, start, end uint64, at time.Time) {
	if !TerminalHopsEnabled() {
		return
	}
	_, file, line, _ := runtime.Caller(1)
	sum := sha256.Sum256([]byte(identity))
	diagnosticlog.TryInfo("terminal output hop", "stage", stage, "hop_label", hex.EncodeToString(sum[:8]),
		"range_start", start, "range_end", end, "observed_unix_us", at.UnixMicro(), "hop_source_file", filepath.Base(file), "hop_source_line", line)
}

// UpdateStep records only the authored operation and timing, never a path or artifact contents.
func UpdateStep(stage string) func() {
	if !TerminalHopsEnabled() {
		return func() {}
	}
	diagnosticlog.TryInfo("update preparation step", "stage", stage, "state", "start")
	return func() { diagnosticlog.TryInfo("update preparation step", "stage", stage, "state", "end") }
}

// ConnectionHopLabel matches both ends without retaining socket addresses.
func ConnectionHopLabel(local, remote string) string {
	if local > remote {
		local, remote = remote, local
	}
	sum := sha256.Sum256([]byte(local + "_" + remote))
	return hex.EncodeToString(sum[:8])
}
