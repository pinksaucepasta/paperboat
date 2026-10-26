package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const cliJSONSchemaVersion = "1.0"

const teamSubscriptionRecovery = "Ask a team owner or admin to manage team billing, or retry with --workspace personal."

type cliJSONError struct {
	Code             string `json:"code"`
	Stage            string `json:"stage,omitempty"`
	Category         string `json:"category"`
	Message          string `json:"message"`
	Target           string `json:"target,omitempty"`
	Retryable        bool   `json:"retryable"`
	StateChanged     any    `json:"state_changed"`
	OutcomeUncertain bool   `json:"outcome_uncertain"`
	SupportReference string `json:"support_reference,omitempty"`
	Recovery         string `json:"recovery,omitempty"`
}

type cliJSONEnvelope struct {
	SchemaVersion string        `json:"schema_version"`
	OK            bool          `json:"ok"`
	Data          any           `json:"data,omitempty"`
	Error         *cliJSONError `json:"error,omitempty"`
}

type unsupportedJSONOutputError struct{ command string }

type jsonFailureExitCodeError struct {
	code    int
	message string
}

func (e jsonFailureExitCodeError) Error() string { return e.message }
func (e jsonFailureExitCodeError) ExitCode() int { return e.code }

// boundedTailBuffer retains only the final bytes of native command output.
type boundedTailBuffer struct{ data []byte }

const boundedTailBufferSize = 4 << 10

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if extra := len(b.data) - boundedTailBufferSize; extra > 0 {
		b.data = append(b.data[:0], b.data[extra:]...)
	}
	return len(p), nil
}

func (b *boundedTailBuffer) String() string { return string(b.data) }

// runtimeFailureLine returns the last curated "pb: " line written by a native
// runtime command, or fallback when none was written.
func runtimeFailureLine(output, fallback string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if message, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "pb: "); ok && message != "" {
			return message
		}
	}
	return fallback
}

func (e unsupportedJSONOutputError) Error() string {
	return "--json is not supported for " + e.command
}
func (e unsupportedJSONOutputError) Unwrap() error { return errUsage }

func writeCLIJSON(writer io.Writer, data any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(cliJSONEnvelope{SchemaVersion: cliJSONSchemaVersion, OK: true, Data: data})
}

func writeCLIJSONError(writer io.Writer, err error) error {
	return writeCLIJSONErrorWithReference(writer, err, "")
}

