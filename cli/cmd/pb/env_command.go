package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const environmentVariableScopePersonal = "personal"

func safeCommandFailureFor(message string, cause error) error {
	if cause == nil {
		return errors.New(message)
	}
	return envCommandError(cause, message)
}

// Only explicitly selected user input files can turn a proven missing,
// inaccessible or invalid file into an invocation error. Internal storage
// callers retain their operational failure classification.
func userInputFileError(message string, cause error) error {
	if onlyEnvironmentFailureLeaves(cause, func(leaf error) bool {
		return errors.Is(leaf, errOwnerOnlyFileInvalid) || errors.Is(leaf, os.ErrNotExist) || errors.Is(leaf, os.ErrPermission)
	}) {
		return usageError{err: cause, publicMessage: message}
	}
	return safeCommandFailureFor(message, cause)
}

type environmentCauseFrame struct {
	err      error
	inMarker bool
}

type environmentCauseInspection struct {
	valid           bool
	allLeavesMatch  bool
	allLeavesMarked bool
	hasJoin         bool
	markerHasJoin   bool
	foundMarker     bool
	leaves          int
}

// inspectEnvironmentCause bounds cause traversal and supports the few ENV
// product decisions that must distinguish a pure state from mixed failures.
func inspectEnvironmentCause(err error, matchesLeaf, isMarker func(error) bool) environmentCauseInspection {
	result := environmentCauseInspection{valid: err != nil, allLeavesMatch: true, allLeavesMarked: true}
	if err == nil {
		return result
	}
	pending := make([]environmentCauseFrame, 0, 16)
	pending = append(pending, environmentCauseFrame{err: err})
	steps := 0
	for len(pending) != 0 {
		if steps == 16 {
			result.valid = false
			return result
		}
		frame := pending[0]
		pending = pending[1:]
		steps++
		if frame.err == nil {
			result.valid = false
			return result
		}
		value := reflect.ValueOf(frame.err)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				result.valid = false
				return result
			}
		}
		if isMarker != nil && isMarker(frame.err) {
			frame.inMarker = true
			result.foundMarker = true
		}
		if joined, ok := frame.err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			result.hasJoin = result.hasJoin || len(children) > 1
			if frame.inMarker && len(children) != 1 {
				result.markerHasJoin = true
			}
			if len(children) == 0 || len(pending)+len(children) > 16-steps {
				result.valid = false
				return result
			}
			for _, child := range children {
				pending = append(pending, environmentCauseFrame{err: child, inMarker: frame.inMarker})
			}
			continue
		}
		if wrapped, ok := frame.err.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			if child != nil {
				if len(pending)+1 > 16-steps {
					result.valid = false
					return result
				}
				pending = append(pending, environmentCauseFrame{err: child, inMarker: frame.inMarker})
				continue
			}
		}
		result.leaves++
		if matchesLeaf != nil && !matchesLeaf(frame.err) {
			result.allLeavesMatch = false
		}
		if !frame.inMarker {
			result.allLeavesMarked = false
		}
	}
	return result
}

// onlyEnvironmentFailureLeaves accepts an expected product state only when
// every bounded leaf agrees. A joined filesystem or transport failure must not
// be presented as an ordinary vault state.
func onlyEnvironmentFailureLeaves(err error, matches func(error) bool) bool {
	if matches == nil {
		return false
	}
	inspection := inspectEnvironmentCause(err, matches, nil)
	return inspection.valid && inspection.leaves != 0 && inspection.allLeavesMatch
}

func environmentFailureContainsJoin(err error) bool {
	inspection := inspectEnvironmentCause(err, nil, nil)
	return !inspection.valid || inspection.hasJoin
}

func environmentAPIErrorForRecovery(err error) (*api.APIError, bool) {
	if environmentFailureContainsJoin(err) {
		return nil, false
	}
	var rejected *api.APIError
	if !errors.As(err, &rejected) {
		return nil, false
	}
	if cause := rejected.Unwrap(); cause != nil && !commandAPIStatusMetadataOnly(cause, rejected.Status) {
		return nil, false
	}
	return rejected, true
}

