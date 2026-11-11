package elevation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CodeOpenSSHUnobtainable is the RemoteError code of an OpenSSH operation that failed only because this machine cannot
// supply the package (no App Installer): the typed error does not cross the elevation boundary, this code does.
const CodeOpenSSHUnobtainable = "openssh_unobtainable"

const (
	SchemaV1 = "paperboat.windows-elevation/v1"

	OperationRuntimeService = "runtime_service"
	OperationOpenSSH        = "openssh"

	ActionInstall          = "install"
	ActionInstallCommit    = "install_commit"
	ActionCommit           = "commit"
	ActionUninstall        = "uninstall"
	ActionUninstallPersist = "uninstall_persisted"
	ActionPurge            = "purge"
	ActionResetPurge       = "reset_purge"
	ActionRepair           = "repair"
	ActionStop             = "stop"
	ActionConfigInstall    = "config_install"
	ActionConfigRemove     = "config_remove"
	ActionDaemonInstall    = "daemon_install"
	ActionDaemonRemove     = "daemon_remove"
	ActionDaemonStart      = "daemon_start"
	ActionDaemonStop       = "daemon_stop"
	ActionBrowserDomain    = "browser_domain"

	ActionOpenSSHSetup   = "setup"
	ActionOpenSSHRepair  = "repair"
	ActionOpenSSHRemove  = "remove"
	RequestFlagName      = "--request-file"
	ResultFlagName       = "--result-file"
	CancelFlagName       = "--cancel-file"
	BridgeCommand        = "bridge"
	MaxRequestBytes      = 128 << 10
	MaxResultBytes       = 16 << 10
	MaxOperationDuration = 5 * time.Minute
	// Runtime activation only touches local files and SCM registrations. It
	// must not leave a one-shot installer waiting behind an opaque privileged
	// child for the general five-minute maintenance window. Measured on a
	// Windows 11 laptop with Defender on (2026-11-10, in-place update): 77 s
	// from stopping the services to all of the new ones running (each fresh
	// 99 MB executable is scanned on first start), then up to 60 s of readiness
	// checks; 90 s expired in the final health check and rolled a good update back.
	//
	// A first pair on a fresh machine then waits for the host service and the per-user daemon to settle (a
	// first scan of the new executable, the first server refresh, certificate and guard reconcile): victus and
	// every fresh hosted runner ran past the former 25 s + 30 s windows after ~100 s and needed a second attempt.
	// The two readiness windows below, the work before them (~50 s measured) and a margin must fit this budget.
	RuntimeActivationDuration = 4 * time.Minute
	// ReadinessHostdWindow is how long the host service gets to hold /healthz for the 5 s stability proof.
	ReadinessHostdWindow = 60 * time.Second
	// ReadinessDaemonBudget is how long the per-user daemon gets to report the installed version and state ready.
	ReadinessDaemonBudget = 90 * time.Second
	// ReadinessTail is the time a committed installation gets after its install phases, whatever they used of
	// RuntimeActivationDuration: victus (Defender scanning each new executable, SCM starts) spent the whole 4
	// minutes in the install phases and left nothing for the two readiness windows, so a first pair failed with
	// install_readiness while everything converged minutes later (2026-11-11).
	ReadinessTail = ReadinessHostdWindow + ReadinessDaemonBudget + 30*time.Second
	// Install may first wait for an already-approved signed update's bounded
	// recovery, then has the usual local activation budget. Other operations
	// retain their shorter deadlines.
	RuntimeInstallRecoveryDuration = 31 * time.Minute
	RuntimeInstallDuration         = RuntimeInstallRecoveryDuration + RuntimeActivationDuration
)

