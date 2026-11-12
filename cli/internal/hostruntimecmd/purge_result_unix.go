//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

const purgeResultLimit = 16 << 10

type purgeFault struct {
	Stage            string `json:"stage"`
	Code             string `json:"code"`
	Cause            string `json:"cause"`
	Errno            int    `json:"errno"`
	ErrorType        string `json:"error_type"`
	SupportReference string `json:"support_reference"`
}
type purgeResult struct {
	Version string       `json:"version"`
	OK      *bool        `json:"ok"`
	Faults  []purgeFault `json:"faults,omitempty"`
	// Remaining names the owned paths and processes the final inventory still found.
	Remaining []string `json:"remaining,omitempty"`
	// RemovedByName names what a cleanup removed by known names because the native lifecycle state was unreadable.
	RemovedByName []string `json:"removed_by_name,omitempty"`
	// Warnings names cleanups that did not finish without failing the purge (purgeWarning*).
	Warnings []string `json:"warnings,omitempty"`
}

const purgeRemainingLimit = 32

func (f purgeFault) Error() string {
	return "Paperboat privileged cleanup failed at " + f.Stage + ". Support reference: " + f.SupportReference
}
func (f purgeFault) DiagnosticStage() string { return f.Stage }
func (f purgeFault) DiagnosticCode() string  { return f.Code }
func (f purgeFault) DiagnosticCause() string { return f.Cause }
func (f purgeFault) DiagnosticErrno() int    { return f.Errno }

func writePurgeResult(ctx context.Context, output io.Writer, err error, removedByName ...string) error {
	return writePurgeReport(ctx, output, err, removedByName, nil)
}