func environmentFailureHasMarker(err error, isMarker func(error) bool) bool {
	if isMarker == nil {
		return false
	}
	inspection := inspectEnvironmentCause(err, nil, isMarker)
	return inspection.valid && inspection.foundMarker
}

func onlyEnvironmentFailureUnderMarker(err error, isMarker func(error) bool) bool {
	if isMarker == nil {
		return false
	}
	inspection := inspectEnvironmentCause(err, nil, isMarker)
	return inspection.valid && inspection.leaves != 0 && inspection.foundMarker && inspection.allLeavesMarked && !inspection.markerHasJoin
}

func onlyEnvironmentHostRefreshConflict(err error) bool {
	return onlyEnvironmentFailureUnderMarker(err, func(cause error) bool {
		_, ok := cause.(*environmentmanager.HostRefreshConflict)
		return ok
	})
}

func onlyEnvironmentHostRotationRequired(err error) bool {
	return onlyEnvironmentFailureUnderMarker(err, func(cause error) bool {
		_, ok := cause.(*environmentmanager.HostRotationRequired)
		return ok
	})
}

type environmentVariableTarget struct {
	machineID   string
	machineName string
}

var environmentVariableBackendForCommand = backendForCommand
var environmentVariableResolveMachine = resolveUserMachine

func environmentVariablesCobraCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "env",
		Short: "Manage ENV Injection for connected hosts",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			if !environmentVariableTerminal(command) {
				return command.Help()
			}
			return runEnvironmentVariablesTUI(command)
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List configured environment-variable metadata",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			team, _ := command.Flags().GetString("team")
			machine, _ := command.Flags().GetString("machine")
			jsonOutput, _ := command.Flags().GetBool("json")
			return listEnvironmentVariablesForScope(command, team, machine, jsonOutput)
		},
	}
	list.Flags().String("team", "", "team scope; defaults to this account's personal scope")
	list.Flags().String("machine", "", "Personal override for an owned machine, even in a Team workspace; omitted uses the active workspace")
	list.Flags().Bool("json", false, "print redacted JSON metadata")

	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Set one environment variable through a hidden prompt or bounded stdin",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			team, _ := command.Flags().GetString("team")
			machine, _ := command.Flags().GetString("machine")
			valueStdin, _ := command.Flags().GetBool("value-stdin")
			valueFile, _ := command.Flags().GetString("value-file")
			jsonOutput, _ := command.Flags().GetBool("json")
			if jsonOutput && !valueStdin && strings.TrimSpace(valueFile) == "" {
				return localArgumentError("env set with --json requires --value-stdin or --value-file")
			}
			if err := setEnvironmentVariableForScope(command, team, machine, args[0], valueStdin, valueFile); err != nil {
				return err
			}
			if jsonOutput {
				return writeCLIJSON(command.OutOrStdout(), map[string]any{"name": args[0], "team": team, "machine": machine, "configured": true})
			}
			return nil
		},
	}
	set.Flags().String("team", "", "team scope; cannot be combined with --machine")
	set.Flags().String("machine", "", "Personal override for an owned machine, even in a Team workspace; omitted uses the active workspace")
	set.Flags().Bool("value-stdin", false, "read the raw value from non-interactive stdin")
	set.Flags().String("value-file", "", "read the raw value from an absolute file path")
	set.Flags().Bool("json", false, "print redacted JSON metadata")

	unset := &cobra.Command{
		Use:   "unset <name>",
		Short: "Remove one environment variable",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			team, _ := command.Flags().GetString("team")
			machine, _ := command.Flags().GetString("machine")
			if err := unsetEnvironmentVariableForScope(command, team, machine, args[0]); err != nil {
				return err
			}
			if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
				return writeCLIJSON(command.OutOrStdout(), map[string]any{"name": args[0], "team": team, "machine": machine, "configured": false})
			}
			return nil
		},
	}
	unset.Flags().String("team", "", "team scope; cannot be combined with --machine")
	unset.Flags().String("machine", "", "Personal override for an owned machine, even in a Team workspace; omitted uses the active workspace")
	unset.Flags().String("confirm", "", "six-character confirmation code from the preview")
	unset.Flags().Bool("json", false, "print redacted JSON metadata")

	root.AddCommand(list, set, unset)
	addVaultScopeCommands(root)
	addPasswordVaultCommands(root)
	return root
}

