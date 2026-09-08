package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	list.Flags().String("machine", "", "machine name or ID; defaults to the personal scope")
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
			return setEnvironmentVariableForScope(command, team, machine, args[0], valueStdin, valueFile)
		},
	}
	set.Flags().String("team", "", "team scope; cannot be combined with --machine")
	set.Flags().String("machine", "", "machine name or ID; defaults to the personal scope")
	set.Flags().Bool("value-stdin", false, "read the raw value from non-interactive stdin")
	set.Flags().String("value-file", "", "read the raw value from an absolute file path")

	unset := &cobra.Command{
		Use:   "unset <name>",
		Short: "Remove one environment variable",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			team, _ := command.Flags().GetString("team")
			machine, _ := command.Flags().GetString("machine")
			yes, _ := command.Flags().GetBool("yes")
			return unsetEnvironmentVariableForScope(command, team, machine, args[0], yes)
		},
	}
	unset.Flags().String("team", "", "team scope; cannot be combined with --machine")
	unset.Flags().String("machine", "", "machine name or ID; defaults to the personal scope")
	unset.Flags().Bool("yes", false, "confirm removal")

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
		return environmentVariableTarget{}, errors.New("ENV Injection is disabled on this device")
	}
	return environmentVariableTarget{machineID: machine.ID, machineName: machine.DisplayName}, nil
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

func unsetEnvironmentVariable(command *cobra.Command, requestedMachine, name string, yes bool) error {
	return unsetEnvironmentVariableForScope(command, "", requestedMachine, name, yes)
}

func readEnvironmentVariableValue(command *cobra.Command, valueStdin bool) ([]byte, error) {
	return readEnvironmentVariableValueFile(command, valueStdin, "")
}