func writeCLIJSONErrorWithReference(writer io.Writer, err error, reference string) error {
	value := classifyCLIJSONError(err)
	if value.SupportReference == "" && supportref.Valid(reference) {
		value.SupportReference = reference
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(cliJSONEnvelope{SchemaVersion: cliJSONSchemaVersion, OK: false, Error: &value})
}

func classifyCLIJSONError(err error) cliJSONError {
	return classifyCLIJSONFailure(err, classifyCommandFailure(err))
}

func classifyCLIJSONFailure(err error, failure commandFailure) cliJSONError {
	result := cliJSONError{Code: "operation_failed", Category: "local_io", Message: boundedCLIJSONMessage(commandFailureMessage(err, failure)), StateChanged: "unknown", SupportReference: failure.supportReference}
	if _, ok := soleCommandOwner(err).(uninstallCleanupError); ok {
		fault := errorreport.ProjectFault(context.Background(), "paperboat-cli", "uninstall", "command", "command_failed", err)
		if strings.HasPrefix(fault.Code, "purge_") && fault.Stage != "unknown" {
			result.Code, result.Stage = fault.Code, fault.Stage
		}
	}
	if result.Message == "" {
		result.Message = "The operation failed."
	}
	if updaterFailure := updaterControlFailure(err); updaterFailure != nil {
		result = classifyUpdaterControlJSONError(result, updaterFailure)
	}
	owner := soleCommandOwner(err)
	switch typed := owner.(type) {
	case *envCommandFailure:
		if failure.kind != commandOperational && failure.kind != commandRejected {
			break
		}
		result.Code = "env_operation_failed"
		result.Category, result.Retryable = "unavailable_retryable", true
		result.Recovery = typed.message
		if failure.kind == commandRejected {
			result.Category, result.Retryable = "conflict", false
		}
		if environmentFailureHasMarker(typed.cause, func(cause error) bool { _, ok := cause.(*environmentmanager.ScopePublicationPending); return ok }) {
			result.Code = "env_source_publication_pending"
			result.OutcomeUncertain = true
		}
		sourceConflict := onlyEnvironmentFailureUnderMarker(typed.cause, func(cause error) bool { _, ok := cause.(*environmentmanager.ScopeRefreshConflict); return ok })
		if sourceConflict {
			result.Code = "env_source_conflict"
			result.Category = "conflict"
			result.StateChanged = false
			result.OutcomeUncertain = false
		}
		if typed.cause == environmentmanager.ErrVaultNotSetUp {
			result.Code, result.Category, result.StateChanged = "env_not_set_up", "conflict", false
			result.Recovery = "Run `pb env vault unlock` if this account already uses ENV, or `pb env vault init` to set it up."
		}
		if onlyEnvironmentFailureLeaves(typed.cause, func(leaf error) bool {
			return leaf == environmentmanager.ErrVaultLocked || leaf == environmentmanager.ErrVaultPending || leaf == environmentmanager.ErrVaultChanged || leaf == environmentmanager.ErrVaultTeamGrantRequired || leaf == environmentmanager.ErrVariableNotConfigured
		}) {
			result.StateChanged = false
		}
		if rejected, ok := failure.owner.(*api.APIError); ok && !sourceConflict && !failure.apiCauseInvalid && !environmentFailureContainsJoin(typed.cause) {
			switch rejected.Code {
			case "team_entitlement_required":
				result.Code, result.Category, result.Retryable = rejected.Code, "authorization_or_entitlement", false
			case "version_conflict", "precondition_failed", "vault_conflict", "operation_conflict":
				result.Code, result.Category = rejected.Code, "conflict"
			case "rotation_required", "key_authorization_required":
				result.Code = rejected.Code
			}
		}
	case *envHostRefreshFailure:
		result.Code, result.Category, result.StateChanged = "env_recipient_refresh_pending", "unavailable_retryable", "unknown"
		result.Retryable = true
		result.Recovery = typed.recovery()
		changed := typed.sourceChanged || typed.operationCompleted || typed.completed > 0
		result.OutcomeUncertain = typed.publicationPending || failure.kind == commandUnexpected || !changed
		if changed {
			result.StateChanged = true
		}
	case updateCompletionFailure:
		result.Code, result.Category, result.StateChanged = "update_installed_unverified", "local_io", true
		result.Recovery = "Run pb doctor to confirm the new version is healthy; do not repeat the update."
	case *configComparisonFailure:
		result.Code, result.Category, result.StateChanged = "config_comparison_unavailable", "unavailable_retryable", false
		result.Retryable = true
		result.Recovery = "Refresh pb config status and retry the exact conflict comparison."
	case *api.APIError:
		if failure.owner != typed || failure.apiCauseInvalid {
			break
		}
		result.Code = typed.PublicCode()
		if supportref.Valid(typed.SupportReference) {
			result.SupportReference = typed.SupportReference
		}
		switch typed.Status {
		case 401:
			result.Category = "authentication"
		case 403:
			result.Category = "authorization_or_entitlement"
		case 409, 412:
			result.Category = "conflict"
		case 408, 429, 500, 502, 503, 504:
			result.Category, result.Retryable = "unavailable_retryable", true
		}
		if typed.Code == "machine_offline" || typed.Code == "machine_capability_unavailable" || typed.Code == "team_suspended" || strings.HasPrefix(typed.Code, "config_compare_") {
			// The target was refused before anything was sent or started.
			result.StateChanged = false
		}
		if typed.Status == 403 && typed.Code == "team_subscription_required" {
			result.StateChanged = false
			result.Recovery = teamSubscriptionRecovery
		}
		if typed.Status == 426 && typed.Code == "update_required" {
			result.Category, result.StateChanged = "conflict", false
			result.Message = typed.Error()
			result.Recovery = "Run `pb update` to install a supported official release."
		}
	case updateOpenConnectionsRefusal:
		// The updater refused before changing anything.
		result.Code, result.Category, result.StateChanged, result.Retryable = "update_busy", "conflict", false, true
		result.Recovery = "Close the SSH connections and port forwards or wait for them to finish, then retry; `pb update --force` ends them."
	case destinationBusyError:
		result.Code, result.Category, result.StateChanged, result.Retryable = "destination_busy", "conflict", false, true
		result.Recovery = "Let the destination's unfinished transfers finish or cancel them (`pb send list --on <machine>`, `pb send cancel <transfer-id> --on <machine>`), then send again."
	case destinationStorageFullError:
		// Nothing was delivered; the partial upload is discarded by the receiver.
		result.Code, result.Category, result.StateChanged, result.Retryable = "destination_storage_full", "conflict", false, false
		result.Recovery = "Free space on the destination or move its Inbox with `pb inbox set`, then send again."
	case *filetransfer.LimitError:
		// The batch was refused before anything was sent.
		result.Code, result.Category, result.StateChanged = "file_transfer_limit_exceeded", "conflict", false
		result.Recovery = "Send fewer or smaller files."
	case *localapi.RemoteError:
		if typed.Code == localapi.TokenScopeCode {
			result.Code, result.Category, result.StateChanged, result.Retryable = localapi.TokenScopeCode, "authorization_or_entitlement", false, false
			result.Recovery = "Use an API token whose scope includes this machine and action, or sign in with `pb login`."
		}
		if typed.Code == localapi.RelayQuotaCode {
			// The connection was refused before anything was sent or started.
			result.Code, result.Category, result.StateChanged = localapi.RelayQuotaCode, "authorization_or_entitlement", false
			result.Recovery = "Add bandwidth or upgrade under Billing in the Paperboat console, or wait for the next allowance period."
		}
	case commandRejection:
		if failure.kind == commandRejected && typed.valid() {
			result.Code, result.Category, result.Message = "operation_refused", "conflict", typed.Error()
			if typed.reason == commandRejectUpdateVerification {
				result.Code, result.StateChanged, result.Retryable = "update_verification_failed", false, false
				result.Recovery = "Download a fresh official update bundle and retry. Force cannot bypass verification."
			}
			if typed.reason == commandRejectUpdateNotReady {
				result.Code, result.StateChanged, result.Retryable = "update_rolled_back", false, true
				result.Recovery = "Run `pb doctor` to see why the background service cannot start, then try the update again."
			}
			if typed.reason == commandRejectInsufficientSpace {
				result.Code, result.StateChanged, result.Retryable = "insufficient_disk_space", false, false
				result.Recovery = "Free disk space on the disk that holds the Paperboat installation, then retry."
			}
			if typed.reason == commandRejectUpdateBusy {
				result.Code, result.StateChanged, result.Retryable = "update_busy", false, true
				result.Recovery = "Wait for active work or another update to finish, then retry."
			}
			if typed.cause == nil {
				result.StateChanged = false
			}
			var inspectorErr *inspectorDaemonError
			if typed.reason == commandRejectInspector && errors.As(typed.cause, &inspectorErr) {
				// Every daemon refusal happens before a replay is sent, except an ambiguous replay.
				result.Code, result.StateChanged = inspectorErr.Code, false
				if inspectorErr.Code == "inspector_replay_ambiguous" {
					result.StateChanged, result.OutcomeUncertain = "unknown", true
				}
			}
			var approvalRefusal *daemoncmd.ApprovalRefusal
			if typed.reason == commandRejectApproval && errors.As(typed.cause, &approvalRefusal) {
				// The daemon checks ownership and the pending request before it signs anything.
				result.Code, result.StateChanged, result.Target = approvalRefusal.Code, false, approvalRefusal.Machine
				if approvalRefusal.Code == localdaemon.RefusalSignerUnavailable {
					result.Category = "authorization_or_entitlement"
				}
				result.Retryable = approvalRefusal.Code == localdaemon.RefusalApprovalAmbiguous
			}
			if typed.reason == commandRejectInboxDeclined {
				// The request was created and declined; file delivery never began.
				result.StateChanged = true
			}
			if typed.reason == commandRejectActivationPending {
				result.Code, result.StateChanged, result.Retryable = "activation_pending", "unknown", true
				result.Recovery = "Wait for the signed update service to finish recovery, then retry; run pb doctor if this continues."
			}
			if typed.reason == commandRejectLegacyCarrierRunning {
				result.Code, result.Category, result.StateChanged, result.Retryable = "initial_update_requires_stop", "conflict", "unknown", true
				result.Recovery = "Finish active sessions and SSH connections, stop the existing native Paperboat service, then retry. Run pb doctor to inspect the recovered installation."
			}
		}
	case unsupportedJSONOutputError:
		result.Code, result.Category, result.StateChanged = "unsupported_output", "usage", false
		result.Recovery = "Run this command without --json."
	case *TunnelCreateExistingError:
		result.Code, result.Category, result.StateChanged = "tunnel_name_exists", "conflict", false
		result.Recovery = "Run pb tunnel status <tunnel> to inspect the existing tunnel."
	case *TunnelCreateChangedError:
		result.Code, result.Category, result.StateChanged = "tunnel_create_partial", "conflict", true
		result.OutcomeUncertain = typed.OutcomeUncertain
		if safeID := safeTunnelCreateTarget(typed.TunnelID); safeID != "" {
			result.Target = safeID
			if recovery := safeTunnelCreateRecoveryCommand(safeID, typed.RecoveryCommand); recovery != "" {
				result.Recovery = "Run `" + recovery + "`."
			} else {
				result.Recovery = "Run `pb tunnel status " + safeID + "` to inspect the preserved tunnel before retrying."
			}
		} else {
			result.Recovery = "Inspect the preserved tunnel using its ID from the command output before retrying."
		}
	case *TunnelOperationOutcomeError:
		result.Code, result.Category, result.StateChanged = "tunnel_operation_failed", "conflict", true
	case *TunnelOperationWaitTimeoutError:
		result.Code, result.Category, result.StateChanged = "tunnel_operation_wait_timeout", "unavailable_retryable", true
		result.Retryable, result.OutcomeUncertain = true, true
	}
	switch failure.kind {
	case commandUsage:
		if result.Code == "operation_failed" {
			result.Code, result.Category, result.StateChanged = "invalid_invocation", "usage", false
		}
	case commandDeadline:
		if result.Code == "operation_failed" {
			result.Code, result.Category, result.Retryable = "operation_timeout", "unavailable_retryable", true
		}
	case commandCanceled, commandInteractiveCanceled:
		result.Code, result.Category = "operation_canceled", "canceled"
	case commandRejected:
		switch failure.owner {
		case api.ErrUnauthenticated, config.ErrSecretNotFound:
			result.Code, result.Category = "authentication_required", "authentication"
		case preferences.ErrChanged:
			if owner == preferences.ErrChanged {
				result.Code, result.Category, result.StateChanged = "preferences_changed", "conflict", false
				result.Recovery = "Reload the preference file and reapply your changes; no changes were saved."
			}
		case preferences.ErrBusy:
			if owner == preferences.ErrBusy {
				result.Code, result.Category, result.StateChanged = "preferences_busy", "conflict", false
				result.Retryable = true
				result.Recovery = "Another process is saving preferences. Retry after it finishes."
			}
		}
	}
	var lost *api.ResponseLostError
	if failure.kind == commandOperational && errors.As(err, &lost) {
		// The change was sent; only its answer is missing, so it may have been applied.
		result.Code, result.Category, result.StateChanged = "response_lost", "unavailable_retryable", "unknown"
		result.Retryable, result.OutcomeUncertain = false, true
		result.Recovery = responseLostRecovery
	}
	return result
}

func safeTunnelCreateTarget(value string) string {
	if !safeTunnelCreateIdentifier(value, "tunnel_", "tun_") {
		return ""
	}
	return value
}

func safeTunnelCreateIdentifier(value string, prefixes ...string) bool {
	if !validTunnelCLIResourceID(value) || value != strings.ToLower(value) {
		return false
	}
	knownPrefix := false
	for _, prefix := range prefixes {
		knownPrefix = knownPrefix || strings.HasPrefix(value, prefix)
	}
	if !knownPrefix {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func safeTunnelCreateRecoveryCommand(tunnelID, recovery string) string {
	if tunnelID == "" || len(recovery) > 240 {
		return ""
	}
	parts := strings.Fields(recovery)
	if len(parts) < 4 || strings.Join(parts, " ") != recovery || parts[0] != "pb" || parts[1] != "tunnel" {
		return ""
	}
	switch {
	case len(parts) == 4 && (parts[2] == "show" || parts[2] == "status") && parts[3] == tunnelID:
		return recovery
	case len(parts) == 5 && parts[2] == "connector" && parts[3] == "add" && parts[4] == tunnelID:
		return recovery
	case len(parts) == 8 && parts[2] == "domain" && parts[3] == "add" && parts[4] == tunnelID && validTunnelCLIHostname(parts[5]) && parts[6] == "--route" && safeTunnelCreateIdentifier(parts[7], "route_"):
		return recovery
	case len(parts) == 6 && parts[2] == "domain" && (parts[3] == "instructions" || parts[3] == "verify") && parts[4] == tunnelID && validTunnelCLIHostname(parts[5]):
		return recovery
	case len(parts) == 5 && parts[2] == "route" && parts[3] == "list" && parts[4] == tunnelID:
		return recovery
	default:
		return ""
	}
}

// Match updater diagnostics without following an unbounded or cyclic cause tree.
func updaterControlFailure(err error) (failure *updated.ControlError) {
	defer func() {
		if recover() != nil {
			failure = nil
		}
	}()
	pending := []error{err}
	for visited := 0; len(pending) > 0 && visited < 32; visited++ {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			continue
		}
		if control, ok := current.(*updated.ControlError); ok {
			return control
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(pending)+len(children) > 32-visited {
				return nil
			}
			pending = append(slices.Clone(children), pending...)
		case interface{ Unwrap() error }:
			pending = append([]error{wrapped.Unwrap()}, pending...)
		}
	}
	return nil
}

func classifyUpdaterControlJSONError(result cliJSONError, failure *updated.ControlError) cliJSONError {
	if failure == nil {
		return result
	}
	result.Code = failure.DiagnosticCode()
	result.Stage = failure.DiagnosticStage()
	result.Category = "local_io"
	result.Retryable = false
	result.Message = "The local updater request failed."
	result.Recovery = "Run `pb update status` and check the support reference before retrying."

	switch failure.Code {
	case "invalid_request":
		result.Category = "protocol_incompatible"
		result.StateChanged = false
		result.Message = "The local updater rejected the request."
		result.Recovery = "Update the CLI and managed updater together before retrying."
	case "custom_installation":
		result.Category = "conflict"
		result.StateChanged = false
		result.Message = "Official updates are unavailable for this custom installation."
		result.Recovery = "Run pb update --force, or install an official bundle with pb update --from <file.pb-update>."
	case "approval_required":
		result.Category = "conflict"
		result.StateChanged = false
		result.Message = "The update requires explicit approval before installation."
		result.Recovery = "Run `pb update` to review the signed candidate, then approve its exact ID."
	case "candidate_changed":
		result.Category = "conflict"
		result.StateChanged = false
		result.Message = "The signed update candidate changed before installation."
		result.Recovery = "Run `pb update` again to download and review the current candidate."
	case "active_terminal_sessions":
		result.Category = "conflict"
		result.Message = "The update is blocked while terminal sessions are active."
		result.Recovery = "Close the active sessions or wait for them to finish, then check `pb update status`."

	case "activation_unavailable":
		result.Category, result.Retryable = "unavailable_retryable", true
		result.Message = "The local updater is unavailable or recovering."
		result.Recovery = "Run `pb update status` and retry after the updater is ready."
	case "recovery_required":
		result.Category = "conflict"
		result.Message = "The local updater requires recovery before continuing."
		result.Recovery = "Run `pb update status` and follow its recovery state before retrying."
	case "release_not_found":
		result.Category = "conflict"
		result.Message = "No eligible signed release was found or validated."
		result.Recovery = "Run `pb update check` and retry when an eligible signed release is available."
	case "update_failed", "check_failed":
		result.Category, result.Retryable = "unavailable_retryable", true
		result.Message = updaterControlFailureMessage(result.Stage)
		result.Recovery = updaterControlRecovery(result.Stage)
	default:
		result.Code = "control_request_failed"
	}
	if result.Stage == "update_install" && result.Category == "unavailable_retryable" {
		result.OutcomeUncertain = true
	}
	return result
}

func updaterControlFailureMessage(stage string) string {
	switch stage {
	case "update_check":
		return "The local updater could not check signed release metadata."
	case "update_download":
		return "The local updater could not verify or download the signed update."
	case "update_install":
		return "The local updater could not complete the approved update."
	default:
		return "The local updater request failed."
	}
}

func updaterControlRecovery(stage string) string {
	switch stage {
	case "update_check":
		return "Run `pb update status` before retrying the check."
	case "update_download":
		return "Run `pb update status` to inspect the candidate before retrying the download."
	case "update_install":
		return "Run `pb update status` and follow any recovery state before retrying."
	default:
		return "Run `pb update status` and check the support reference before retrying."
	}
}

func boundedCLIJSONMessage(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	characters := []rune(value)
	if len(characters) > 240 {
		value = string(characters[:237]) + "..."
	}
	return value
}

func jsonArgumentRequested(args []string) bool {
	requested := false
	for _, argument := range args {
		if argument == "--" {
			break
		}
		switch {
		case argument == "--json", argument == "--json=true":
			requested = true
		case argument == "--json=false":
			requested = false
		}
	}
	return requested
}

func jsonOutputRequested(command *cobra.Command) bool {
	for current := command; current != nil; current = current.Parent() {
		if flag := current.Flags().Lookup("json"); flag != nil && flag.Changed {
			value, _ := current.Flags().GetBool("json")
			return value
		}
		if flag := current.PersistentFlags().Lookup("json"); flag != nil && flag.Changed {
			value, _ := current.PersistentFlags().GetBool("json")
			return value
		}
	}
	return false
}

func prepareJSONCommand(command *cobra.Command) error {
	if !jsonOutputRequested(command) {
		return nil
	}
	if command == command.Root() {
		return nil
	}
	if local := command.LocalNonPersistentFlags().Lookup("json"); local != nil {
		_ = local.Value.Set("true")
		local.Changed = true
		return nil
	}
	if command.HasSubCommands() && command.Name() != "ssh" {
		if err := writeCLIJSON(command.OutOrStdout(), map[string]any{"command": command.CommandPath(), "commands": cliCommandCatalog(command)}); err != nil {
			return err
		}
		return exitCodeError{code: 0}
	}
	return unsupportedJSONOutputError{command: strings.TrimSpace(command.CommandPath())}
}

type cliCommandCatalogEntry struct {
	Command string `json:"command"`
	JSON    bool   `json:"json"`
}

func newCLIHelpCommand(root *cobra.Command) *cobra.Command {
	command := &cobra.Command{
		Use: "help [command]", Short: "Show help for a command", Args: cobra.ArbitraryArgs,
		RunE: func(command *cobra.Command, args []string) error {
			target, remaining, err := root.Find(args)
			if err != nil {
				return invocationError(err)
			}
			if len(remaining) != 0 {
				return invocationError(fmt.Errorf("unknown help command %q", strings.Join(args, " ")))
			}
			if jsonOutputRequested(command) {
				return writeCLIJSON(command.OutOrStdout(), cliCommandHelp(root, args))
			}
			return target.Help()
		},
	}
	command.Flags().Bool("json", false, "print machine-readable JSON")
	return command
}

type cliCommandHelpFlag struct {
	Name        string `json:"name"`
	Shorthand   string `json:"shorthand,omitempty"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

type cliCommandHelpOutput struct {
	Command     string               `json:"command"`
	Usage       string               `json:"usage"`
	Summary     string               `json:"summary"`
	Description string               `json:"description,omitempty"`
	Flags       []cliCommandHelpFlag `json:"flags"`
}

func cliCommandHelp(root *cobra.Command, args []string) cliCommandHelpOutput {
	findArgs := args
	if dash := slices.Index(findArgs, "--"); dash >= 0 {
		findArgs = findArgs[:dash]
	}
	command, _, err := root.Find(findArgs)
	if err != nil || command == nil {
		command = root
	}
	result := cliCommandHelpOutput{Command: command.CommandPath(), Usage: command.UseLine(), Summary: command.Short, Description: command.Long, Flags: []cliCommandHelpFlag{}}
	seen := make(map[string]struct{})
	appendFlags := func(flags *pflag.FlagSet) {
		flags.VisitAll(func(flag *pflag.Flag) {
			if flag.Hidden {
				return
			}
			if _, ok := seen[flag.Name]; ok {
				return
			}
			seen[flag.Name] = struct{}{}
			_, required := flag.Annotations[cobra.BashCompOneRequiredFlag]
			result.Flags = append(result.Flags, cliCommandHelpFlag{Name: flag.Name, Shorthand: flag.Shorthand, Type: flag.Value.Type(), Description: flag.Usage, Required: required})
		})
	}
	appendFlags(command.NonInheritedFlags())
	appendFlags(command.InheritedFlags())
	return result
}

func cliCommandCatalog(root *cobra.Command) []cliCommandCatalogEntry {
	entries := make([]cliCommandCatalogEntry, 0)
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		for _, child := range command.Commands() {
			if child.Hidden {
				continue
			}
			if child.Runnable() {
				jsonSupported := child.LocalNonPersistentFlags().Lookup("json") != nil || child.HasSubCommands() && child.Name() != "ssh"
				entries = append(entries, cliCommandCatalogEntry{Command: child.CommandPath(), JSON: jsonSupported})
			}
			visit(child)
		}
	}
	visit(root)
	return entries
}