func environmentVariableTerminal(command *cobra.Command) bool {
	if input, ok := command.InOrStdin().(*os.File); ok && input != nil {
		return term.IsTerminal(int(input.Fd()))
	}
	return false
}

func environmentVariableTargetForCommand(command *cobra.Command, client *api.Client, requested string) (environmentVariableTarget, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return environmentVariableTarget{}, nil
	}
	machine, err := environmentVariableResolveMachine(command.Context(), client, requested)
	if err != nil {
		return environmentVariableTarget{}, friendlyCommandError(err)
	}
	if !machineSupportsEnvironmentInjection(machine) {
		return environmentVariableTarget{}, errors.New("ENV Injection is disabled on this machine")
	}
	return environmentVariableTarget{machineID: machine.ID, machineName: machine.Alias}, nil
}

func machineSupportsEnvironmentInjection(machine api.UserMachine) bool {
	return machine.Capabilities.EnvironmentInjection.Configured
}

func environmentVariableMachines(machines []api.UserMachine) []api.UserMachine {
	filtered := make([]api.UserMachine, 0, len(machines))
	for _, machine := range machines {
		if machineSupportsEnvironmentInjection(machine) {
			filtered = append(filtered, machine)
		}
	}
	return filtered
}

func setEnvironmentVariable(command *cobra.Command, requestedMachine, name string, valueStdin bool) error {
	return setEnvironmentVariableForScope(command, "", requestedMachine, name, valueStdin, "")
}

func unsetEnvironmentVariable(command *cobra.Command, requestedMachine, name string) error {
	return unsetEnvironmentVariableForScope(command, "", requestedMachine, name)
}

func readEnvironmentVariableValueFile(command *cobra.Command, valueStdin bool, valueFile string) ([]byte, error) {
	valueFile = strings.TrimSpace(valueFile)
	if valueStdin && valueFile != "" {
		return nil, localArgumentError("choose --value-stdin or --value-file, not both")
	}
	if valueFile != "" {
		if !filepath.IsAbs(valueFile) {
			return nil, localArgumentError("--value-file requires an absolute path")
		}
		file, err := os.Open(valueFile)
		if err != nil {
			return nil, userInputFileError("could not read --value-file; check its path and permissions", err)
		}
		value, readErr := readBoundedEnvironmentVariableStdin(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			clear(value)
			cause := errors.Join(readErr, closeErr)
			return nil, safeCommandFailureFor("could not read environment variable value file", cause)
		}
		return value, nil
	}
	input := command.InOrStdin()
	isTTY := environmentVariableTerminal(command)
	if valueStdin {
		if isTTY {
			return nil, localArgumentError("--value-stdin is only available when stdin is not a terminal")
		}
		return readBoundedEnvironmentVariableStdin(input)
	}
	if !isTTY {
		return nil, localArgumentError("set requires --value-stdin when stdin is not a terminal")
	}
	file, ok := input.(*os.File)
	if !ok || file == nil {
		return nil, localArgumentError("set requires an interactive terminal for hidden input")
	}
	return prompt.Secret(prompt.SecretOptions{Context: command.Context(),
		Title:       "Set ENV Injection variable",
		Description: "Value is hidden and can be empty",
		Placeholder: "value",
		Stdin:       file,
		Output:      command.ErrOrStderr(),
		MaxBytes:    api.MaximumEnvironmentVariableValueBytes,
	})
}