var (
	// ErrElevationDenied means that Windows did not start the elevated helper,
	// normally because the user denied or canceled the UAC prompt.
	ErrElevationDenied = errors.New("Windows administrator approval was denied")
	// ErrElevationRestrictedJob means the caller runs inside a job object that forbids breakaway (some IDE and CI
	// terminals), so Windows refused to start the helper outside it; this is not a refused UAC prompt.
	ErrElevationRestrictedJob = errors.New("This terminal runs inside a restricted job (common in IDEs and CI) that can't start the administrator helper. Open a normal PowerShell window and run the command again.")
	// ErrElevationCanceled means the caller canceled before the helper finished.
	ErrElevationCanceled = errors.New("elevated Windows operation canceled")
	// ErrElevationTimedOut means the bounded elevation operation exceeded its
	// deadline. The helper is canceled and terminated when possible.
	ErrElevationTimedOut = errors.New("elevated Windows operation timed out")
	// ErrElevationUnavailable means that the requested executable could not be
	// started as an elevated process.
	ErrElevationUnavailable = errors.New("elevated Windows helper unavailable")
	// ErrElevationProtocol means the helper did not return a valid protected
	// result for the request.
	ErrElevationProtocol = errors.New("invalid elevated Windows helper result")
	// ErrElevatedOperation is the stable wrapper for an operation failure that
	// occurred after UAC consent and was reported by the helper.
	ErrElevatedOperation = errors.New("elevated Windows operation failed")
	// ErrNotElevated is returned by the hidden child entrypoint if it is invoked
	// directly without a full administrator token.
	ErrNotElevated = errors.New("Windows elevated helper is not running as administrator")
	// ErrUnsupported is used by the non-Windows build of this package.
	ErrUnsupported = errors.New("Windows elevation is unavailable on this platform")
)

// Request is the only data persisted for an elevated operation. It contains
// operation metadata and a non-secret payload, never credentials, tokens, or
// private keys. The request and result paths are passed separately as opaque
// protected file names to the elevated child.
type Request struct {
	Schema     string          `json:"schema"`
	RequestID  string          `json:"request_id"`
	OwnerSID   string          `json:"owner_sid"`
	Operation  string          `json:"operation"`
	Action     string          `json:"action"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	CancelPath string          `json:"cancel_path"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
}

type Result struct {
	Schema       string `json:"schema"`
	RequestID    string `json:"request_id"`
	Status       string `json:"status"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	ErrorCause   string `json:"error_cause,omitempty"`
	ErrorErrno   int    `json:"error_errno,omitempty"`
}

// RemoteError preserves a typed elevated-operation boundary while keeping the
// returned message bounded and separate from the UAC launch errors above.
type RemoteError struct {
	Code    string
	Message string
	Cause   string
	Errno   int
}

func (e *RemoteError) DiagnosticCause() string { return e.Cause }
func (e *RemoteError) DiagnosticErrno() int    { return e.Errno }

// RuntimeInstallFailureMessage is the finite presentation contract for named
// Windows installation steps. Generic remote messages remain private.
func RuntimeInstallFailureMessage(code string) string {
	switch code {
	case "install_source":
		return "Windows installation failed while verifying the executable."
	case "install_recovery":
		return "Windows installation failed while recovering prior installation state."
	case "install_pending_daemon":
		return "Windows installation failed while stopping the pending local daemon."
	case "install_candidate":
		return "Windows installation failed while preparing the replacement executable."
	case "install_updater":
		return "Windows installation failed while preparing updater state."
	case "install_runtime":
		return "Windows installation failed while installing runtime services."
	case "install_commit":
		return "Windows installation failed while committing runtime state."
	case "install_readiness":
		return "Paperboat installed its background service but it is still starting (a first start on a new machine can take a few minutes)."
	}
	return ""
}

func (e *RemoteError) Error() string {
	if e == nil {
		return ErrElevatedOperation.Error()
	}
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = "the elevated Windows operation failed"
	}
	if e.Code == "" {
		return fmt.Sprintf("%s: %s", ErrElevatedOperation, message)
	}
	return fmt.Sprintf("%s (%s): %s", ErrElevatedOperation, e.Code, message)
}

func (*RemoteError) Unwrap() error { return ErrElevatedOperation }

// operationDuration is part of the bridge contract: the launcher and elevated
// child use the same expiry. That gives callers a bounded result even if an
// individual SCM or firewall API blocks during runtime activation.
func operationDuration(operation, action string) time.Duration {
	if operation == OperationRuntimeService && (action == ActionInstall || action == ActionInstallCommit) {
		return RuntimeInstallDuration
	}

	if operation == OperationRuntimeService && (action == ActionCommit || action == ActionUninstall || action == ActionStop || action == ActionConfigInstall || action == ActionConfigRemove || action == ActionDaemonInstall || action == ActionDaemonRemove || action == ActionDaemonStart || action == ActionDaemonStop || action == ActionBrowserDomain) {
		return RuntimeActivationDuration
	}
	return MaxOperationDuration
}
