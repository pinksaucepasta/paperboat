//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
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
}

func (f purgeFault) Error() string {
	return "Paperboat privileged cleanup failed at " + f.Stage + ". Support reference: " + f.SupportReference
}
func (f purgeFault) DiagnosticStage() string { return f.Stage }
func (f purgeFault) DiagnosticCode() string  { return f.Code }
func (f purgeFault) DiagnosticCause() string { return f.Cause }
func (f purgeFault) DiagnosticErrno() int    { return f.Errno }

func writePurgeResult(ctx context.Context, output io.Writer, err error) error {
	ok := err == nil
	result := purgeResult{Version: "paperboat.purge-result/v1", OK: &ok}
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
	invalid := func() error { return runtimeFailure("privileged cleanup returned an invalid result", processErr) }
	if len(data) > purgeResultLimit {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result purgeResult
	if decoder.Decode(&result) != nil {
		return invalid()
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || result.Version != "paperboat.purge-result/v1" || result.OK == nil || *result.OK != (processErr == nil) || len(result.Faults) > 16 {
		return invalid()
	}
	if *result.OK {
		if result.Faults != nil {
			return invalid()
		}
		return nil
	}
	if len(result.Faults) == 0 {
		return invalid()
	}
	var retained []error
	for _, f := range result.Faults {
		if !validPurgeStage(f.Stage) || f.Code != "purge_"+f.Stage+"_failed" && !(f.Stage == "command" && f.Code == "command_failed") || !errorreport.ValidCause(f.Cause) || f.Errno < 0 || f.Errno > 65535 || !supportref.Valid(f.SupportReference) || len(f.ErrorType) > 64 {
			return invalid()
		}
		for _, c := range f.ErrorType {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return invalid()
			}
		}
		if f.ErrorType == "" {
			return invalid()
		}
		retained = append(retained, f)
	}
	return errors.Join(append(retained, processErr)...)
}
func validPurgeStage(stage string) bool {
	switch stage {
	case "uninstall_prepare", "uninstall_lock", "uninstall_recover", "uninstall_sessions", "uninstall_activator", "uninstall_services", "uninstall_power", "uninstall_journal", "uninstall_files", "uninstall_lock_close", "native_uninstall", "account_sweep", "machine_guard", "shared_policy", "shared_manuals", "shared_payload_parents", "final_inventory", "command":
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

// ExecutePurge retains the privileged cleanup's safe typed failure for the public owner.
func ExecutePurge(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if ctx == nil || stdin == nil || stdout == nil || stderr == nil {
		return runtimeFailure("privileged cleanup invocation is invalid")
	}
	return runPurgeCommand(ctx, nil, stdin, stdout, stderr)
}