func readBoundedEnvironmentVariableStdin(input io.Reader) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(input, api.MaximumEnvironmentVariableValueBytes+1))
	if err != nil {
		clear(value)
		return nil, safeCommandFailureFor("could not read environment variable value", err)
	}
	if len(value) > api.MaximumEnvironmentVariableValueBytes {
		clear(value)
		return nil, localArgumentError("environment variable value exceeds 32767 bytes")
	}
	if bytes.IndexByte(value, 0) >= 0 {
		clear(value)
		return nil, localArgumentError("environment variable value contains NUL")
	}
	if !utf8.Valid(value) {
		clear(value)
		return nil, localArgumentError("environment variable value must be valid UTF-8")
	}
	return value, nil
}

func writeEnvironmentRecoveryFile(path string, value []byte) (resultErr error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) || len(value) == 0 || bytes.ContainsAny(value, "\x00\r\n") {
		return errors.New("ENV recovery output is invalid")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return safeCommandFailureFor("ENV recovery output directory is invalid", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("ENV recovery output directory is invalid")
	}
	file, err := createEnvironmentRecoveryFile(path)
	if err != nil {
		return safeCommandFailureFor("could not create ENV recovery-key file", err)
	}
	created := true
	defer func() {
		if file != nil {
			if err := file.Close(); err != nil {
				resultErr = errors.Join(resultErr, safeCommandFailureFor("could not close ENV recovery-key file", err))
			}
		}
		if resultErr != nil && created {
			removeErr := os.Remove(path)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, safeCommandFailureFor("could not remove incomplete ENV recovery-key file", removeErr))
			} else if syncErr := syncEnvironmentRecoveryDirectory(parent); syncErr != nil {
				resultErr = errors.Join(resultErr, safeCommandFailureFor("could not sync ENV recovery output directory after cleanup", syncErr))
			}
		}
	}()
	payload := make([]byte, 0, len(value)+1)
	payload = append(payload, value...)
	payload = append(payload, '\n')
	defer clear(payload)
	if _, err := file.Write(payload); err != nil {
		return safeCommandFailureFor("could not write ENV recovery-key file", err)
	}
	if err := file.Sync(); err != nil {
		return safeCommandFailureFor("could not sync ENV recovery-key file", err)
	}
	if err := file.Close(); err != nil {
		file = nil
		return safeCommandFailureFor("could not close ENV recovery-key file", err)
	}
	file = nil
	if err := syncEnvironmentRecoveryDirectory(parent); err != nil {
		return safeCommandFailureFor("could not sync ENV recovery output directory", err)
	}
	created = false
	return nil
}

