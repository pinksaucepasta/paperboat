package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const cliJSONSchemaVersion = "1.0"

const teamSubscriptionRecovery = "Ask a team owner or admin to manage team billing, or retry with --workspace personal."

type cliJSONError struct {
	Code             string `json:"code"`
	Category         string `json:"category"`
	Message          string `json:"message"`
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
	result := cliJSONError{Code: "operation_failed", Category: "local_io", Message: boundedCLIJSONMessage(commandFailureMessage(err, failure)), StateChanged: "unknown"}
	if result.Message == "" {
		result.Message = "The operation failed."
	}
	owner := soleCommandOwner(err)
	switch typed := owner.(type) {
	case *configComparisonFailure:
		result.Code, result.Category, result.StateChanged = "config_comparison_unavailable", "unavailable_retryable", false
		result.Retryable = true
		result.Recovery = "Refresh pb config status and retry the exact conflict comparison."
	case *api.APIError:
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
		if typed.Status == 403 && typed.Code == "team_subscription_required" {
			result.StateChanged = false
			result.Recovery = teamSubscriptionRecovery
		}
		if typed.Status == 426 && typed.Code == "update_required" {
			result.Category, result.StateChanged = "conflict", false
			result.Message = typed.Error()
			result.Recovery = "Run `pb update` to install a supported official release."
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
		result.Recovery = "Run pb tunnel status <tunnel> before retrying the failed step."
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
	return result
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