func readEnvironmentVariableValueFile(command *cobra.Command, valueStdin bool, valueFile string) ([]byte, error) {
	valueFile = strings.TrimSpace(valueFile)
	if valueStdin && valueFile != "" {
		return nil, invocationError(errors.New("choose --value-stdin or --value-file, not both"))
	}
	if valueFile != "" {
		if !filepath.IsAbs(valueFile) {
			return nil, invocationError(errors.New("--value-file requires an absolute path"))
		}
		file, err := os.Open(valueFile)
		if err != nil {
			return nil, errors.New("could not read environment variable value file")
		}
		defer file.Close()
		return readBoundedEnvironmentVariableStdin(file)
	}
	input := command.InOrStdin()
	isTTY := environmentVariableTerminal(command)
	if valueStdin {
		if isTTY {
			return nil, errors.New("--value-stdin is only available when stdin is not a terminal")
		}
		return readBoundedEnvironmentVariableStdin(input)
	}
	if !isTTY {
		return nil, errors.New("set requires --value-stdin when stdin is not a terminal")
	}
	file, ok := input.(*os.File)
	if !ok || file == nil {
		return nil, errors.New("set requires an interactive terminal for hidden input")
	}
	return prompt.Secret(prompt.SecretOptions{
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
		return nil, errors.New("could not read environment variable value")
	}
	if len(value) > api.MaximumEnvironmentVariableValueBytes {
		clear(value)
		return nil, errors.New("environment variable value exceeds 32767 bytes")
	}
	if bytes.IndexByte(value, 0) >= 0 {
		clear(value)
		return nil, errors.New("environment variable value contains NUL")
	}
	if !utf8.Valid(value) {
		clear(value)
		return nil, errors.New("environment variable value must be valid UTF-8")
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
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("ENV recovery output directory is invalid")
	}
	file, err := createEnvironmentRecoveryFile(path)
	if err != nil {
		return fmt.Errorf("create ENV recovery-key file: %w", err)
	}
	created := true
	defer func() {
		if file != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
		if resultErr != nil && created {
			_ = os.Remove(path)
		}
	}()
	payload := make([]byte, 0, len(value)+1)
	payload = append(payload, value...)
	payload = append(payload, '\n')
	defer clear(payload)
	if _, err := file.Write(payload); err != nil {
		return fmt.Errorf("write ENV recovery-key file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync ENV recovery-key file: %w", err)
	}
	if err := file.Close(); err != nil {
		file = nil
		return fmt.Errorf("close ENV recovery-key file: %w", err)
	}
	file = nil
	if err := syncEnvironmentRecoveryDirectory(parent); err != nil {
		return fmt.Errorf("sync ENV recovery-key directory: %w", err)
	}
	created = false
	return nil
}

func safeEnvironmentVariableCommandError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, environmentmanager.ErrVaultPending):
		return errors.New("ENV vault publication is pending; run `pb env vault resume` before retrying")
	case errors.Is(err, environmentmanager.ErrVaultTeamGrantRequired):
		return errors.New("ENV team access needs a key grant; ask a surviving team member to regrant access, then run `pb env grants sync`")
	case errors.Is(err, environmentmanager.ErrVaultLocked):
		return errors.New("ENV vault is locked; run `pb env vault unlock` before changing variables")
	case errors.Is(err, environmentmanager.ErrVaultChanged):
		return errors.New("ENV vault changed; unlock the current vault and retry")
	case errors.Is(err, environmentmanager.ErrVariableNotConfigured):
		return errors.New("environment variable is not configured")
	case errors.Is(err, environmentmanager.ErrAuthorityFork):
		return errors.New("encrypted ENV vault history conflicts with the server; unlock the current vault and retry")
	case errors.Is(err, environmentmanager.ErrIntegrity):
		return errors.New("encrypted ENV vault verification failed; no change was sent")
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		// API mutations sanitize the server response before it reaches this
		// helper. Keep conflict recovery useful, but derive the text only from
		// the stable code so arbitrary server details can never be printed.
		switch apiErr.Code {
		case "version_conflict", "precondition_failed":
			return errors.New("environment variable scope changed; fetch it and retry")
		case "vault_conflict":
			return errors.New("ENV vault changed on the server; unlock the current vault and retry")
		case "rotation_required":
			return errors.New("ENV key rotation is required; use `pb env rotate` for personal ENV or `pb env team rotate <team>` for team ENV before retrying")
		case "key_authorization_required":
			return errors.New("ENV recipient authorization is unavailable; run `pb env grants sync` and retry")
		case "operation_conflict":
			return errors.New("the ENV operation conflicts with an existing operation; run `pb env vault resume` before retrying")
		}
	}
	// A submitted value must not be retained in arbitrary transport or server
	// error text, even if it was escaped or embedded in structured details.
	return errors.New("environment variable update failed")
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
	machines, err := client.ListUserMachines(command.Context())
	if err != nil {
		return friendlyCommandError(err)
	}
	items := environmentVariableScopePickerItems(machines)
	for {
		selection, selectErr := selector.Choose(selector.Options{
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
		if selection.ID != "personal" {
			for _, machine := range machines {
				if machine.ID == selection.ID {
					target = environmentVariableTarget{machineID: machine.ID, machineName: machine.DisplayName}
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
	items := []selector.Item{{ID: "personal", Title: "Personal", Description: "Personal encrypted values; provision explicit host selections", Search: "account personal encrypted"}}
	for _, machine := range environmentVariableMachines(machines) {
		items = append(items, selector.Item{ID: machine.ID, Title: machine.DisplayName, Description: machineStatusSummary(machine), Search: machine.ID + " " + machine.DisplayName})
	}
	return items
}

func runEnvironmentVariableScopeTUI(command *cobra.Command, _ *api.Client, target environmentVariableTarget) error {
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	scope := vaultScopeTarget{kind: "personal", owner: manager.AccountID, machine: target.machineID, label: environmentVariableScopeLabel(target)}
	for {
		metadata, err := readVaultScopeMetadata(command.Context(), manager, scope)
		if err != nil {
			return safeEnvironmentVariableCommandError(err)
		}
		items := []selector.Item{{ID: "set", Title: "Set variable", Description: "Add or replace a variable with hidden input", Search: "add update"}}
		for _, name := range metadata.Names {
			items = append(items, selector.Item{ID: "unset:" + name, Title: name, Description: "configured  ·  revision " + fmt.Sprint(metadata.Revision), Search: name + " remove unset"})
		}
		selection, selectErr := selector.Choose(selector.Options{
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
			name, promptErr := prompt.Text(prompt.TextOptions{
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
			value, valueErr := prompt.Secret(prompt.SecretOptions{Title: "Variable value", Description: "The value is hidden; press Enter to store an empty value", Stdin: os.Stdin, Output: command.ErrOrStderr(), MaxBytes: api.MaximumEnvironmentVariableValueBytes})
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
			_, _ = fmt.Fprintf(command.ErrOrStderr(), "Set %s on %s (encrypted vault scope). Run `pb env host provision` to refresh host selections.\n", name, scope.label)
			continue
		}
		name := strings.TrimPrefix(selection.ID, "unset:")
		confirmed, confirmErr := prompt.Confirm(prompt.ConfirmOptions{Title: "Unset " + name + "?", Description: "New processes on this scope will no longer receive it.", Stdin: os.Stdin, Output: command.ErrOrStderr()})
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
		_, _ = fmt.Fprintf(command.ErrOrStderr(), "Unset %s from %s (encrypted vault scope). Run `pb env host provision` to refresh host selections.\n", name, scope.label)
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

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