func safeEnvironmentVariableCommandError(err error) error {
	if err == nil {
		return nil
	}
	if onlyEnvironmentFailureUnderMarker(err, func(cause error) bool { _, ok := cause.(*environmentmanager.ScopeRefreshConflict); return ok }) {
		var sourceConflict *environmentmanager.ScopeRefreshConflict
		if errors.As(err, &sourceConflict) && sourceConflict.EpochChanged {
			return envCommandError(err, "ENV source keys changed. Unlock current keys or synchronize Team grants, then submit the intended edit again; no automatic overwrite was applied.")
		}
		return envCommandError(err, "ENV source changed. Read the current source and submit the intended edit again; no automatic overwrite was applied.")
	}
	if environmentFailureHasMarker(err, func(cause error) bool { _, ok := cause.(*environmentmanager.ScopePublicationPending); return ok }) {
		if rejected, ok := environmentAPIErrorForRecovery(err); ok && rejected.Code == "team_entitlement_required" {
			return envCommandError(err, "ENV source publication remains pending. Activate Team Billing, then run `pb env vault resume` to retry the exact staged operation.")
		}
		return envCommandError(err, "ENV source publication remains unconfirmed. Run `pb env vault resume` to retry the exact staged operation before another edit.")
	}
	if environmentFailureHasMarker(err, func(cause error) bool {
		_, ok := cause.(*environmentmanager.HostPublicationPending)
		return ok
	}) {
		return &envHostRefreshFailure{cause: err, publicationPending: true}
	}
	if environmentFailureHasMarker(err, func(cause error) bool {
		_, ok := cause.(*environmentmanager.HostRotationRequired)
		return ok
	}) {
		return &envHostRefreshFailure{cause: err}
	}
	if environmentFailureHasMarker(err, func(cause error) bool {
		_, ok := cause.(*environmentmanager.HostRefreshConflict)
		return ok
	}) {
		return &envHostRefreshFailure{cause: err}
	}
	switch {
	case onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultPending) }):
		return envCommandError(err, "ENV vault publication is pending; run `pb env vault resume` before retrying")
	case onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultTeamGrantRequired) }):
		return envCommandError(err, "ENV team access needs a key grant; ask a surviving team member to regrant access, then run `pb env grants sync`")
	case onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultLocked) }):
		return envCommandError(err, "ENV vault is locked; run `pb env vault unlock` before changing variables")
	case onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVaultChanged) }):
		return envCommandError(err, "ENV vault changed; unlock the current vault and retry")
	case onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrVariableNotConfigured) }):
		return envCommandError(err, "environment variable is not configured")
	case onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrAuthorityFork) }):
		return envCommandError(err, "encrypted ENV vault history conflicts with the server; unlock the current vault and retry")
	case onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return errors.Is(leaf, environmentmanager.ErrIntegrity) }):
		return envCommandError(err, "encrypted ENV vault verification failed; no change was sent")
	}
	if apiErr, ok := environmentAPIErrorForRecovery(err); ok {
		// API mutations sanitize the server response before it reaches this
		// helper. Keep conflict recovery useful, but derive the text only from
		// the stable code so arbitrary server details can never be printed.
		switch apiErr.Code {
		case "version_conflict", "precondition_failed":
			return envCommandError(err, "environment variable scope changed; fetch it and retry")
		case "vault_conflict":
			return envCommandError(err, "ENV vault changed on the server; unlock the current vault and retry")
		case "rotation_required":
			return envCommandError(err, "ENV key rotation is required; use `pb env rotate` for personal ENV or `pb env team rotate <team>` for team ENV before retrying")
		case "key_authorization_required":
			return envCommandError(err, "ENV recipient authorization is unavailable; run `pb env grants sync` and retry")
		case "team_entitlement_required":
			return envCommandError(err, "This ENV Team source requires an active Team subscription. Activate Team Billing, then retry the source operation or run `pb env vault resume` for a pending delivery.")
		case "operation_conflict":
			return envCommandError(err, "the ENV operation conflicts with an existing operation; run `pb env vault resume` before retrying")
		}
	}
	// A submitted value must not be retained in arbitrary transport or server
	// error text, even if it was escaped or embedded in structured details.
	return envCommandError(err, "environment variable update failed")
}

func environmentVariableScopeLabel(target environmentVariableTarget) string {
	if target.machineID == "" {
		return environmentVariableScopePersonal
	}
	if target.machineName == "" {
		return target.machineID
	}
	return target.machineName + " (" + target.machineID + ")"
}

func environmentVariableConfigured(items []api.EnvironmentVariable, name string) bool {
	_, ok := environmentVariableConfiguredName(items, name)
	return ok
}

func environmentVariableConfiguredName(items []api.EnvironmentVariable, name string) (string, bool) {
	for _, item := range items {
		if strings.EqualFold(item.Name, name) && item.Configured {
			return item.Name, true
		}
	}
	return "", false
}