func writePurgeReport(ctx context.Context, output io.Writer, err error, removedByName, warnings []string) error {
	ok := err == nil
	result := purgeResult{Version: "paperboat.purge-result/v1", OK: &ok}
	for _, warning := range warnings {
		if warning == purgeWarningSharedManuals {
			result.Warnings = append(result.Warnings, warning)
		}
	}
	for _, item := range removedByName {
		if len(result.RemovedByName) < purgeRemainingLimit && validPurgeRemaining(item) {
			result.RemovedByName = append(result.RemovedByName, item)
		}
	}
	if err != nil {
		pending := []error{err}
		for visited := 0; len(pending) > 0 && visited < 32; visited++ {
			current := pending[0]
			pending = pending[1:]
			if current == nil {
				continue
			}
			if staged, ok := current.(purgeStageFailure); ok {
				f := errorreport.ProjectFault(ctx, "paperboat-daemon", "uninstall", staged.stage, "purge_"+staged.stage+"_failed", staged)
				result.Faults = append(result.Faults, purgeFault{f.Stage, f.Code, f.Cause, f.Errno, f.ErrorType, f.SupportReference})
				continue
			}
			if joined, ok := current.(interface{ Unwrap() []error }); ok {
				pending = append(pending, joined.Unwrap()...)
			} else if wrapped, ok := current.(interface{ Unwrap() error }); ok {
				pending = append(pending, wrapped.Unwrap())
			}
		}
		if len(result.Faults) == 0 {
			f := errorreport.ProjectFault(ctx, "paperboat-daemon", "uninstall", "command", "command_failed", err)
			result.Faults = []purgeFault{{f.Stage, f.Code, f.Cause, f.Errno, f.ErrorType, f.SupportReference}}
		}
		var leftovers unixLeftoverError
		if errors.As(err, &leftovers) {
			for _, item := range leftovers.items {
				if len(result.Remaining) < purgeRemainingLimit && validPurgeRemaining(item) {
					result.Remaining = append(result.Remaining, item)
				}
			}
		}
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil || len(encoded)+1 > purgeResultLimit {
		return runtimeFailure("privileged cleanup result exceeded its bound", encodeErr)
	}
	_, writeErr := output.Write(append(encoded, '\n'))
	return errors.Join(err, writeErr)
}

// The report is trusted only when its complete framing and the process exit agree.
func readPurgeResult(data []byte, processErr error) error {
	_, err := readPurgeReport(data, processErr)
	return err
}

// readPurgeReport is readPurgeResult that also returns what the cleanup removed by known names.
func readPurgeReport(data []byte, processErr error) (removedByName []string, _ error) {
	removed, _, err := readPurgeSummary(data, processErr)
	return removed, err
}

// readPurgeSummary is readPurgeReport that also returns the warnings of a purge that otherwise succeeded.
func readPurgeSummary(data []byte, processErr error) (removedByName, warnings []string, _ error) {
	invalid := func() error { return runtimeFailure("privileged cleanup returned an invalid result", processErr) }
	if len(data) > purgeResultLimit {
		return nil, nil, invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result purgeResult
	if decoder.Decode(&result) != nil {
		return nil, nil, invalid()
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || result.Version != "paperboat.purge-result/v1" || result.OK == nil || *result.OK != (processErr == nil) || len(result.Faults) > 16 {
		return nil, nil, invalid()
	}
	if len(result.RemovedByName) > purgeRemainingLimit || len(result.Warnings) > 1 {
		return nil, nil, invalid()
	}
	for _, warning := range result.Warnings {
		if warning != purgeWarningSharedManuals {
			return nil, nil, invalid()
		}
	}
	for _, item := range result.RemovedByName {
		if !validPurgeRemaining(item) {
			return nil, nil, invalid()
		}
	}
	if *result.OK {
		if result.Faults != nil || result.Remaining != nil {
			return nil, nil, invalid()
		}
		return result.RemovedByName, result.Warnings, nil
	}
	if len(result.Faults) == 0 || len(result.Remaining) > purgeRemainingLimit {
		return nil, nil, invalid()
	}
	for _, item := range result.Remaining {
		if !validPurgeRemaining(item) {
			return nil, nil, invalid()
		}
	}
	incomplete := PurgeIncomplete{Remaining: result.Remaining}
	var retained []error
	for _, f := range result.Faults {
		if !validPurgeStage(f.Stage) || f.Code != "purge_"+f.Stage+"_failed" && !(f.Stage == "command" && f.Code == "command_failed") || !errorreport.ValidCause(f.Cause) || f.Errno < 0 || f.Errno > 65535 || !supportref.Valid(f.SupportReference) || len(f.ErrorType) > 64 {
			return nil, nil, invalid()
		}
		for _, c := range f.ErrorType {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return nil, nil, invalid()
			}
		}
		if f.ErrorType == "" {
			return nil, nil, invalid()
		}
		retained = append(retained, f)
		incomplete.Stages = append(incomplete.Stages, f.Stage)
	}
	return result.RemovedByName, nil, errors.Join(append(retained, incomplete, processErr)...)
}

// validPurgeRemaining accepts only what the final inventory names: an absolute
// path or "process <pid> (<path>)", bounded and free of control characters.
func validPurgeRemaining(item string) bool {
	if len(item) == 0 || len(item) > 512 || !strings.HasPrefix(item, "/") && !strings.HasPrefix(item, "process ") {
		return false
	}
	for _, c := range item {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
func validPurgeStage(stage string) bool {
	switch stage {
	case "uninstall_prepare", "uninstall_lock", "uninstall_recover", "uninstall_sessions", "uninstall_activator", "uninstall_services", "uninstall_power", "uninstall_journal", "uninstall_files", "uninstall_lock_close", "native_uninstall", "account_sweep", "machine_guard", "shared_payload_parents", "final_inventory", "reset_cli", "command":
		return true
	}
	return false
}

type purgeOutput struct {
	data     []byte
	exceeded bool
}

func (w *purgeOutput) Write(data []byte) (int, error) {
	count := len(data)
	if len(w.data)+count > purgeResultLimit {
		w.exceeded = true
		remaining := purgeResultLimit - len(w.data)
		w.data = append(w.data, data[:remaining]...)
	} else {
		w.data = append(w.data, data...)
	}
	return count, nil
}

// ExecutePurge retains the privileged cleanup's safe typed failure for the public owner. It also returns what the
// cleanup removed by known names when the native lifecycle state was unreadable.
func ExecutePurge(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) ([]string, error) {
	if ctx == nil || stdin == nil || stdout == nil || stderr == nil {
		return nil, runtimeFailure("privileged cleanup invocation is invalid")
	}
	return runPurge(ctx, nil, stdin, stdout, stderr)
}

// ExecuteResetPurge is ExecutePurge for pb reset: it keeps the account CLI it can prove.
func ExecuteResetPurge(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if ctx == nil || stdin == nil || stdout == nil || stderr == nil {
		return runtimeFailure("privileged cleanup invocation is invalid")
	}
	return runPurgeCommand(ctx, []string{"--preserve-cli"}, stdin, stdout, stderr)
}