func runEnvironmentVariablesTUI(command *cobra.Command) error {
	client, err := environmentVariableBackendForCommand(command)
	if err != nil {
		return err
	}
	endScreen := selector.BeginScreen(command.ErrOrStderr())
	defer endScreen()
	personal := *client
	if err := personal.SetWorkspace("personal"); err != nil {
		return err
	}
	machines, err := personal.ListUserMachines(command.Context())
	if err != nil {
		return friendlyCommandError(err)
	}
	workspace, err := effectiveWorkspace(command)
	if err != nil {
		return err
	}
	items := environmentVariableScopePickerItemsFor(machines, workspace)
	for {
		selection, selectErr := selector.Choose(selector.Options{Context: command.Context(),
			Title:    "ENV Injection",
			Subtitle: "Choose a scope",
			Items:    items,
			Empty:    "No machine scopes are available",
			Footer:   "↑/↓ move  type to filter  enter open  esc exit",
			Stdin:    os.Stdin,
			Output:   command.ErrOrStderr(),
		})
		if selectErr != nil {
			return selectErr
		}
		target := environmentVariableTarget{}
		if selection.ID != "workspace" {
			for _, machine := range machines {
				if machine.ID == selection.ID {
					target = environmentVariableTarget{machineID: machine.ID, machineName: machine.Alias}
					break
				}
			}
		}
		if err := runEnvironmentVariableScopeTUI(command, client, target); err != nil && !errors.Is(err, selector.ErrCanceled) {
			return err
		}
	}
}

func environmentVariableScopePickerItems(machines []api.UserMachine) []selector.Item {
	return environmentVariableScopePickerItemsFor(machines, "personal")
}

func environmentVariableScopePickerItemsFor(machines []api.UserMachine, workspace string) []selector.Item {
	name, description, search := "Personal", "Personal encrypted values; provision explicit host selections", "account personal encrypted"
	if workspace != "personal" {
		name, description, search = workspace, "Team encrypted values; provision explicit host selections", workspace+" team encrypted"
	}
	items := []selector.Item{{ID: "workspace", Title: name, Description: description, Search: search}}
	for _, machine := range environmentVariableMachines(machines) {
		items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: "Personal machine override  ·  " + machineStatusSummary(machine), Search: machine.ID + " " + machine.Alias + " personal override"})
	}
	return items
}

func runEnvironmentVariableScopeTUI(command *cobra.Command, _ *api.Client, target environmentVariableTarget) error {
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	workspace, err := effectiveWorkspace(command)
	if err != nil {
		return err
	}
	scope := vaultScopeTarget{kind: "personal", owner: manager.AccountID, machine: target.machineID, label: "personal " + environmentVariableScopeLabel(target)}
	if workspace != "personal" && target.machineID == "" {
		scope.kind = "team"
		scope.owner = workspace
		scope.label = "team " + workspace
		if target.machineID != "" {
			scope.label += " machine " + environmentVariableScopeLabel(target)
		}
	}
	for {
		metadata, err := readVaultScopeMetadata(command.Context(), manager, scope)
		if err != nil {
			return safeEnvironmentVariableCommandError(err)
		}
		items := []selector.Item{{ID: "set", Title: "Set variable", Description: "Add or replace a variable with hidden input", Search: "add update"}}
		for _, name := range metadata.Names {
			items = append(items, selector.Item{ID: "unset:" + name, Title: name, Description: "configured  ·  revision " + fmt.Sprint(metadata.Revision), Search: name + " remove unset"})
		}
		selection, selectErr := selector.Choose(selector.Options{Context: command.Context(),
			Title:    "ENV Injection",
			Subtitle: scope.label + "  ·  scope revision " + fmt.Sprint(metadata.Revision),
			Items:    items,
			Empty:    "No variables configured",
			Footer:   "↑/↓ move  enter select  esc back",
			Stdin:    os.Stdin,
			Output:   command.ErrOrStderr(),
		})
		if selectErr != nil {
			return selectErr
		}
		if selection.ID == "set" {
			name, promptErr := prompt.Text(prompt.TextOptions{Context: command.Context(),
				Title:       "Variable name",
				Description: "Letters, numbers, and underscores; values stay hidden",
				Placeholder: "NAME",
				Stdin:       os.Stdin,
				Output:      command.ErrOrStderr(),
				Validate: func(value string) error {
					return validateEnvironmentVariableNameForCLI(value)
				},
			})
			if errors.Is(promptErr, prompt.ErrCanceled) {
				continue
			}
			if promptErr != nil {
				return promptErr
			}
			value, valueErr := prompt.Secret(prompt.SecretOptions{Context: command.Context(), Title: "Variable value", Description: "The value is hidden; press Enter to store an empty value", Stdin: os.Stdin, Output: command.ErrOrStderr(), MaxBytes: api.MaximumEnvironmentVariableValueBytes})
			if errors.Is(valueErr, prompt.ErrCanceled) {
				continue
			}
			if valueErr != nil {
				return valueErr
			}
			setErr := manager.MutateScope(command.Context(), scope.kind, scope.owner, scope.machine, func(values map[string][]byte) error {
				canonical, exists := vaultScopeConfiguredName(values, name)
				if exists {
					clear(values[canonical])
					values[canonical] = append([]byte(nil), value...)
					return nil
				}
				values[name] = append([]byte(nil), value...)
				return nil
			})
			clear(value)
			if setErr != nil {
				return safeEnvironmentVariableCommandError(setErr)
			}
			if err := refreshSelectedENVHosts(command, manager, true); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.ErrOrStderr(), "Set %s on %s (encrypted vault scope). Encrypted deliveries for previously selected recipients were refreshed.\n", name, scope.label)
			continue
		}
		name := strings.TrimPrefix(selection.ID, "unset:")
		confirmed, confirmErr := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Unset " + name + "?", Description: "Remove this value from the encrypted scope and refresh previously selected recipients. Running processes retain their existing environment.", Stdin: os.Stdin, Output: command.ErrOrStderr()})
		if errors.Is(confirmErr, prompt.ErrCanceled) || confirmErr != nil {
			if confirmErr != nil && !errors.Is(confirmErr, prompt.ErrCanceled) {
				return confirmErr
			}
			continue
		}
		if !confirmed {
			continue
		}
		deleteErr := manager.MutateScope(command.Context(), scope.kind, scope.owner, scope.machine, func(values map[string][]byte) error {
			canonical, exists := vaultScopeConfiguredName(values, name)
			if !exists {
				return environmentmanager.ErrVariableNotConfigured
			}
			clear(values[canonical])
			delete(values, canonical)
			return nil
		})
		if deleteErr != nil {
			return safeEnvironmentVariableCommandError(deleteErr)
		}
		if err := refreshSelectedENVHosts(command, manager, true); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(command.ErrOrStderr(), "Unset %s from %s (encrypted vault scope). Encrypted deliveries for previously selected recipients were refreshed.\n", name, scope.label)
	}
}

func validateEnvironmentVariableNameForCLI(name string) error {
	if name == "" || len(name) > api.MaximumEnvironmentVariableNameBytes {
		return errors.New("environment variable name must be 1-128 characters")
	}
	for index, char := range name {
		if index == 0 && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && char != '_' {
			return errors.New("environment variable name must start with a letter or underscore")
		}
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return errors.New("environment variable name may contain only letters, numbers, and underscores")
		}
	}
	upperName := strings.ToUpper(name)
	if strings.HasPrefix(upperName, "PAPERBOAT_") || strings.HasPrefix(upperName, "LD_") || strings.HasPrefix(upperName, "DYLD_") || upperName == "NODE_OPTIONS" || upperName == "PYTHONPATH" || upperName == "PYTHONHOME" || upperName == "GOTRACEBACK" {
		return errors.New("environment variable name is reserved")
	}
	return nil
}
